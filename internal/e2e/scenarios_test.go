package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// allScenarios returns every scenario.
func allScenarios() []scenario {
	return []scenario{
		{Name: "echo", Check: expect(workload{Image: Busybox, Args: []string{"echo", "hi"}}, 0, OK, stdout("hi\n"))},
		{Name: "exit code", Check: expect(workload{Image: Busybox, Args: []string{"sh", "-c", "exit 3"}}, 3, OK)},
		{Name: "relative workdir", Needs: []capability{SpecValidation}, Check: expect(workload{Image: Busybox, Args: []string{"true"}, Workdir: "app"}, 125, InvalidSpec)},
		{Name: "control character in argument", Needs: []capability{SpecValidation}, Check: expect(workload{Image: Busybox, Args: []string{"echo", "a\x1bb"}}, 125, InvalidSpec)},
		{Name: "reserved env name", Needs: []capability{SpecValidation}, Check: expect(workload{Image: Busybox, Args: []string{"true"}, Env: []string{"KRUN_INIT=/bin/sh"}}, 125, InvalidSpec)},
		{Name: "missing command", Check: expect(workload{Image: Busybox, Args: []string{"nope"}}, 127, NotFound)},
		{Name: "not executable", Check: expect(workload{Image: Busybox, Args: []string{"/etc/passwd"}}, 126, NotExecutable)},
		{Name: "missing image", Check: expect(workload{Image: MissingImage, Args: []string{"true"}}, 125, Image)},
		{
			Name: "arguments reach the workload unchanged",
			Check: expect(workload{Image: Busybox, Args: []string{"echo", `"q'`, "a\tb", "line\nnext", "é😀", "uid", "Env", "args"}}, 0, OK,
				stdout("\"q' a\tb line\nnext é😀 uid Env args\n")),
		},
		{
			Name:  "image config applies",
			Needs: []capability{ExactConfig},
			Check: expect(workload{Image: Configured}, 0, OK, stdout("image image\n1000\n1000\n/tmp\n")),
		},
		{
			Name:  "arguments replace the image command",
			Check: expect(workload{Image: Configured, Args: []string{"echo x"}}, 0, OK, stdout("x\n")),
		},
		{
			Name:  "env flags override the image env",
			Check: expect(workload{Image: Configured, Env: []string{"B=flag", "C=flag"}, Args: []string{"echo $A $B $C"}}, 0, OK, stdout("image flag flag\n")),
		},
		{
			Name:  "user and workdir flags override the image",
			Check: expect(workload{Image: Configured, User: "0", Workdir: "/root", Args: []string{"id -u; pwd"}}, 0, OK, stdout("0\n/root\n")),
		},
		{
			Name:  "guest env holds only image env, flags, and defaults",
			Needs: []capability{ExactConfig},
			Check: expect(workload{Image: Busybox, Args: []string{"env"}, Env: []string{"A=1", "TERM=xterm"}}, 0, OK,
				envExactly("PATH=/bin", "A=1", "TERM=xterm", "HOME=/")),
		},
		{
			Name:  "user flag wins over arguments that look like config keys",
			Check: expect(workload{Image: Busybox, User: "1000", Args: []string{"sh", "-c", "id -u", "uid", "0"}}, 0, OK, stdout("1000\n")),
		},
		{
			Name:  "image user name resolves in the image",
			Check: expect(workload{Image: NamedUser, Args: []string{"sh", "-c", "id -u; id -g"}}, 0, OK, stdout("65534\n65534\n")),
		},
		{
			Name:  "files keep owner, mode, and hardlinks from the image",
			Check: expect(workload{Image: Busybox, Args: []string{"stat", "-c", "%u:%g %a %h", SetuidFile}}, 0, OK, stdout("0:42 4755 2\n")),
		},
		{
			Name:  "image config file does not change what runs",
			Check: expect(workload{Image: Busybox, Args: []string{"echo", "ok"}}, 0, OK, stdout("ok\n")),
		},
		{Name: "changes do not reach the next run", Check: changesDoNotPersist},
		{Name: "workdir flag", Check: expect(workload{Image: Busybox, Workdir: "/tmp", Args: []string{"pwd"}}, 0, OK, stdout("/tmp\n"))},
		{
			Name:  "stdin with interactive",
			Needs: []capability{Stdin},
			Check: expect(workload{Image: Busybox, Args: []string{"cat"}, Interactive: true, Stdin: "data\n"}, 0, OK, stdout("data\n")),
		},
		{
			Name:  "stdin is empty without interactive",
			Needs: []capability{Stdin},
			Check: expect(workload{Image: Busybox, Args: []string{"cat"}, Stdin: "data\n"}, 0, OK, stdout("")),
		},
		{
			Name:  "tty",
			Needs: []capability{TTY},
			Check: expect(workload{Image: Busybox, Args: []string{"echo", "hi"}, TTY: true}, 0, OK, stdoutContains("hi")),
		},
		{
			Name:  "interrupt",
			Needs: []capability{SignalExit},
			Check: expect(workload{Image: Busybox, Args: []string{"sh", "-c", "echo ready; sleep 30"}, InterruptAfter: "ready"}, 130, Interrupted),
		},
		{
			Name:  "terminate",
			Needs: []capability{SignalExit},
			Check: expect(workload{Image: Busybox, Args: []string{"sh", "-c", "echo ready; sleep 30"}, InterruptAfter: "ready", Terminate: true}, 143, Interrupted),
		},
		{
			Name:  "memory limit kills the workload",
			Needs: []capability{MemoryLimit},
			Check: expect(workload{Image: Busybox, Memory: 128, Args: []string{"sh", "-c", "x=$(yes | head -c 400000000); echo survived"}}, 137, OK),
		},
		{
			Name:  "no network without --net",
			Needs: []capability{NetOff},
			Check: expect(workload{Image: Busybox, Args: wget("http://1.1.1.1")}, 1, OK),
		},
		{Name: "allow-lan reaches a host address", Needs: []capability{Net, EgressGuard}, Check: reachHost(true)},
		{Name: "net blocks host addresses without allow-lan", Needs: []capability{Net, EgressGuard}, Check: reachHost(false)},
		{
			Name:  "allow-lan still blocks cloud metadata",
			Needs: []capability{Net, EgressGuard},
			Check: expect(workload{Image: Busybox, Net: true, AllowLAN: true, Args: wget("http://169.254.169.254/")}, 1, OK),
		},
		{
			Name:  "dns resolves through the gateway",
			Needs: []capability{DNS, EgressGuard},
			Check: expect(workload{Image: Busybox, Net: true, Args: []string{"nslookup", "-type=a", "localhost"}}, 0, OK, stdoutContains("127.0.0.1")),
		},
		{Name: "volume is readable and writable both ways", Needs: []capability{Volumes}, Check: volumeReadWrite},
		{Name: "read-only volume rejects writes", Needs: []capability{Volumes}, Check: volumeReadOnly},
		{
			Name:  "volume keeps the image config",
			Needs: []capability{Volumes, ExactConfig},
			Check: withVolume(workload{Image: Configured}, 0, OK, stdout("image image\n1000\n1000\n/tmp\n")),
		},
		{
			Name:  "volume adds nothing to the guest env",
			Needs: []capability{Volumes, ExactConfig},
			Check: withVolume(workload{Image: Busybox, Args: []string{"env"}, Env: []string{"A=1", "TERM=xterm"}}, 0, OK, envExactly("PATH=/bin", "A=1", "TERM=xterm", "HOME=/")),
		},
		{
			Name:  "volume with a missing command",
			Needs: []capability{Volumes},
			Check: withVolume(workload{Image: Busybox, Args: []string{"nope"}}, 127, NotFound),
		},
		{
			Name:  "volume with a relative guest path",
			Needs: []capability{Volumes, SpecValidation},
			Check: invalidVolume(volume{Guest: "work"}),
		},
		{
			Name:  "volume with a missing host directory",
			Needs: []capability{Volumes, SpecValidation},
			Check: invalidVolume(volume{Host: "/nonexistent/sbxr-volume", Guest: workDir}),
		},
		{Name: "publish reaches a guest port on 127.0.0.1", Needs: []capability{Publish, Stdin}, Check: publishReachesGuest},
		{Name: "publish on a busy port", Needs: []capability{Publish, SpecValidation}, Check: publishBusyPort},
	}
}

