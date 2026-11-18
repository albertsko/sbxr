package run

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/albertsko/sbxr/internal/krunconfig"
	"github.com/albertsko/sbxr/internal/runspec"
)

// DefaultPath is the guest PATH when the image sets none.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Workload merges the run request with the image config into the guest init config.
// The -u flag wins over the image User, which resolveUser resolves only when needed.
// It returns warnings for image env entries that it drops.
func Workload(s runspec.Spec, img ImageConfig, resolveUser UserResolver) (krunconfig.Config, []string, error) {
	var uid, gid uint32

	if s.User != nil {
		uid, gid = s.User.UID, s.User.GID
	} else {
		var err error

		uid, gid, err = resolveUser(img.User)
		if err != nil {
			return krunconfig.Config{}, nil, err
		}
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
func args(s runspec.Spec, img ImageConfig) []string {
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

// UserResolver returns the uid and gid for an image User value, such as "app" or "1000:staff".
type UserResolver func(user string) (uid, gid uint32, err error)
