// Command guest is a static helper that runs inside the VM: an HTTP server and a TCP reachability probe.
package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: guest serve ADDR | guest dial ADDR")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "hello from guest, remote=%s\n", r.RemoteAddr)
		})
		fmt.Println(http.ListenAndServe(os.Args[2], nil))
	case "dial":
		dial(os.Args[2])
	}
}

func dial(addr string) {
	conn, err := net.DialTimeout("tcp", addr, 4*time.Second)
	if err != nil {
		fmt.Printf("PROBE %s BLOCKED (%v)\n", addr, err)
		return
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(conn, "GET / HTTP/1.0\r\n\r\n")
	b, _ := io.ReadAll(io.LimitReader(conn, 200))
	fmt.Printf("PROBE %s REACHABLE (%d bytes: %q)\n", addr, len(b), firstLine(b))
}

func firstLine(b []byte) string {
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	return string(b)
}