// changesDoNotPersist removes a file in one run and expects it in the next run.
func changesDoNotPersist(ctx context.Context, t target) error {
	err := expect(workload{Image: Busybox, Args: []string{"rm", "/etc/passwd", HijackFile}}, 0, OK)(ctx, t)
	if err != nil {
		return fmt.Errorf("first run: %w", err)
	}

	err = expect(workload{Image: Busybox, Args: []string{"cat", "/etc/passwd", HijackFile}}, 0, OK, stdoutContains("root:"), stdoutContains("hijacked"))(ctx, t)
	if err != nil {
		return fmt.Errorf("second run: %w", err)
	}

	return nil
}

type resultCheck func(runResult) error

// expect runs w and checks the exit code, the error class, and any extra checks.
func expect(w workload, code int, class errorClass, checks ...resultCheck) func(context.Context, target) error {
	return func(ctx context.Context, t target) error {
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
			return fmt.Errorf("%w (stdout %q, stderr %q)", err, res.Stdout, res.Stderr)
		}

		return nil
	}
}

func stdout(want string) resultCheck {
	return func(r runResult) error {
		if r.Stdout != want {
			return fmt.Errorf("stdout %q, want %q", r.Stdout, want)
		}

		return nil
	}
}

func stdoutContains(want string) resultCheck {
	return func(r runResult) error {
		if !strings.Contains(r.Stdout, want) {
			return fmt.Errorf("stdout %q, want it to contain %q", r.Stdout, want)
		}

		return nil
	}
}

