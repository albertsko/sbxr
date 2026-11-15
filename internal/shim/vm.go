//go:build linux

package shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/albertsko/sbxr/internal/podvm"
	"github.com/albertsko/sbxr/internal/subid"
	"github.com/albertsko/sbxr/internal/vm/libkrun"
)

// VMArg is the hidden first argument that runs the shim binary as a pod's VM process.
const VMArg = "vm"

// machine is what the VM process needs. The shim writes it as vm.json into the pod directory.
type machine struct {
	LibDir        string `json:"libDir"`
	GuestRoot     string `json:"guestRoot"`
	Share         string `json:"share"`
	ControlSocket string `json:"controlSocket"`
	StreamSocket  string `json:"streamSocket"`
	TAP           string `json:"tap"`
	MAC           string `json:"mac"`
	CPUs          uint8  `json:"cpus"`
	MemoryMiB     uint32 `json:"memoryMiB"`
	LogLevel      uint32 `json:"logLevel"`
}

const machineFile = "vm.json"

// vmProcess is a running VM process.
type vmProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	// exit is the wait status of the VM process, valid after done.
	exit error
}

// startVM starts the VM process for the pod directory dir: in the pod network namespace and in cgroup, as root of a new
// user namespace that maps subid.PerPod ids from uid, with /dev/kvm access through its group when the device needs one.
func startVM(dir, netnsPath string, uid int, cgroup, log *os.File) (*vmProcess, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find shim binary: %w", err)
	}

	cmd := exec.Command(self, VMArg, dir) //nolint:noctx // The VM lives as long as the pod, not a request.
	cmd.Env = libkrun.Env(libDir)
	cmd.Stdout, cmd.Stderr = log, log

	idmap := []syscall.SysProcIDMap{{ContainerID: 0, HostID: uid, Size: subid.PerPod}}
	gidmap := idmap
	cred := &syscall.Credential{Uid: 0, Gid: 0, NoSetGroups: false}

	// The VM process opens /dev/kvm as an unprivileged host user, so it joins the device's group when other users have no access.
	if gid, ok := kvmGroup(); ok {
		gidmap = append(gidmap, syscall.SysProcIDMap{ContainerID: subid.PerPod, HostID: gid, Size: 1})
		cred.Groups = []uint32{subid.PerPod}
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER,
		UidMappings:                idmap,
		GidMappings:                gidmap,
		GidMappingsEnableSetgroups: true,
		Credential:                 cred,
		// The parent thread below lives until the VM exits, so the VM process dies with the shim.
		Pdeathsig: syscall.SIGKILL,
		// clone3 places the process in the pod cgroup before it runs, so no moment escapes the pod's limits.
		UseCgroupFD: true,
		CgroupFD:    int(cgroup.Fd()),
	}

	vm := &vmProcess{cmd: cmd, done: make(chan struct{}), exit: nil}
	started := make(chan error, 1)

	go func() {
		// Start the VM process from a thread in the pod network namespace, and keep the thread for Pdeathsig.
		runtime.LockOSThread()

		err := enterNetns(netnsPath)
		if err == nil {
			err = cmd.Start()
		}

		started <- err

		if err != nil {
			return
		}

		vm.exit = cmd.Wait()
		close(vm.done)
	}()

	if err := <-started; err != nil {
		return nil, fmt.Errorf("start VM process: %w", err)
	}

	return vm, nil
}

func enterNetns(path string) error {
	ns, err := netns.GetFromPath(path)
	if err != nil {
		return fmt.Errorf("open network namespace: %w", err)
	}
	defer ns.Close() //nolint:errcheck // Read-only handle.

	err = netns.Set(ns)
	if err != nil {
		return fmt.Errorf("enter network namespace: %w", err)
	}

	return nil
}

// kvmGroup returns the group of /dev/kvm when the device is not open to all users.
func kvmGroup() (int, bool) {
	var st unix.Stat_t
	if unix.Stat("/dev/kvm", &st) != nil || st.Mode&0o006 == 0o006 || st.Gid == 0 {
		return 0, false
	}

	return int(st.Gid), true
}

// RunVM is the VM process. It returns only when the VM cannot start.
func RunVM(args []string, stderr io.Writer) int {
	err := runVM(args)
	_, _ = fmt.Fprintf(stderr, "sbxr vm: %v\n", err)

	return 1
}

func runVM(args []string) error {
	// Another process of the same user, such as an escaped guest of another pod, cannot attach to this one.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("set not dumpable: %w", err)
	}

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no new privileges: %w", err)
	}

	// The execve as root of the new user namespace cleared the parent death signal, so set it again.
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0); err != nil || os.Getppid() == 1 {
		return errors.Join(errors.New("the shim is gone"), err)
	}

	if len(args) != 1 {
		return fmt.Errorf("want the pod directory, got %d arguments", len(args))
	}

	data, err := os.ReadFile(filepath.Join(args[0], machineFile))
	if err != nil {
		return fmt.Errorf("read VM config: %w", err)
	}

	var m machine

	err = json.Unmarshal(data, &m)
	if err != nil {
		return fmt.Errorf("decode VM config: %w", err)
	}

	cfg, err := vmConfig(m)
	if err != nil {
		return err
	}

	return libkrun.Enter(m.LibDir, cfg) //nolint:wrapcheck // Enter names the failing call.
}

// vmConfig turns m into the libkrun config. It opens the TAP device first, because libkrun reports a TAP device it
// cannot open only in a log line, and then the VM hangs.
func vmConfig(m machine) (libkrun.Config, error) {
	cfg := libkrun.Config{
		Root:      m.GuestRoot,
		CPUs:      m.CPUs,
		MemoryMiB: m.MemoryMiB,
		LogLevel:  m.LogLevel,
		Shares:    []libkrun.Share{{Tag: podvm.ShareTag, Path: m.Share}},
		Vsock: []libkrun.VsockPort{
			{Port: podvm.ControlPort, Path: m.ControlSocket, Listen: true},
			{Port: podvm.StreamPort, Path: m.StreamSocket, Listen: true},
		},
	}

	if m.TAP != "" {
		if err := openTAP(m.TAP); err != nil {
			return cfg, err
		}

		mac, err := parseMAC(m.MAC)
		if err != nil {
			return cfg, err
		}

		cfg.TAP = &libkrun.TAP{Name: m.TAP, MAC: mac}
	}

	return cfg, nil
}

// openTAP opens the TAP device like libkrun does, and closes it again.
func openTAP(name string) error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}
	defer unix.Close(fd) //nolint:errcheck // Only a check.

	var ifr struct {
		name  [unix.IFNAMSIZ]byte
		flags uint16
		_     [22]byte
	}

	copy(ifr.name[:], name)
	ifr.flags = unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return fmt.Errorf("open TAP device %s: %w", name, errno)
	}

	return nil
}

func parseMAC(s string) ([6]byte, error) {
	var mac [6]byte

	n, err := fmt.Sscanf(s, "%02x:%02x:%02x:%02x:%02x:%02x", &mac[0], &mac[1], &mac[2], &mac[3], &mac[4], &mac[5])
	if err != nil || n != len(mac) {
		return mac, errors.Join(fmt.Errorf("MAC address %q is not valid", s), err)
	}

	return mac, nil
}
