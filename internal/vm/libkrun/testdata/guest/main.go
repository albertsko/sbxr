// Command guest is the workload of the libkrun VM tests.
//
// Usage:
//
//	guest echo WORD...
//	guest exit CODE
//	guest net LISTEN_ADDR DIAL_ADDR...
package main

import (
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
	}
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