// envExactly checks that stdout lists exactly these env entries, in any order.
func envExactly(want ...string) resultCheck {
	return func(r runResult) error {
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

// hostBody is the response of the host test server.
const hostBody = "from host\n"

// reachHost runs wget in the guest against an HTTP server on a non-loopback host address.
// With allowLAN, the guest must get the response. Without it, the request must fail and never reach the server.
func reachHost(allowLAN bool) func(context.Context, target) error {
	return func(ctx context.Context, t target) error {
		addr, requests, stop, err := hostServer(ctx)
		if err != nil {
			return err
		}
		defer stop()

		w := workload{Image: Busybox, Net: true, AllowLAN: allowLAN, Args: wget("http://" + addr + "/")}

		if allowLAN {
			return expect(w, 0, OK, stdout(hostBody))(ctx, t)
		}

		err = expect(w, 1, OK)(ctx, t)
		if err != nil {
			return err
		}

		if n := requests(); n > 0 {
			return fmt.Errorf("the host server got %d requests from the guest", n)
		}

		return nil
	}
}

// wget returns the arguments that get url with busybox wget and print the body.
func wget(url string) []string { return []string{"wget", "-T", "5", "-qO-", url} }

// hostServer serves hostBody over HTTP on the first non-loopback IPv4 address of the host.
// It never uses a public address: the egress policy treats the host's own addresses as LAN addresses.
func hostServer(ctx context.Context) (addr string, requests func() int64, stop func(), err error) {
	host, ok := hostIPv4()
	if !ok {
		return "", nil, nil, skipError("the host has no non-loopback IPv4 address")
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp4", netip.AddrPortFrom(host, 0).String())
	if err != nil {
		return "", nil, nil, fmt.Errorf("host server: %w", err)
	}

	var n atomic.Int64

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n.Add(1)

			_, _ = io.WriteString(w, hostBody)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() { _ = srv.Serve(ln) }() // Serve ends at Close.

	return ln.Addr().String(), n.Load, func() { _ = srv.Close() }, nil
}

func hostIPv4() (netip.Addr, bool) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return netip.Addr{}, false
	}

	for _, a := range addrs {
		pfx, err := netip.ParsePrefix(a.String())
		if err == nil && pfx.Addr().Is4() && !pfx.Addr().IsLoopback() && !pfx.Addr().IsLinkLocalUnicast() {
			return pfx.Addr(), true
		}
	}

	return netip.Addr{}, false
}

// publishReachesGuest publishes a guest port on a free host port of 127.0.0.1 and reads what the guest sends.
func publishReachesGuest(ctx context.Context, t target) error {
	const msg = "published\n"

	type result struct {
		addr string
		got  string
	}

	results := make(chan result, 1)

	w := workload{
		Image:       Busybox,
		Net:         true,
		Publish:     []publishPort{{Host: 0, Guest: 8080}},
		Interactive: true,
		Stdin:       msg,
		Args:        []string{"nc", "-l", "-p", "8080"},
		Published: func(ctx context.Context, _ uint16, addr string) {
			results <- result{addr: addr, got: readUntil(ctx, addr, msg)}
		},
	}

	err := expect(w, 0, OK)(ctx, t)
	if err != nil {
		return err
	}

	select {
	case r := <-results:
		if !strings.HasPrefix(r.addr, "127.0.0.1:") || r.got != msg {
			return fmt.Errorf("published at %s, read %q; want 127.0.0.1 and %q", r.addr, r.got, msg)
		}

		return nil
	case <-ctx.Done():
		return errors.New("the target never reported the published address")
	}
}

// readUntil connects to addr until a connection returns want, which needs retries until the guest listens.
// It returns the last data read.
func readUntil(ctx context.Context, addr, want string) string {
	var got string

	for ctx.Err() == nil && got != want {
		var d net.Dialer

		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))

			data, _ := io.ReadAll(io.LimitReader(c, int64(len(want)))) // A short read means the guest is not ready.
			got = string(data)

			_ = c.Close()
		}

		if got != want {
			time.Sleep(200 * time.Millisecond)
		}
	}

	return got
}

