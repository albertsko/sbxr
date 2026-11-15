// Package fake provides the backends of the fake target, which runs without a hypervisor.
//
// Hypervisor interprets a small busybox subset in the VM process instead of booting a VM.
// Images serves image roots from a local directory. Checks are preflight checks with chosen results.
// Only binaries built with the sbxrfake build tag use this package.
package fake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"golang.org/x/term"

	"github.com/albertsko/sbxr/internal/krunconfig"
	"github.com/albertsko/sbxr/internal/vm"
)

// Marker starts every fake busybox binary in an image root.
const Marker = "#!sbxr-fake-busybox\n"

// Hypervisor runs the workload with the fake guest init instead of a VM.
type Hypervisor struct{}

// Start runs the fake guest init in root and exits the process with the workload's exit code.
func (Hypervisor) Start(m vm.Machine) error {
	root, err := os.OpenRoot(m.Root)
	if err != nil {
		return fmt.Errorf("open root: %w", err)
	}

	var in io.Reader = os.Stdin
	if term.IsTerminal(int(os.Stdin.Fd())) {
		in = console(os.Stdin)
	}

	os.Exit(Boot(root, Stdio{In: in, Out: os.Stdout, Err: os.Stderr}))

	return nil
}

// Stdio holds the guest console streams.
type Stdio struct {
	In       io.Reader
	Out, Err io.Writer
}

// config is the guest init view of /.krun_config.json.
type config struct {
	UID        string   `json:"uid"`
	GID        string   `json:"gid"`
	WorkingDir string   `json:"WorkingDir"`
	Env        []string `json:"Env"`
	Args       []string `json:"args"`
}

// Boot behaves like the libkrun guest init: it reads the config, changes to the working directory,
// and runs the command through the image PATH. It returns the exit code.
func Boot(root *os.Root, stdio Stdio) int {
	data, err := root.ReadFile(krunconfig.FileName)
	if err != nil {
		sayf(stdio.Out, "fake init: read config: %v\n", err)

		return vm.ExitSetup
	}

	var cfg config

	err = json.Unmarshal(data, &cfg)
	if err != nil || len(cfg.Args) == 0 {
		sayf(stdio.Out, "fake init: invalid config: %v\n", err)

		return vm.ExitSetup
	}

	g := &guest{root: root, cfg: cfg, cwd: "/", status: 0}

	// Like the real init, a failed chdir keeps / as the working directory.
	if info, err := root.Stat(rel(cfg.WorkingDir)); err == nil && info.IsDir() {
		g.cwd = path.Clean(cfg.WorkingDir)
	}

	applet, err := g.resolve(cfg.Args[0])
	if err != nil {
		// The real init prints this line to stdout.
		sayf(stdio.Out, "Couldn't execute '%s' inside the vm: %s\n", cfg.Args[0], err)

		return exitCode(err)
	}

	return g.run(applet, cfg.Args[1:], stdio)
}

// guest is the state of the fake guest.
type guest struct {
	root   *os.Root
	cfg    config
	cwd    string
	status int
}

// Errors from command resolution, with libc strerror text.
var (
	errNotFound   = errors.New("No such file or directory") //nolint:staticcheck // Matches libc strerror.
	errPermission = errors.New("Permission denied")         //nolint:staticcheck // Matches libc strerror.
	errFormat     = errors.New("Exec format error")         //nolint:staticcheck // Matches libc strerror.
)

func exitCode(err error) int {
	if errors.Is(err, errNotFound) {
		return 127
	}

	return 126
}

// resolve finds name like execvp: through PATH for a bare name, else as a path.
// It returns the applet that the fake busybox runs.
func (g *guest) resolve(name string) (string, error) {
	if strings.Contains(name, "/") {
		return g.applet(name, g.abs(name))
	}

	for dir := range strings.SplitSeq(g.getenv("PATH"), ":") {
		if dir == "" {
			continue
		}

		applet, err := g.applet(name, path.Join(g.abs(dir), name))
		if !errors.Is(err, errNotFound) {
			return applet, err
		}
	}

	return "", errNotFound
}

func (g *guest) applet(name, file string) (string, error) {
	info, err := g.root.Stat(rel(file))
	if errors.Is(err, fs.ErrNotExist) {
		return "", errNotFound
	}

	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return "", errPermission
	}

	data, err := g.root.ReadFile(rel(file))
	if err != nil || !strings.HasPrefix(string(data), Marker) {
		return "", errFormat
	}

	return path.Base(name), nil
}

func (g *guest) getenv(name string) string {
	for _, kv := range g.cfg.Env {
		if k, v, _ := strings.Cut(kv, "="); k == name {
			return v
		}
	}

	return ""
}

func (g *guest) abs(p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}

	return path.Join(g.cwd, p)
}

// rel turns an absolute guest path into a path relative to the root.
func rel(p string) string {
	p = strings.TrimLeft(path.Clean("/"+p), "/")
	if p == "" {
		return "."
	}

	return p
}

// say writes guest console output. Like a real program, the guest ignores a failed console write.
func sayf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func sayln(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}
