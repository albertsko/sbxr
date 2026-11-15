package fake

import (
	"bytes"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// With -t, the runner sets the VM terminal raw, so the fake console does what the guest kernel's terminal would do.

// isRaw reports whether f is a terminal without output processing, which is the VM terminal of -t.
func isRaw(f *os.File) bool {
	t, err := unix.IoctlGetTermios(int(f.Fd()), getTermios)

	return err == nil && t.Oflag&unix.OPOST == 0
}

// crlfFile writes to a terminal and turns each newline into CR LF.
type crlfFile struct{ *os.File }

func (f crlfFile) Write(p []byte) (int, error) {
	_, err := f.File.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n")))
	if err != nil {
		return 0, err //nolint:wrapcheck // This is a plain write.
	}

	return len(p), nil
}

// console reads keys from the terminal f, echoes them to echo, and returns the guest stdin.
// Ctrl-C ends the workload with exit code 130, as SIGINT would.
// Keys are handled at once and queued for the workload, so a workload that does not read stdin never delays Ctrl-C.
func console(f *os.File, echo io.Writer) io.Reader {
	q := &inputQueue{mu: sync.Mutex{}, ready: nil, buf: bytes.Buffer{}, err: nil}
	q.ready = sync.NewCond(&q.mu)

	go func() {
		buf := make([]byte, 1024)

		for {
			n, err := f.Read(buf)
			keys := bytes.ReplaceAll(buf[:n], []byte("\r"), []byte("\n"))

			if bytes.IndexByte(keys, 0x03) >= 0 {
				os.Exit(130)
			}

			_, _ = echo.Write(keys) // A failed echo does not stop the input.

			q.put(keys, err)

			if err != nil {
				return
			}
		}
	}()

	return q
}

// inputQueue is an unbounded buffer between the console and the workload.
type inputQueue struct {
	mu    sync.Mutex
	ready *sync.Cond
	buf   bytes.Buffer
	err   error
}

func (q *inputQueue) put(p []byte, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.buf.Write(p)
	q.err = err
	q.ready.Broadcast()
}

func (q *inputQueue) Read(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for q.buf.Len() == 0 && q.err == nil {
		q.ready.Wait()
	}

	if q.buf.Len() > 0 {
		return q.buf.Read(p) //nolint:wrapcheck // bytes.Buffer returns only io.EOF here, which cannot happen.
	}

	return 0, q.err
}
