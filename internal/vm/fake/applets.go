package fake

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/albertsko/sbxr/internal/image"
)

// Applets lists the commands of the fake busybox.
var Applets = []string{"cat", "echo", "env", "false", "id", "pwd", "rm", "sh", "sleep", "stat", "stty", "true"}

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
	case "rm":
		return g.rm(args, stdio)
	case "sh":
		return g.sh(args, stdio)
	case "sleep":
		return sleep(args, stdio)
	case "stat":
		return g.stat(args, stdio)
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
	f, ok := stdio.Out.(*os.File)
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

func (g *guest) rm(args []string, stdio Stdio) int {
	status := 0

	for _, name := range args {
		err := g.root.Remove(rel(g.abs(name)))
		if err != nil {
			sayf(stdio.Err, "rm: can't remove '%s': %v\n", name, err)

			status = 1
		}
	}

	return status
}

// stat supports only `stat -c FORMAT FILE` with %u, %g, %a, and %h.
// Owner, group, and mode come from the ownership xattr that the image cache writes.
func (g *guest) stat(args []string, stdio Stdio) int {
	if len(args) != 3 || args[0] != "-c" {
		sayln(stdio.Err, "stat: only 'stat -c FORMAT FILE' is supported")

		return 1
	}

	file := rel(g.abs(args[2]))

	info, err := g.root.Stat(file)
	if err != nil {
		sayf(stdio.Err, "stat: can't stat '%s': %v\n", args[2], err)

		return 1
	}

	buf := make([]byte, 64)

	n, err := unix.Getxattr(filepath.Join(g.root.Name(), file), image.OverrideStatXattr, buf)
	if err != nil {
		sayf(stdio.Err, "stat: can't read the owner of '%s': %v\n", args[2], err)

		return 1
	}

	var uid, gid, mode uint64

	_, err = fmt.Sscanf(string(buf[:n]), "%d:%d:%o", &uid, &gid, &mode)
	if err != nil {
		sayf(stdio.Err, "stat: bad owner of '%s': %v\n", args[2], err)

		return 1
	}

	var nlink uint64
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		nlink = uint64(st.Nlink)
	}

	out := strings.NewReplacer(
		"%u", strconv.FormatUint(uid, 10),
		"%g", strconv.FormatUint(gid, 10),
		"%a", strconv.FormatUint(mode&0o7777, 8),
		"%h", strconv.FormatUint(nlink, 10),
	).Replace(args[1])

	return lines(stdio, out)
}
