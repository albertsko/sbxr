// Package runspec turns raw run input into a validated run request.
//
// It does no I/O, so every check runs before any download or VM start.
package runspec

import (
	"fmt"
	"math"
	"net/netip"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/albertsko/sbxr/internal/guestmount"
	"github.com/albertsko/sbxr/internal/krunconfig"
)

// Default values for flags that the user did not set.
const (
	DefaultCPUs   = 2
	DefaultMemory = "1G"
)

// Pull is the image download policy.
type Pull string

// Pull policies.
const (
	PullMissing Pull = "missing"
	PullAlways  Pull = "always"
	PullNever   Pull = "never"
)

// Input is the raw run input, as the user typed it.
type Input struct {
	Image string
	Args  []string

	CPUs     string // Empty means the default.
	Memory   string // Empty means the default.
	Env      []string
	EnvFiles []EnvFile
	Volumes  []string
	// Dir is the absolute directory that relative -v host paths start from, usually the working directory.
	Dir        string
	Publish    []string
	Net        bool
	AllowLAN   bool
	Workdir    string
	User       string
	Entrypoint *string // Nil means the image value.
	Pull       string  // Empty means the default.

	Interactive bool
	TTY         bool
}

// EnvFile is the content of one --env-file.
type EnvFile struct {
	Path string
	Data []byte
}

// Spec is a validated run request.
type Spec struct {
	Image string
	Args  []string

	CPUs      int
	MemoryMiB uint32
	// Env holds KEY=VALUE entries in input order. A later entry wins.
	Env        []string
	Volumes    []Volume
	Publish    []Port
	Net        bool
	AllowLAN   bool
	Workdir    string
	User       *User
	Entrypoint *string
	Pull       Pull

	Interactive bool
	TTY         bool
}

// Volume is a host directory shared with the guest.
type Volume struct {
	Host     string
	Guest    string
	ReadOnly bool
}

// Port publishes a guest TCP port on a host address.
type Port struct {
	HostIP    netip.Addr
	HostPort  uint16
	GuestPort uint16
}

// User is the guest user from -u.
type User struct {
	UID uint32
	GID uint32 // 0 when -u has no group.
}

// Parse validates in and returns the run request.
// hostCPUs is the number of CPUs on the host.
func Parse(in Input, hostCPUs int) (Spec, error) {
	checks := []func() error{
		func() error { return checkImage(in.Image) },
		func() error { return checkArgs(in.Args) },
		func() error { return checkNet(in) },
		func() error { return checkWorkdir(in.Workdir) },
		func() error { return checkEntrypoint(in.Entrypoint) },
	}

	for _, check := range checks {
		if err := check(); err != nil {
			return Spec{}, err
		}
	}

	cpus, err := parseCPUs(in.CPUs, hostCPUs)
	if err != nil {
		return Spec{}, err
	}

	memory, err := parseMemory(in.Memory)
	if err != nil {
		return Spec{}, err
	}

	env, err := parseEnv(in.Env, in.EnvFiles)
	if err != nil {
		return Spec{}, err
	}

	volumes, err := parseVolumes(in.Volumes, in.Dir)
	if err != nil {
		return Spec{}, err
	}

	ports, err := parsePublish(in.Publish)
	if err != nil {
		return Spec{}, err
	}

	user, err := parseUser(in.User)
	if err != nil {
		return Spec{}, err
	}

	pull, err := parsePull(in.Pull)
	if err != nil {
		return Spec{}, err
	}

	return Spec{
		Image:       in.Image,
		Args:        in.Args,
		CPUs:        cpus,
		MemoryMiB:   memory,
		Env:         env,
		Volumes:     volumes,
		Publish:     ports,
		Net:         in.Net,
		AllowLAN:    in.AllowLAN,
		Workdir:     in.Workdir,
		User:        user,
		Entrypoint:  in.Entrypoint,
		Pull:        pull,
		Interactive: in.Interactive,
		TTY:         in.TTY,
	}, nil
}

