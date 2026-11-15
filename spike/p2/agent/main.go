// Command agent is the A7 spike guest PID 1: it mounts a few filesystems and runs shell lines from a vsock port.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

const port = 1024

func main() {
	fmt.Printf("AGENT pid=%d args=%v\n", os.Getpid(), os.Args)

	for _, m := range []struct{ src, dst, typ string }{
		{"tmpfs", "/run", "tmpfs"},
		{"tmpfs", "/tmp", "tmpfs"},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2"},
		{"shared", "/mnt/shared", "virtiofs"},
	} {
		_ = os.MkdirAll(m.dst, 0o755)
		err := unix.Mount(m.src, m.dst, m.typ, 0, "")
		fmt.Printf("AGENT mount %s %s: %v\n", m.typ, m.dst, err)
	}

	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	check(err)
	check(unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}))
	check(unix.Listen(fd, 8))

	fmt.Printf("AGENT listening on vsock %d\n", port)

	for {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			fmt.Printf("AGENT accept: %v\n", err)

			continue
		}

		go serve(os.NewFile(uintptr(nfd), "vsock-conn"))
	}
}

func serve(c *os.File) {
	defer c.Close()

	sc := bufio.NewScanner(c)
	for sc.Scan() {
		line := sc.Text()
		out, err := exec.Command("/bin/sh", "-c", line).CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
			out = append(out, []byte(err.Error())...)
		}
		fmt.Fprintf(c, "%s\nEXIT %d\n", strings.TrimRight(string(out), "\n"), code)
	}
}

func check(err error) {
	if err != nil {
		fmt.Printf("AGENT fatal: %v\n", err)
		select {}
	}
}
