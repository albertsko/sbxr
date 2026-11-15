package image

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// ResolveUser returns the uid and gid for a user value in the form user[:group], where each part is a name or a number.
// Names resolve through /etc/passwd and /etc/group inside the image root, and the workload gets no supplementary groups.
// An empty value means root.
func (img *Image) ResolveUser(user string) (uid, gid uint32, err error) {
	if user == "" {
		return 0, 0, nil
	}

	root, err := os.OpenRoot(img.Root)
	if err != nil {
		return 0, 0, fmt.Errorf("open image root: %w", err)
	}
	defer func() { _ = root.Close() }()

	userPart, groupPart, hasGroup := strings.Cut(user, ":")

	uid, gid, err = resolveUserPart(root, userPart)
	if err != nil {
		return 0, 0, fmt.Errorf("user %q: %w", user, err)
	}

	if !hasGroup {
		return uid, gid, nil
	}

	gid, err = resolveGroupPart(root, groupPart)
	if err != nil {
		return 0, 0, fmt.Errorf("user %q: %w", user, err)
	}

	return uid, gid, nil
}

// resolveUserPart returns the uid and the primary gid from /etc/passwd, or gid 0 for a number without an entry.
func resolveUserPart(root *os.Root, user string) (uid, gid uint32, err error) {
	id, numErr := parseID(user)

	entries, err := readDB(root, "etc/passwd")
	if err != nil && (numErr != nil || !errors.Is(err, fs.ErrNotExist)) {
		return 0, 0, err
	}

	for _, e := range entries {
		if len(e) < 4 {
			continue
		}

		field := e[0]
		if numErr == nil {
			field = e[2]
		}

		if field != user {
			continue
		}

		uid, err = parseID(e[2])
		if err != nil {
			return 0, 0, fmt.Errorf("/etc/passwd entry %q: %w", e[0], err)
		}

		gid, err = parseID(e[3])
		if err != nil {
			return 0, 0, fmt.Errorf("/etc/passwd entry %q: %w", e[0], err)
		}

		return uid, gid, nil
	}

	if numErr == nil {
		return id, 0, nil
	}

	return 0, 0, errors.New("no such user in the image /etc/passwd")
}

func resolveGroupPart(root *os.Root, group string) (uint32, error) {
	id, err := parseID(group)
	if err == nil {
		return id, nil
	}

	entries, err := readDB(root, "etc/group")
	if err != nil {
		return 0, err
	}

	for _, e := range entries {
		if len(e) >= 3 && e[0] == group {
			id, err := parseID(e[2])
			if err != nil {
				return 0, fmt.Errorf("/etc/group entry %q: %w", e[0], err)
			}

			return id, nil
		}
	}

	return 0, errors.New("no such group in the image /etc/group")
}

func parseID(s string) (uint32, error) {
	id, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid id %q", s)
	}

	return uint32(id), nil
}

// readDB reads a colon-separated database such as /etc/passwd through root, so symlinks cannot leave the image.
func readDB(root *os.Root, name string) ([][]string, error) {
	data, err := root.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read image /%s: %w", name, err)
	}

	var entries [][]string

	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		entries = append(entries, strings.Split(line, ":"))
	}

	return entries, nil
}