func checkImage(ref string) error {
	if ref == "" {
		return &Error{Source: "IMAGE", Problem: "missing", Fix: "for example: sbxr run alpine echo hi"}
	}

	_, err := name.ParseReference(ref)
	if err != nil {
		return &Error{Source: "IMAGE", Value: ref, Problem: "invalid image reference", Fix: "for example alpine or ghcr.io/org/app:1.0"}
	}

	return nil
}

func checkArgs(args []string) error {
	for i, arg := range args {
		err := checkValue(fmt.Sprintf("argument %d", i+1), arg)
		if err != nil {
			return err
		}
	}

	return nil
}

func parseCPUs(raw string, hostCPUs int) (int, error) {
	if raw == "" {
		return min(DefaultCPUs, hostCPUs), nil
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > hostCPUs {
		return 0, &Error{Source: "--cpus", Value: raw, Problem: fmt.Sprintf("must be a number from 1 to %d", hostCPUs), Fix: "for example --cpus 2"}
	}

	return n, nil
}

func parseMemory(raw string) (uint32, error) {
	if raw == "" {
		raw = DefaultMemory
	}

	bad := &Error{Source: "--memory", Value: raw, Problem: "must be a size of at least 1M in whole MiB", Fix: "for example 512M or 2G"}

	units := map[byte]uint64{'k': 1 << 10, 'm': 1 << 20, 'g': 1 << 30}
	digits, unit := raw, uint64(1)

	if n := len(raw); n > 0 {
		if u, ok := units[raw[n-1]|0x20]; ok {
			digits, unit = raw[:n-1], u
		}
	}

	n, err := strconv.ParseUint(digits, 10, 32)
	if err != nil || n == 0 {
		return 0, bad
	}

	size := n * unit
	if size%(1<<20) != 0 || size>>20 > 1<<32-1 {
		return 0, bad
	}

	return uint32(size >> 20), nil
}

func parseEnv(flags []string, files []EnvFile) ([]string, error) {
	var env []string

	for _, f := range files {
		for i, line := range strings.Split(string(f.Data), "\n") {
			// Values stay unchanged, except for the CR of a CRLF line end.
			line = strings.TrimSuffix(line, "\r")

			trimmed := strings.TrimLeft(line, " \t")
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}

			source := fmt.Sprintf("--env-file %s:%d", f.Path, i+1)

			err := checkEnv(source, line)
			if err != nil {
				return nil, err
			}

			env = append(env, line)
		}
	}

	for _, kv := range flags {
		err := checkEnv("-e", kv)
		if err != nil {
			return nil, err
		}

		env = append(env, kv)
	}

	return env, nil
}

func checkEnv(source, kv string) error {
	key, value, ok := strings.Cut(kv, "=")
	if !ok {
		return &Error{Source: source, Value: kv, Problem: "must be KEY=VALUE", Fix: "the host environment is never passed to the guest"}
	}

	err := krunconfig.CheckEnvName(key)
	if err == nil {
		err = krunconfig.CheckValue(value)
	}

	if err != nil {
		return &Error{Source: source, Value: kv, Problem: err.Error()}
	}

	return nil
}

func parseVolumes(raw []string, dir string) ([]Volume, error) {
	volumes := make([]Volume, 0, len(raw))
	seen := map[string]bool{}

	for _, v := range raw {
		bad := &Error{Source: "-v", Value: v, Problem: "must be HOST:GUEST or HOST:GUEST:ro", Fix: "for example -v ./src:/src:ro"}

		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" {
			return nil, bad
		}

		readOnly := len(parts) == 3
		if readOnly && parts[2] != "ro" {
			return nil, bad
		}

		for _, p := range parts[:2] {
			err := checkValue("-v", p)
			if err != nil {
				return nil, err
			}
		}

		guest := path.Clean(parts[1])
		if !path.IsAbs(parts[1]) || guest == "/" {
			return nil, &Error{Source: "-v", Value: v, Problem: "the guest path must be absolute and not /", Fix: "for example /work"}
		}

		if r := guestmount.Reserved(guest); r != "" {
			return nil, &Error{Source: "-v", Value: v, Problem: "the guest path " + r + " is reserved", Fix: "choose another guest path"}
		}

		if seen[guest] {
			return nil, &Error{Source: "-v", Value: v, Problem: "the guest path is used twice"}
		}

		seen[guest] = true

		host := parts[0]
		if !filepath.IsAbs(host) {
			host = filepath.Join(dir, host)
		}

		volumes = append(volumes, Volume{Host: filepath.Clean(host), Guest: guest, ReadOnly: readOnly})
	}

	return volumes, nil
}