// publishBusyPort publishes on a host port that is in use. The run must fail before it starts anything.
func publishBusyPort(ctx context.Context, t target) error {
	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("busy port: %w", err)
	}
	defer ln.Close()

	port := netip.MustParseAddrPort(ln.Addr().String()).Port()
	w := workload{Image: Busybox, Net: true, Publish: []publishPort{{Host: port, Guest: 80}}, Args: []string{"false"}}

	return expect(w, 125, InvalidSpec)(ctx, t)
}

// hostFile is a file that the volume scenarios put into the host directory.
const (
	workDir     = "/work"
	hostFile    = "from-host"
	hostContent = "host data\n"
)

// volumeDir creates a host directory with hostFile for one scenario and returns it with its removal.
func volumeDir() (string, func(), error) {
	dir, err := os.MkdirTemp("", "sbxr-volume-")
	if err != nil {
		return "", nil, fmt.Errorf("volume directory: %w", err)
	}

	remove := func() { _ = os.RemoveAll(dir) }

	err = os.WriteFile(filepath.Join(dir, hostFile), []byte(hostContent), 0o644)
	if err != nil {
		remove()

		return "", nil, fmt.Errorf("volume directory: %w", err)
	}

	return dir, remove, nil
}

// volumeReadWrite reads a host file in the volume and creates a file that must appear on the host.
// The working directory is the mount point, which exists only after the mount.
func volumeReadWrite(ctx context.Context, t target) error {
	dir, remove, err := volumeDir()
	if err != nil {
		return err
	}
	defer remove()

	w := workload{
		Image:   Busybox,
		Volumes: []volume{{Host: dir, Guest: workDir}},
		Workdir: workDir,
		Args:    []string{"sh", "-c", "cat " + hostFile + "; touch from-guest; pwd"},
	}

	err = expect(w, 0, OK, stdout(hostContent+workDir+"\n"))(ctx, t)
	if err != nil {
		return err
	}

	_, err = os.Stat(filepath.Join(dir, "from-guest"))
	if err != nil {
		return fmt.Errorf("the file from the guest is missing on the host: %w", err)
	}

	return nil
}

// volumeReadOnly reads a host file in a read-only volume and fails to create a file.
func volumeReadOnly(ctx context.Context, t target) error {
	dir, remove, err := volumeDir()
	if err != nil {
		return err
	}
	defer remove()

	w := workload{
		Image:   Busybox,
		Volumes: []volume{{Host: dir, Guest: "/data", ReadOnly: true}},
		Args:    []string{"sh", "-c", "cat /data/" + hostFile + "; touch /data/from-guest"},
	}

	err = expect(w, 1, OK, stdout(hostContent))(ctx, t)
	if err != nil {
		return err
	}

	_, err = os.Stat(filepath.Join(dir, "from-guest"))
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the guest wrote to a read-only volume: %w", err)
	}

	return nil
}

// withVolume runs check with a read-only volume added to the workload of check, so that scenarios run once with the mount helper.
func withVolume(w workload, code int, class errorClass, checks ...resultCheck) func(context.Context, target) error {
	return func(ctx context.Context, t target) error {
		dir, remove, err := volumeDir()
		if err != nil {
			return err
		}
		defer remove()

		w.Volumes = []volume{{Host: dir, Guest: "/mnt/volume", ReadOnly: true}}

		return expect(w, code, class, checks...)(ctx, t)
	}
}

// invalidVolume expects that the run input with flag is rejected before anything starts.
func invalidVolume(v volume) func(context.Context, target) error {
	return func(ctx context.Context, t target) error {
		dir, remove, err := volumeDir()
		if err != nil {
			return err
		}
		defer remove()

		if v.Host == "" {
			v.Host = dir
		}

		return expect(workload{Image: Busybox, Volumes: []volume{v}, Args: []string{"pwd"}}, 125, InvalidSpec)(ctx, t)
	}
}
