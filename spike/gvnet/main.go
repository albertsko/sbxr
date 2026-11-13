//go:build !baseline

// Command gvnet is a spike: the parent process hosts a gvisor-tap-vsock network stack in-process and a child process runs a libkrun VM attached to it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/transport"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

const (
	subnet     = "192.168.127.0/24"
	gatewayIP  = "192.168.127.1"
	guestIP    = "192.168.127.2"
	gatewayMAC = "5a:94:ef:e4:0c:dd"
	guestMAC   = "5a:94:ef:e4:0c:ee"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: gvnet host|vmm [flags] -- guest argv")
	}
	var err error
	switch os.Args[1] {
	case "host":
		err = runHost(os.Args[2:])
	case "vmm":
		err = runVMMCmd(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}

func runVMMCmd(args []string) error {
	fs := flag.NewFlagSet("vmm", flag.ExitOnError)
	rootfs := fs.String("rootfs", "", "guest root directory")
	sock := fs.String("sock", "", "gvisor-tap-vsock unixgram socket path (path mode)")
	sockFD := fs.Int("fd", -1, "inherited datagram socket fd (fd mode)")
	dhcp := fs.Bool("dhcp", false, "set NET_FLAG_DHCP_CLIENT")
	logLevel := fs.Uint("log-level", 1, "libkrun log level")
	var env multiFlag
	fs.Var(&env, "env", "guest env KEY=VALUE (repeatable)")
	_ = fs.Parse(args)

	mac, err := net.ParseMAC(guestMAC)
	if err != nil {
		return err
	}
	cfg := vmmConfig{
		rootfs:   *rootfs,
		sockPath: *sock,
		sockFD:   *sockFD,
		dhcp:     *dhcp,
		argv:     fs.Args(),
		env:      append([]string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}, env...),
		logLevel: uint32(*logLevel),
	}
	copy(cfg.mac[:], mac)
	if len(cfg.argv) == 0 {
		return errors.New("vmm: missing guest argv")
	}
	return runVMM(cfg)
}

