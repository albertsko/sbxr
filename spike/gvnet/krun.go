package main

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Values from libkrun v1.19.8 include/libkrun.h.
const (
	netFlagVfkit      = 1 << 0
	netFlagDHCPClient = 1 << 1

	netFeatureCsum      = 1 << 0
	netFeatureGuestCsum = 1 << 1
	netFeatureGuestTSO4 = 1 << 7
	netFeatureGuestUFO  = 1 << 10
	netFeatureHostTSO4  = 1 << 11
	netFeatureHostUFO   = 1 << 14

	compatNetFeatures = netFeatureCsum | netFeatureGuestCsum | netFeatureGuestTSO4 |
		netFeatureGuestUFO | netFeatureHostTSO4 | netFeatureHostUFO
)

type vmmConfig struct {
	rootfs   string
	sockPath string
	sockFD   int
	mac      [6]byte
	dhcp     bool
	argv     []string
	env      []string
	logLevel uint32
}

func libkrunName() string {
	if runtime.GOOS == "darwin" {
		return "libkrun.1.dylib"
	}
	return "libkrun.so.1"
}

func cStringArray(items []string) []*byte {
	out := make([]*byte, 0, len(items)+1)
	for _, s := range items {
		b := append([]byte(s), 0)
		out = append(out, &b[0])
	}
	return append(out, nil)
}

// runVMM configures libkrun and calls krun_start_enter, which does not return on success.
func runVMM(cfg vmmConfig) error {
	lib, err := purego.Dlopen(libkrunName(), purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("dlopen libkrun: %w", err)
	}

	var (
		setLogLevel    func(uint32) int32
		createCtx      func() int32
		setVMConfig    func(uint32, uint8, uint32) int32
		setRoot        func(uint32, string) int32
		setWorkdir     func(uint32, string) int32
		setExec        func(uint32, string, unsafe.Pointer, unsafe.Pointer) int32
		addNetUnixgram func(uint32, unsafe.Pointer, int32, *byte, uint32, uint32) int32
		startEnter     func(uint32) int32
	)
	purego.RegisterLibFunc(&setLogLevel, lib, "krun_set_log_level")
	purego.RegisterLibFunc(&createCtx, lib, "krun_create_ctx")
	purego.RegisterLibFunc(&setVMConfig, lib, "krun_set_vm_config")
	purego.RegisterLibFunc(&setRoot, lib, "krun_set_root")
	purego.RegisterLibFunc(&setWorkdir, lib, "krun_set_workdir")
	purego.RegisterLibFunc(&setExec, lib, "krun_set_exec")
	purego.RegisterLibFunc(&addNetUnixgram, lib, "krun_add_net_unixgram")
	purego.RegisterLibFunc(&startEnter, lib, "krun_start_enter")

	setLogLevel(cfg.logLevel)

	ctx := createCtx()
	if ctx < 0 {
		return fmt.Errorf("krun_create_ctx: %d", ctx)
	}
	id := uint32(ctx)

	if rc := setVMConfig(id, 1, 512); rc < 0 {
		return fmt.Errorf("krun_set_vm_config: %d", rc)
	}
	if rc := setRoot(id, cfg.rootfs); rc < 0 {
		return fmt.Errorf("krun_set_root: %d", rc)
	}

	// libkrun sends the VFKT magic only for the path backend, so the flag is set only there.
	var flags uint32
	if cfg.dhcp {
		flags |= netFlagDHCPClient
	}
	var pathPtr unsafe.Pointer
	var pathBuf []byte
	fd := int32(-1)
	if cfg.sockPath != "" {
		pathBuf = append([]byte(cfg.sockPath), 0)
		pathPtr = unsafe.Pointer(&pathBuf[0])
		flags |= netFlagVfkit
	} else {
		fd = int32(cfg.sockFD)
	}
	// Without a socket the VM keeps the default TSI backend. The baseline size and timing build uses this.
	if cfg.sockPath != "" || cfg.sockFD >= 0 {
		if rc := addNetUnixgram(id, pathPtr, fd, &cfg.mac[0], compatNetFeatures, flags); rc < 0 {
			return fmt.Errorf("krun_add_net_unixgram: %d", rc)
		}
	}

	if rc := setWorkdir(id, "/"); rc < 0 {
		return fmt.Errorf("krun_set_workdir: %d", rc)
	}
	argv := cStringArray(cfg.argv[1:])
	envp := cStringArray(cfg.env)
	if rc := setExec(id, cfg.argv[0], unsafe.Pointer(&argv[0]), unsafe.Pointer(&envp[0])); rc < 0 {
		return fmt.Errorf("krun_set_exec: %d", rc)
	}

	fmt.Fprintf(os.Stderr, "vmm: krun_start_enter (pid %d)\n", os.Getpid())
	rc := startEnter(id)
	runtime.KeepAlive(pathBuf)
	runtime.KeepAlive(argv)
	runtime.KeepAlive(envp)
	return fmt.Errorf("krun_start_enter: %d", rc)
}
