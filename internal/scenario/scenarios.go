package scenario

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// All returns every scenario.
func All() []Scenario {
	return []Scenario{
		{Name: "echo", Check: expect(Workload{Image: Busybox, Args: []string{"echo", "hi"}}, 0, OK, stdout("hi\n"))},
		{Name: "exit code", Check: expect(Workload{Image: Busybox, Args: []string{"sh", "-c", "exit 3"}}, 3, OK)},
		{Name: "relative workdir", Check: expect(Workload{Image: Busybox, Args: []string{"true"}, Workdir: "app"}, 125, InvalidSpec)},
		{Name: "control character in argument", Check: expect(Workload{Image: Busybox, Args: []string{"echo", "a\x1bb"}}, 125, InvalidSpec)},
		{Name: "reserved env name", Check: expect(Workload{Image: Busybox, Args: []string{"true"}, Env: []string{"KRUN_INIT=/bin/sh"}}, 125, InvalidSpec)},
		{Name: "missing command", Check: expect(Workload{Image: Busybox, Args: []string{"nope"}}, 127, NotFound)},
		{Name: "not executable", Check: expect(Workload{Image: Busybox, Args: []string{"/etc/passwd"}}, 126, NotExecutable)},
		{Name: "missing image", Check: expect(Workload{Image: MissingImage, Args: []string{"true"}}, 125, Image)},
		{
			Name: "arguments reach the workload unchanged",
			Check: expect(Workload{Image: Busybox, Args: []string{"echo", `"q'`, "a\tb", "line\nnext", "é😀", "uid", "Env", "args"}}, 0, OK,
				stdout("\"q' a\tb line\nnext é😀 uid Env args\n")),
		},
		{
			Name:  "image config applies",
			Check: expect(Workload{Image: Configured}, 0, OK, stdout("image image\n1000\n1000\n/tmp\n")),
		},
		{
			Name:  "arguments replace the image command",
			Check: expect(Workload{Image: Configured, Args: []string{"echo x"}}, 0, OK, stdout("x\n")),
		},
		{
			Name:  "env flags override the image env",
			Check: expect(Workload{Image: Configured, Env: []string{"B=flag", "C=flag"}, Args: []string{"echo $A $B $C"}}, 0, OK, stdout("image flag flag\n")),
		},
		{
			Name:  "user and workdir flags override the image",
			Check: expect(Workload{Image: Configured, User: "0", Workdir: "/root", Args: []string{"id -u; pwd"}}, 0, OK, stdout("0\n/root\n")),
		},
		{
			Name: "guest env holds only image env, flags, and defaults",
			Check: expect(Workload{Image: Busybox, Args: []string{"env"}, Env: []string{"A=1", "TERM=xterm"}}, 0, OK,
				envExactly("PATH=/bin", "A=1", "TERM=xterm", "HOME=/")),
		},
		{
			Name:  "user flag wins over arguments that look like config keys",
			Check: expect(Workload{Image: Busybox, User: "1000", Args: []string{"sh", "-c", "id -u", "uid", "0"}}, 0, OK, stdout("1000\n")),
		},
		{Name: "workdir flag", Check: expect(Workload{Image: Busybox, Workdir: "/tmp", Args: []string{"pwd"}}, 0, OK, stdout("/tmp\n"))},
		{
			Name:  "stdin with interactive",
			Needs: []Capability{Stdin},
			Check: expect(Workload{Image: Busybox, Args: []string{"cat"}, Interactive: true, Stdin: "data\n"}, 0, OK, stdout("data\n")),
		},
		{
			Name:  "stdin is empty without interactive",
			Needs: []Capability{Stdin},
			Check: expect(Workload{Image: Busybox, Args: []string{"cat"}, Stdin: "data\n"}, 0, OK, stdout("")),
		},
		{
			Name:  "tty",
			Needs: []Capability{TTY},
			Check: expect(Workload{Image: Busybox, Args: []string{"echo", "hi"}, TTY: true}, 0, OK, stdoutContains("hi")),
		},
		{
			Name:  "interrupt",
			Needs: []Capability{SignalExit},
			Check: expect(Workload{Image: Busybox, Args: []string{"sh", "-c", "echo ready; sleep 30"}, InterruptAfter: "ready"}, 130, Interrupted),
		},
		{
			Name:    "no network without --net",
			Needs:   []Capability{NetOff},
			Pending: "needs run: S5",
			Check:   expect(Workload{Image: Busybox, Args: []string{"wget", "-T", "2", "-qO-", "http://1.1.1.1"}}, 1, OK),
		},
	}
}

type resultCheck func(Result) error

// expect runs w and checks the exit code, the error class, and any extra checks.
func expect(w Workload, code int, class ErrorClass, checks ...resultCheck) func(context.Context, Target) error {
	return func(ctx context.Context, t Target) error {
		res, err := t.Run(ctx, w)
		if err != nil {
			return fmt.Errorf("run: %w", err)
		}

		var errs []error

		if res.ExitCode != code || res.Class != class {
			errs = append(errs, fmt.Errorf("exit code %d, class %s; want %d, %s", res.ExitCode, res.Class, code, class))
		}

		for _, check := range checks {
			errs = append(errs, check(res))
		}

		if err := errors.Join(errs...); err != nil {
			return fmt.Errorf("%w (stdout %q)", err, res.Stdout)
		}

		return nil
	}
}

func stdout(want string) resultCheck {
	return func(r Result) error {
		if r.Stdout != want {
			return fmt.Errorf("stdout %q, want %q", r.Stdout, want)
		}

		return nil
	}
}

func stdoutContains(want string) resultCheck {
	return func(r Result) error {
		if !strings.Contains(r.Stdout, want) {
			return fmt.Errorf("stdout %q, want it to contain %q", r.Stdout, want)
		}

		return nil
	}
}

// envExactly checks that stdout lists exactly these env entries, in any order.
func envExactly(want ...string) resultCheck {
	return func(r Result) error {
		got := strings.Fields(r.Stdout)
		if len(got) != len(want) {
			return fmt.Errorf("env %q, want exactly %q", got, want)
		}

		for _, kv := range want {
			if !strings.Contains("\n"+r.Stdout, "\n"+kv+"\n") {
				return fmt.Errorf("env %q, want exactly %q", got, want)
			}
		}

		return nil
	}
}
