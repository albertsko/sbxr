package scenario

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
)

// CheckClean reports run state under cacheDir/runs and any process whose command line mentions cacheDir.
// It looks only at this one directory, so parallel tests do not see each other.
func CheckClean(cacheDir string) error {
	runs, err := os.ReadDir(cacheDir + "/runs")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read runs directory: %w", err)
	}

	var errs []error

	for _, r := range runs {
		errs = append(errs, fmt.Errorf("run state remains: %s", r.Name()))
	}

	out, err := exec.CommandContext(context.Background(), "ps", "-A", "-ww", "-o", "pid=,args=").Output()
	if err != nil {
		return fmt.Errorf("list processes: %w", err)
	}

	for line := range strings.Lines(string(out)) {
		if strings.Contains(line, cacheDir) {
			errs = append(errs, fmt.Errorf("process remains: %s", strings.TrimSpace(line)))
		}
	}

	return errors.Join(errs...)
}
