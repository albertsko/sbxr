package cli

import (
	"fmt"
	"os"
	"runtime"

	charmlog "charm.land/log/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/albertsko/sbxr/internal/run"
	"github.com/albertsko/sbxr/internal/spec"
)

func (a *app) runCommand() *cobra.Command {
	var (
		in         spec.Input
		envFiles   []string
		entrypoint string
		quiet      bool
	)

	cmd := &cobra.Command{
		Use:   "run [flags] IMAGE [CMD [ARG...]]",
		Short: "Run a workload in a new VM and remove the VM after exit",
		Example: `  sbxr run alpine echo hi
  sbxr run -it alpine sh
  sbxr run --net -p 8080:80 nginx`,
		Args: cobra.ArbitraryArgs,
	}

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in.Image, in.Args = splitArgs(args)

		if cmd.Flags().Changed("entrypoint") {
			in.Entrypoint = &entrypoint
		}

		code, err := a.run(cmd, in, envFiles, quiet)
		if err != nil {
			return failure(cmd, err)
		}

		if code != 0 {
			return &exitError{code: code, err: nil}
		}

		return nil
	}

	flags := cmd.Flags()
	flags.SetInterspersed(false)
	flags.StringVar(&in.CPUs, "cpus", "", fmt.Sprintf("Number of vCPUs (default %d, at most the host CPU count)", spec.DefaultCPUs))
	flags.StringVar(&in.Memory, "memory", "", "Guest RAM, for example 512M or 2G (default "+spec.DefaultMemory+")")
	flags.StringArrayVarP(&in.Env, "env", "e", nil, "Set an env var KEY=VALUE")
	flags.StringArrayVar(&envFiles, "env-file", nil, "Read env vars from a file")
	flags.StringArrayVarP(&in.Volumes, "volume", "v", nil, "Share a host directory HOST:GUEST[:ro]")
	flags.BoolVar(&in.Net, "net", false, "Allow outbound IPv4 to public addresses")
	flags.BoolVar(&in.AllowLAN, "allow-lan", false, "Also allow LAN addresses (requires --net)")
	flags.StringArrayVarP(&in.Publish, "publish", "p", nil, "Publish a guest TCP port [IP:]HOST:GUEST (requires --net)")
	flags.StringVarP(&in.Workdir, "workdir", "w", "", "Working directory in the guest, absolute")
	flags.StringVarP(&in.User, "user", "u", "", "User in the guest, UID[:GID]")
	flags.StringVar(&entrypoint, "entrypoint", "", "Replace the image entrypoint")
	flags.StringVar(&in.Pull, "pull", string(spec.PullMissing), "When to download the image: missing, always, or never")
	flags.BoolVarP(&in.Interactive, "interactive", "i", false, "Keep stdin open")
	flags.BoolVarP(&in.TTY, "tty", "t", false, "Allocate a terminal")
	// Progress output comes with image pull in S4. The flag exists now, so that scripts can pass it.
	flags.BoolVarP(&quiet, "quiet", "q", false, "Hide progress output")

	return cmd
}

// splitArgs returns the image and the workload arguments. A "--" right after the image is dropped.
func splitArgs(args []string) (string, []string) {
	if len(args) == 0 {
		return "", nil
	}

	image, rest := args[0], args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}

	return image, rest
}

func (a *app) run(cmd *cobra.Command, in spec.Input, envFiles []string, quiet bool) (int, error) {
	for _, path := range envFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, &spec.Error{Source: "--env-file", Value: path, Problem: "cannot read the file", Fix: err.Error()}
		}

		in.EnvFiles = append(in.EnvFiles, spec.EnvFile{Path: path, Data: data})
	}

	s, err := spec.Parse(in, runtime.NumCPU())
	if err != nil {
		return 0, err //nolint:wrapcheck // spec errors are complete user messages.
	}

	dir, err := cacheDir()
	if err != nil {
		return 0, err
	}

	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("find the sbxr binary: %w", err)
	}

	// The workload writes to the real stdout, so restore it while the run lasts.
	a.stdout.restore()
	defer a.stdout.hide()

	r := &run.Runner{
		Images:     nil,
		Checks:     a.backend.Checks(dir),
		Isolate:    nil,
		Exe:        exe,
		VMEnv:      a.backend.VMEnv,
		VMLogLevel: vmLogLevel(a.level),
		RunsDir:    run.RunsDir(dir),
		Log:        a.log,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
	}

	if a.backend.Isolate != nil {
		r.Isolate = func(stdin, stdout, stderr *os.File) (int, error) {
			return a.backend.Isolate(a.args, stdin, stdout, stderr)
		}
	} else {
		store, err := a.backend.Images(dir)
		if err != nil {
			return 0, err
		}

		// Progress shows only on a terminal, so that logs stay readable.
		if !quiet && term.IsTerminal(int(os.Stderr.Fd())) {
			store.Progress = pullProgress(os.Stderr, s.Image)
		}

		r.Images = store
	}

	return r.Run(cmd.Context(), s) //nolint:wrapcheck // Runner errors are complete user messages.
}

// vmLogLevel shows libkrun info logs only at the debug level, so that a normal run prints nothing of its own.
func vmLogLevel(level charmlog.Level) uint32 {
	if level <= charmlog.DebugLevel {
		return 3
	}

	return 0
}
