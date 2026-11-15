package preflight

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/albertsko/sbxr/internal/userns"
)

const (
	hypervisorDetail = "/dev/kvm is usable"
	libkrunInstall   = "install libkrun 1.19.8 or later built with NET=1 and libkrunfw 5.6.4 or later into /usr/local/lib"
	krunfwInstall    = "install libkrunfw 5.6.4 or later into /usr/local/lib"
)

var platformChecks = []Check{Userns}

func hypervisorFix(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "enable virtualization (KVM) for this machine"
	case errors.Is(err, fs.ErrPermission):
		return `sudo usermod -aG kvm "$USER" and log in again`
	default:
		return "use a Linux kernel with standard KVM"
	}
}

// Userns checks that sbxr can map the user's subordinate ids, chown, and mount in a new user namespace.
func Userns(ctx context.Context) Result {
	const id = "userns"

	err := userns.Check(ctx)
	if err != nil {
		return Failed(id, err.Error(), usernsFix(usernsSetup(err)))
	}

	return Passed(id, "user namespaces with subordinate ids work")
}

// installPaths are the paths that the AppArmor profile covers.
var installPaths = []string{"/usr/bin/sbxr", "/usr/local/bin/sbxr"}

// appArmorProfile is where the AppArmor profile of sbxr is installed.
const appArmorProfile = "/etc/apparmor.d/sbxr"

// usernsState is what the fix for a failed userns check depends on.
type usernsState struct {
	noHelpers, noSubIDs bool
	// restricts reports that AppArmor restricts unprivileged user namespaces.
	restricts bool
	// exe is the resolved path of the running binary, or "" when unknown.
	exe                 string
	profileExists       bool
	primaryGroupDiffers bool
}

// usernsSetup reads the state that usernsFix needs after the check failed with err.
func usernsSetup(err error) usernsState {
	exe, exeErr := os.Executable()
	if exeErr == nil {
		exe, exeErr = filepath.EvalSymlinks(exe)
	}

	if exeErr != nil {
		exe = ""
	}

	_, profileErr := os.Stat(appArmorProfile)

	return usernsState{
		noHelpers:           errors.Is(err, userns.ErrNoHelpers),
		noSubIDs:            errors.Is(err, userns.ErrNoSubIDs),
		restricts:           appArmorRestricts(),
		exe:                 exe,
		profileExists:       profileErr == nil,
		primaryGroupDiffers: !primaryGroup(),
	}
}

// usernsFix returns the fix for a failed userns check. The check detail holds the helper's own message.
func usernsFix(st usernsState) string {
	covered := slices.Contains(installPaths, st.exe)

	switch {
	case st.noHelpers:
		return "sudo apt-get install -y uidmap"
	case st.noSubIDs:
		return `sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 "$USER"`
	case st.restricts && !covered:
		exe := cmp.Or(st.exe, "./sbxr")

		return "install sbxr to /usr/local/bin, then run /usr/local/bin/sbxr doctor for the AppArmor fix: sudo install -m 755 " + exe + " /usr/local/bin/sbxr"
	case st.restricts && !st.profileExists:
		return "sbxr doctor apparmor-profile | sudo tee " + appArmorProfile + " && sudo apparmor_parser -r " + appArmorProfile
	case st.primaryGroupDiffers:
		return "log in again: newgidmap maps only the primary group from /etc/passwd, which newgrp and sg replace"
	case st.restricts:
		// The profile file exists, but the kernel may not have loaded it.
		return "sudo apparmor_parser -r " + appArmorProfile
	default:
		return "enable unprivileged user namespaces: sudo sysctl -w kernel.unprivileged_userns_clone=1"
	}
}

// appArmorRestricts reports whether AppArmor restricts unprivileged user namespaces (Ubuntu 23.10 and later).
func appArmorRestricts() bool {
	data, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")

	return err == nil && strings.TrimSpace(string(data)) == "1"
}

// primaryGroup reports whether the process runs with the primary group of its user from /etc/passwd.
func primaryGroup() bool {
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))

	return err != nil || u.Gid == strconv.Itoa(os.Getgid())
}
