package image

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// hardlinksFile lists the hardlink groups of a cached root, because an APFS directory clone does not keep hardlinks.
const hardlinksFile = "hardlinks.json"

// hardlinkGroups returns the paths of each set of regular files under root that share an inode.
func hardlinkGroups(root string) ([][]string, error) {
	byInode := map[uint64]int{}

	var groups [][]string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err //nolint:wrapcheck // hardlinkGroups wraps the error.
		}

		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Nlink < 2 {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err //nolint:wrapcheck // hardlinkGroups wraps the error.
		}

		i, ok := byInode[st.Ino]
		if !ok {
			i = len(groups)
			groups = append(groups, nil)
			byInode[st.Ino] = i
		}

		groups[i] = append(groups[i], rel)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("find hardlinks: %w", err)
	}

	return groups, nil
}

func writeHardlinks(imageDir string, groups [][]string) error {
	data, err := json.Marshal(groups)
	if err != nil {
		return fmt.Errorf("write hardlinks: %w", err)
	}

	err = os.WriteFile(filepath.Join(imageDir, hardlinksFile), data, 0o600)
	if err != nil {
		return fmt.Errorf("write hardlinks: %w", err)
	}

	return nil
}

// relink turns each hardlink group under dst back into links of its first path.
func (img *Image) relink(dst string) error {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(img.Root), hardlinksFile))
	if err != nil {
		return fmt.Errorf("read hardlinks: %w", err)
	}

	var groups [][]string

	err = json.Unmarshal(data, &groups)
	if err != nil {
		return fmt.Errorf("read hardlinks: %w", err)
	}

	root, err := os.OpenRoot(dst)
	if err != nil {
		return fmt.Errorf("open run root: %w", err)
	}
	defer func() { _ = root.Close() }()

	for _, g := range groups {
		for _, name := range g[1:] {
			err = root.Remove(name)
			if err == nil {
				err = root.Link(g[0], name)
			}

			if err != nil {
				return fmt.Errorf("restore hardlink: %w", err)
			}
		}
	}

	return nil
}
