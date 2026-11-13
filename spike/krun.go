//go:build linux

package main

import (
	"fmt"
	"os"

	"github.com/ebitengine/purego"
)

func runVM(root string, argv []string) error {
	lib, err := purego.Dlopen("libkrun.so.1", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return err
	}

	var (
		setLogLevel func(uint32) int32
		createCtx   func() int32
		setVMConfig func(uint32, uint8, uint32) int32
		setRoot     func(uint32, string) int32
		setExec     func(uint32, string, []*byte, []*byte) int32
		startEnter  func(uint32) int32
	)
	purego.RegisterLibFunc(&setLogLevel, lib, "krun_set_log_level")
	purego.RegisterLibFunc(&createCtx, lib, "krun_create_ctx")
	purego.RegisterLibFunc(&setVMConfig, lib, "krun_set_vm_config")
	purego.RegisterLibFunc(&setRoot, lib, "krun_set_root")
	purego.RegisterLibFunc(&setExec, lib, "krun_set_exec")
	purego.RegisterLibFunc(&startEnter, lib, "krun_start_enter")

	setLogLevel(1)
	ctx := createCtx()
	if ctx < 0 {
		return fmt.Errorf("krun_create_ctx: %d", ctx)
	}
	c := uint32(ctx)
	if rc := setVMConfig(c, 1, 512); rc < 0 {
		return fmt.Errorf("krun_set_vm_config: %d", rc)
	}
	if rc := setRoot(c, root); rc < 0 {
		return fmt.Errorf("krun_set_root: %d", rc)
	}
	env := cStrings([]string{"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME=/root"})
	if rc := setExec(c, argv[0], cStrings(argv[1:]), env); rc < 0 {
		return fmt.Errorf("krun_set_exec: %d", rc)
	}

	rc := startEnter(c)
	fmt.Fprintf(os.Stderr, "krun_start_enter returned %d\n", rc)
	return fmt.Errorf("krun_start_enter: %d", rc)
}

func cStrings(ss []string) []*byte {
	out := make([]*byte, 0, len(ss)+1)
	for _, s := range ss {
		b := append([]byte(s), 0)
		out = append(out, &b[0])
	}
	return append(out, nil)
}
