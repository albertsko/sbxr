// Command guest is the workload of the libkrun VM tests.
//
// Usage:
//
//	guest echo WORD...
//	guest exit CODE
//	guest net LISTEN_ADDR DIAL_ADDR...
//	guest report ARG...
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	switch os.Args[1] {
	case "echo":
		fmt.Println(strings.Join(os.Args[2:], " "))
	case "exit":
		code, _ := strconv.Atoi(os.Args[2])
		os.Exit(code)
	case "net":
		probe(os.Args[2], os.Args[3:])
	case "report":
		report(os.Args[2:])
	}
}

// report prints the arguments, env, ids, and working directory as one JSON object.
func report(args []string) {
	cwd, _ := os.Getwd()

	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"args": args,
		"env":  os.Environ(),
		"uid":  os.Getuid(),
		"gid":  os.Getgid(),
		"cwd":  cwd,
	})
}

// probe listens so the host can try to reach the guest, then tries to reach the host.
func probe(listenAddr string, dialAddrs []string) {
	ln, err := net.Listen("tcp4", listenAddr)
	if err != nil {
		fmt.Println("listen failed:", err)
		os.Exit(1)
	}

	fmt.Println("listening")

	for _, addr := range dialAddrs {
		conn, err := net.DialTimeout("tcp4", addr, 2*time.Second)
		if err != nil {
			fmt.Println("dial failed", addr)

			continue
		}

		conn.Close()
		fmt.Println("dial ok", addr)
	}

	// Give the host time to dial the listener.
	time.Sleep(3 * time.Second)
	ln.Close()
}
