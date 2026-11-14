// Command guest is a static helper for the spike.
// In the VM it runs raw-frame probes and a connection holder. On the host it runs the TCP and UDP listeners that the probes target.
package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: guest serve ADDR | dial ADDR | listen SPEC... | probe SRCIP SPEC... | hold N ADDR SECONDS")
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
	case "listen":
		listen(os.Args[2:])
	case "probe":
		probe(os.Args[2], os.Args[3:])
	case "hold":
		n, _ := strconv.Atoi(os.Args[2])
		secs, _ := strconv.Atoi(os.Args[4])
		hold(n, os.Args[3], time.Duration(secs)*time.Second)
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

// listen starts host listeners. SPEC is tcp:IP:PORT or udp:IP:PORT. TCP sends a banner and keeps the connection open until EOF.
func listen(specs []string) {
	for _, spec := range specs {
		proto, addr, _ := strings.Cut(spec, ":")
		switch proto {
		case "tcp":
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				fmt.Printf("listen %s: %v\n", spec, err)
				os.Exit(1)
			}
			go acceptLoop(ln, addr)
		case "udp":
			pc, err := net.ListenPacket("udp", addr)
			if err != nil {
				fmt.Printf("listen %s: %v\n", spec, err)
				os.Exit(1)
			}
			go udpEcho(pc, addr)
		}
		fmt.Printf("listening %s\n", spec)
	}
	select {}
}

func acceptLoop(ln net.Listener, addr string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			fmt.Printf("accept %s: %v\n", addr, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if os.Getenv("LOG_ACCEPT") == "1" {
			fmt.Printf("HOSTSVC accept tcp %s from %s\n", addr, c.RemoteAddr())
		}
		go func() {
			defer c.Close()
			fmt.Fprintf(c, "HOSTSVC tcp %s\n", addr)
			_, _ = io.Copy(io.Discard, c)
		}()
	}
}

func udpEcho(pc net.PacketConn, addr string) {
	buf := make([]byte, 2048)
	for {
		_, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		fmt.Printf("HOSTSVC recv udp %s from %s\n", addr, from)
		_, _ = pc.WriteTo([]byte("HOSTSVC udp "+addr+"\n"), from)
	}
}

// hold opens n TCP connections to addr with the guest kernel, reads each banner, and keeps them open.
func hold(n int, addr string, d time.Duration) {
	lim := syscall.Rlimit{Cur: uint64(n + 100), Max: uint64(n + 100)}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		fmt.Printf("HOLD setrlimit: %v\n", err)
	}

	start := time.Now()
	conns := make([]net.Conn, 0, n)
	var firstErr error
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			firstErr = fmt.Errorf("conn %d: %w", i, err)
			break
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
			firstErr = fmt.Errorf("conn %d banner: %w", i, err)
			c.Close()
			break
		}
		conns = append(conns, c)
	}
	fmt.Printf("HOLD established=%d/%d in %v err=%v\n", len(conns), n, time.Since(start).Round(time.Millisecond), firstErr)
	_ = os.WriteFile("/gvnet-held", []byte(strconv.Itoa(len(conns))), 0o644)
	time.Sleep(d)
	for _, c := range conns {
		c.Close()
	}
	fmt.Printf("HOLD closed\n")
}

func firstLine(b []byte) string {
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	return string(b)
}