func parsePublish(raw []string) ([]Port, error) {
	ports := make([]Port, 0, len(raw))

	for _, p := range raw {
		bad := &Error{Source: "-p", Value: p, Problem: "must be [IP:]HOST:GUEST", Fix: "for example -p 8080:80"}

		parts := strings.Split(p, ":")
		if len(parts) > 3 || strings.HasPrefix(p, "[") {
			return nil, &Error{Source: "-p", Value: p, Problem: "IPv6 bind addresses are not supported", Fix: "use an IPv4 address"}
		}

		if len(parts) < 2 {
			return nil, bad
		}

		ip := netip.AddrFrom4([4]byte{127, 0, 0, 1})

		if len(parts) == 3 {
			addr, err := netip.ParseAddr(parts[0])
			if err != nil || !addr.Is4() {
				return nil, &Error{Source: "-p", Value: p, Problem: "the bind address must be an IPv4 address", Fix: "for example 127.0.0.1:8080:80"}
			}

			ip, parts = addr, parts[1:]
		}

		host, err1 := strconv.ParseUint(parts[0], 10, 16)
		guest, err2 := strconv.ParseUint(parts[1], 10, 16)

		if err1 != nil || err2 != nil || guest == 0 {
			return nil, bad
		}

		ports = append(ports, Port{HostIP: ip, HostPort: uint16(host), GuestPort: uint16(guest)})
	}

	return ports, nil
}

func checkNet(in Input) error {
	if in.Net {
		return nil
	}

	if in.AllowLAN {
		return &Error{Source: "--allow-lan", Problem: "requires --net"}
	}

	if len(in.Publish) > 0 {
		return &Error{Source: "-p", Value: in.Publish[0], Problem: "requires --net"}
	}

	return nil
}

func checkWorkdir(dir string) error {
	if dir == "" {
		return nil
	}

	err := checkValue("-w", dir)
	if err != nil {
		return err
	}

	if !path.IsAbs(dir) {
		return &Error{Source: "-w", Value: dir, Problem: "must be absolute", Fix: "for example /" + strings.TrimLeft(dir, "./")}
	}

	return nil
}

func parseUser(raw string) (*User, error) {
	if raw == "" {
		return nil, nil //nolint:nilnil // No -u means the image user.
	}

	bad := &Error{Source: "-u", Value: raw, Problem: "must be UID or UID:GID with decimal numbers below 4294967295", Fix: "for example -u 1000:1000"}

	uidRaw, gidRaw, hasGID := strings.Cut(raw, ":")

	uid, ok := ParseID(uidRaw)
	if !ok {
		return nil, bad
	}

	gid := uint32(0)

	if hasGID {
		gid, ok = ParseID(gidRaw)
		if !ok {
			return nil, bad
		}
	}

	return &User{UID: uid, GID: gid}, nil
}

// ParseID parses a decimal user or group ID.
// It rejects 4294967295, because setuid and setgid treat it as -1.
func ParseID(raw string) (uint32, bool) {
	id, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || id == math.MaxUint32 {
		return 0, false
	}

	return uint32(id), true
}

func checkEntrypoint(entrypoint *string) error {
	if entrypoint == nil {
		return nil
	}

	return checkValue("--entrypoint", *entrypoint)
}

func parsePull(raw string) (Pull, error) {
	switch p := Pull(raw); p {
	case "":
		return PullMissing, nil
	case PullMissing, PullAlways, PullNever:
		return p, nil
	default:
		return "", &Error{Source: "--pull", Value: raw, Problem: "unknown policy", Fix: "use missing, always, or never"}
	}
}

func checkValue(source, value string) error {
	err := krunconfig.CheckValue(value)
	if err != nil {
		return &Error{Source: source, Value: value, Problem: err.Error()}
	}

	return nil
}
