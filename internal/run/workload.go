package run

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/albertsko/sbxr/internal/krunconfig"
	"github.com/albertsko/sbxr/internal/spec"
)

// DefaultPath is the guest PATH when the image sets none.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Workload merges the run request with the image config into the guest init config.
// It returns warnings for image env entries that it drops.
func Workload(s spec.Spec, img ImageConfig) (krunconfig.Config, []string, error) {
	uid, gid, err := user(s.User, img.User)
	if err != nil {
		return krunconfig.Config{}, nil, err
	}

	env, warnings := mergeEnv(img.Env, s.Env)

	return krunconfig.Config{
		UID:        uid,
		GID:        gid,
		WorkingDir: cmp.Or(s.Workdir, img.WorkingDir, "/"),
		Env:        env,
		Args:       args(s, img),
	}, warnings, nil
}

// args follows docker: --entrypoint replaces the image entrypoint and drops the image Cmd.
func args(s spec.Spec, img ImageConfig) []string {
	entrypoint, cmd := img.Entrypoint, img.Cmd

	if s.Entrypoint != nil {
		entrypoint, cmd = nil, nil

		if *s.Entrypoint != "" {
			entrypoint = []string{*s.Entrypoint}
		}
	}

	if len(s.Args) > 0 {
		cmd = s.Args
	}

	return slices.Concat(entrypoint, cmd)
}

// mergeEnv drops invalid and reserved image entries, applies user entries (a later entry wins),
// and adds HOME, TERM, and PATH when they are missing.
func mergeEnv(image, user []string) ([]string, []string) {
	var (
		names    []string
		values   = map[string]string{}
		warnings []string
	)

	set := func(name, value string) {
		if _, ok := values[name]; !ok {
			names = append(names, name)
		}

		values[name] = value
	}

	for _, kv := range image {
		name, value, _ := strings.Cut(kv, "=")

		err := krunconfig.CheckEnvName(name)
		if err == nil {
			err = krunconfig.CheckValue(value)
		}

		if err != nil {
			warnings = append(warnings, fmt.Sprintf("dropped image env %q: %v", kv, err))

			continue
		}

		set(name, value)
	}

	for _, kv := range user {
		name, value, _ := strings.Cut(kv, "=")
		set(name, value)
	}

	defaults := []struct{ name, value string }{{"HOME", "/"}, {"TERM", "linux"}, {"PATH", DefaultPath}}
	for _, d := range defaults {
		if _, ok := values[d.name]; !ok {
			set(d.name, d.value)
		}
	}

	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+values[name])
	}

	return env, warnings
}

// user resolves the guest uid and gid. Without a group, the gid is 0.
func user(flag *spec.User, image string) (uint32, uint32, error) {
	if flag != nil {
		return flag.UID, flag.GID, nil
	}

	if image == "" {
		return 0, 0, nil
	}

	bad := &spec.Error{Source: "image user", Value: image, Problem: "only numeric UID[:GID] below 4294967295 is supported yet", Fix: "pass -u UID[:GID]"}

	uidRaw, gidRaw, hasGID := strings.Cut(image, ":")

	uid, ok := spec.ParseID(uidRaw)
	if !ok {
		return 0, 0, bad
	}

	gid := uint32(0)

	if hasGID {
		gid, ok = spec.ParseID(gidRaw)
		if !ok {
			return 0, 0, bad
		}
	}

	return uid, gid, nil
}
