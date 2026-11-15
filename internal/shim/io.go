//go:build linux

package shim

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/albertsko/sbxr/internal/podvm"
)

// procIO connects one process's containerd FIFOs to its stdio streams in the guest.
type procIO struct {
	stdin  *net.UnixConn
	copies sync.WaitGroup
	files  []io.Closer
}

// connectIO opens the FIFOs that containerd created and one stream to the guest for each.
// FIFOs open read-write, so opening never blocks and the stdin copy ends only through CloseIO.
func connectIO(streamSock, container, exec, stdin, stdout, stderr string, terminal bool) (*procIO, error) {
	p := &procIO{stdin: nil, copies: sync.WaitGroup{}, files: nil}

	if terminal {
		stderr = "" // A terminal carries both output streams.
	}

	for _, s := range []struct{ kind, fifo string }{{podvm.Stdin, stdin}, {podvm.Stdout, stdout}, {podvm.Stderr, stderr}} {
		if s.fifo == "" {
			continue
		}

		if err := p.connect(streamSock, podvm.Header{Kind: s.kind, Container: container, Exec: exec, Port: 0}, s.fifo); err != nil {
			p.close()

			return nil, err
		}
	}

	return p, nil
}

func (p *procIO) connect(streamSock string, h podvm.Header, fifo string) error {
	f, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", h.Kind, err)
	}

	conn, err := dialStream(streamSock, h)
	if err != nil {
		_ = f.Close()

		return err
	}

	p.files = append(p.files, f, conn)

	if h.Kind == podvm.Stdin {
		p.stdin = conn

		go func() { _, _ = io.Copy(conn, f) }()

		return nil
	}

	p.copies.Go(func() { _, _ = io.Copy(f, conn) })

	return nil
}

// dialStream opens a stream to the guest and sends its header.
func dialStream(streamSock string, h podvm.Header) (*net.UnixConn, error) {
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: streamSock, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("open %s stream: %w", h.Kind, err)
	}

	w := bufio.NewWriter(conn)
	_, err = w.WriteString(h.String() + "\n")

	if err = errors.Join(err, w.Flush()); err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("open %s stream: %w", h.Kind, err)
	}

	return conn, nil
}

// closeStdin ends the process's stdin.
func (p *procIO) closeStdin() {
	if p.stdin != nil {
		_ = p.stdin.CloseWrite()
	}
}

// drain waits until the output streams end, which happens when the process and its children closed them, or until timeout.
func (p *procIO) drain(timeout time.Duration) {
	done := make(chan struct{})

	go func() {
		p.copies.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (p *procIO) close() {
	for _, c := range p.files {
		_ = c.Close()
	}
}