func runHost(args []string) error {
	start := time.Now()
	fs := flag.NewFlagSet("host", flag.ExitOnError)
	rootfs := fs.String("rootfs", "", "guest root directory")
	mode := fs.String("mode", "path", "VM socket mode: path (unixgram path + VFKT magic) or fd (socketpair)")
	dhcp := fs.Bool("dhcp", false, "let libkrun init configure eth0 with DHCP")
	filter := fs.Bool("filter", false, "drop guest frames to 127.0.0.0/8, 0.0.0.0/8 and 169.254.0.0/16")
	linger := fs.Duration("linger", 0, "keep the parent alive after cleanup (for lsof checks)")
	var env multiFlag
	fs.Var(&env, "env", "guest env KEY=VALUE (repeatable)")
	_ = fs.Parse(args)
	log.SetPrefix("host: ")
	log.SetFlags(log.Lmicroseconds)
	log.Printf("pid %d mode=%s dhcp=%v filter=%v", os.Getpid(), *mode, *dhcp, *filter)

	runDir, err := os.MkdirTemp("", "gvnet-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(runDir)

	// NAT and GatewayVirtualIPs stay empty, so 192.168.127.254 does not exist and is not mapped to host 127.0.0.1.
	cfg := types.Configuration{
		MTU:               1500,
		Subnet:            subnet,
		GatewayIP:         gatewayIP,
		GatewayMacAddress: gatewayMAC,
		DHCPStaticLeases:  map[string]string{guestIP: guestMAC},
		Forwards:          map[string]string{"127.0.0.1:18080": guestIP + ":8080"},
		NAT:               map[string]string{},
		GatewayVirtualIPs: []string{},
		Protocol:          types.VfkitProtocol,
	}
	vn, err := virtualnetwork.New(&cfg)
	if err != nil {
		return fmt.Errorf("virtualnetwork.New: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gc := &guestConn{}
	if *filter {
		for _, p := range []string{"127.0.0.0/8", "0.0.0.0/8", "169.254.0.0/16"} {
			gc.blocked = append(gc.blocked, netip.MustParsePrefix(p))
		}
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	childArgs := []string{"vmm", "-rootfs", *rootfs}
	if *dhcp {
		childArgs = append(childArgs, "-dhcp")
	}
	for _, e := range env {
		childArgs = append(childArgs, "-env", e)
	}

	var closeVMSide func() error
	var acceptGuest func() (net.Conn, error)
	var extraFiles []*os.File
	switch *mode {
	case "path":
		sockPath := filepath.Join(runDir, "net.sock")
		listenConn, err := transport.ListenUnixgram("unixgram://" + sockPath)
		if err != nil {
			return fmt.Errorf("ListenUnixgram: %w", err)
		}
		closeVMSide = listenConn.Close
		acceptGuest = func() (net.Conn, error) { return transport.AcceptVfkit(listenConn) }
		childArgs = append(childArgs, "-sock", sockPath)
	case "fd":
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
		if err != nil {
			return fmt.Errorf("socketpair: %w", err)
		}
		hostFile := os.NewFile(uintptr(fds[0]), "gvnet-host")
		vmFile := os.NewFile(uintptr(fds[1]), "gvnet-vm")
		hostConn, err := net.FileConn(hostFile)
		hostFile.Close()
		if err != nil {
			return fmt.Errorf("FileConn: %w", err)
		}
		closeVMSide = hostConn.Close
		acceptGuest = func() (net.Conn, error) { return hostConn, nil }
		extraFiles = []*os.File{vmFile}
		childArgs = append(childArgs, "-fd", "3")
		defer vmFile.Close()
	default:
		return fmt.Errorf("unknown mode %q", *mode)
	}
	log.Printf("stack ready after %v", time.Since(start))

	childArgs = append(childArgs, "--")
	childArgs = append(childArgs, fs.Args()...)
	child := exec.Command(self, childArgs...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.ExtraFiles = extraFiles
	// libkrun binds its own end of the path-mode socket in TMPDIR, so point TMPDIR at runDir to clean it up.
	child.Env = append(os.Environ(), "TMPDIR="+runDir)
	// macOS strips DYLD_* when a SIP-protected shell starts us, so GVNET_LIB_DIR passes the libkrun directory through.
	if dir := os.Getenv("GVNET_LIB_DIR"); dir != "" {
		child.Env = append(child.Env, "DYLD_FALLBACK_LIBRARY_PATH="+dir, "LD_LIBRARY_PATH="+dir)
	}
	if err := child.Start(); err != nil {
		return fmt.Errorf("start vmm: %w", err)
	}
	for _, f := range extraFiles {
		f.Close()
	}
	log.Printf("vmm child pid %d started after %v", child.Process.Pid, time.Since(start))

	go func() {
		conn, err := acceptGuest()
		if err != nil {
			log.Printf("accept guest: %v", err)
			return
		}
		log.Printf("guest connected after %v", time.Since(start))
		gc.Conn = conn
		if err := vn.AcceptVfkit(ctx, gc); err != nil && ctx.Err() == nil {
			log.Printf("AcceptVfkit: %v", err)
		}
	}()

	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	var waitErr error
	select {
	case waitErr = <-childDone:
	case s := <-sigs:
		log.Printf("got %v, stopping vmm", s)
		_ = child.Process.Signal(syscall.SIGTERM)
		select {
		case waitErr = <-childDone:
		case <-time.After(3 * time.Second):
			_ = child.Process.Kill()
			waitErr = <-childDone
		}
	}
	log.Printf("vmm exited after %v: %v", time.Since(start), waitErr)

	cancel()
	if err := closeVMSide(); err != nil {
		log.Printf("close vm socket: %v", err)
	}
	if err := vn.Close(); err != nil {
		log.Printf("vn.Close: %v", err)
	}
	if err := os.RemoveAll(runDir); err != nil {
		log.Printf("remove run dir: %v", err)
	}
	log.Printf("cleanup done, dropped %d guest frames, run dir removed: %s", gc.dropped.Load(), runDir)

	if *linger > 0 {
		log.Printf("lingering %v", *linger)
		time.Sleep(*linger)
	}
	return nil
}
