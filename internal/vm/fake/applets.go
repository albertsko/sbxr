package fake

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// Applets lists the commands of the fake busybox.
var Applets = []string{"cat", "echo", "env", "false", "id", "pwd", "sh", "sleep", "stty", "true"}

// run runs one applet and returns its exit code.
func (g *guest) run(applet string, args []string, stdio Stdio) int {
	if applet == "busybox" && len(args) > 0 {
		applet, args = args[0], args[1:]
	}

	switch applet {
	case "cat":
		return g.cat(args, stdio)
	case "echo":
		return echo(args, stdio)
	case "env":
		return lines(stdio, g.cfg.Env...)
	case "false":
		return 1
	case "id":
		return g.id(args, stdio)
	case "pwd":
		return lines(stdio, g.cwd)
	case "sh":
		return g.sh(args, stdio)
	case "sleep":
		return sleep(args, stdio)
	case "stty":
		return stty(args, stdio)
	case "true":
		return 0
	default:
		sayf(stdio.Err, "%s: applet not found\n", applet)

		return 127
	}
}

func echo(args []string, stdio Stdio) int {
	end := "\n"
	if len(args) > 0 && args[0] == "-n" {
		args, end = args[1:], ""
	}

	sayf(stdio.Out, "%s", strings.Join(args, " ")+end)

	return 0
}

func lines(stdio Stdio, values ...string) int {
	for _, v := range values {
		sayln(stdio.Out, v)
	}

	return 0
}

func (g *guest) cat(args []string, stdio Stdio) int {
	if len(args) == 0 {
		_, err := io.Copy(stdio.Out, stdio.In)
		if err != nil {
			sayf(stdio.Err, "cat: %v\n", err)

			return 1
		}

		return 0
	}

	status := 0

	for _, name := range args {
		data, err := g.root.ReadFile(rel(g.abs(name)))
		if err != nil {
			sayf(stdio.Err, "cat: can't open '%s': No such file or directory\n", name)

			status = 1

			continue
		}

		stdio.Out.Write(data) //nolint:errcheck // Like cat, a failed write to the console is ignored.
	}

	return status
}

func (g *guest) id(args []string, stdio Stdio) int {
	switch {
	case len(args) == 0:
		return lines(stdio, fmt.Sprintf("uid=%s gid=%s", g.cfg.UID, g.cfg.GID))
	case args[0] == "-u":
		return lines(stdio, g.cfg.UID)
	case args[0] == "-g":
		return lines(stdio, g.cfg.GID)
	default:
		sayf(stdio.Err, "id: unrecognized option '%s'\n", args[0])

		return 1
	}
}

func sleep(args []string, stdio Stdio) int {
	if len(args) != 1 {
		sayln(stdio.Err, "sleep: usage: sleep SECONDS")

		return 1
	}

	seconds, err := strconv.ParseFloat(args[0], 64)
	if err != nil {
		sayf(stdio.Err, "sleep: invalid number '%s'\n", args[0])

		return 1
	}

	time.Sleep(time.Duration(seconds * float64(time.Second)))

	return 0
}

// stty supports only `stty size`. It prints the rows and columns of the console, which is stdout here,
// because the fake console replaces stdin with a pipe.
func stty(args []string, stdio Stdio) int {
	f, ok := stdio.Out.(interface{ Fd() uintptr })
	if len(args) != 1 || args[0] != "size" || !ok {
		sayln(stdio.Err, "stty: only 'stty size' is supported")

		return 1
	}

	cols, rows, err := term.GetSize(int(f.Fd()))
	if err != nil {
		sayln(stdio.Err, "stty: standard input: Not a tty")

		return 1
	}

	return lines(stdio, fmt.Sprintf("%d %d", rows, cols))
}
