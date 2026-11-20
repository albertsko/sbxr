package run

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/albertsko/sbxr/internal/image"
	"github.com/albertsko/sbxr/internal/krunconfig"
	"github.com/albertsko/sbxr/internal/runspec"
)

// DefaultPath is the guest PATH when the image sets none.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Workload merges the run request with the config of img into the guest init config.
// It returns warnings for image env entries that it drops.
func Workload(s runspec.Spec, img *image.Image) (krunconfig.Config, []string, error) {
	uid, gid, err := workloadUser(s, img)
	if err != nil {
		return krunconfig.Config{}, nil, err
	}

	env, warnings := mergeEnv(img.Config.Env, s.Env)

	return krunconfig.Config{
		UID:        uid,
		GID:        gid,
		WorkingDir: cmp.Or(s.Workdir, img.Config.WorkingDir, "/"),
		Env:        env,
		Args:       args(s, img.Config),
	}, warnings, nil
}

// workloadUser returns the -u flag, or else the image User resolved in the image root.
func workloadUser(s runspec.Spec, img *image.Image) (uint32, uint32, error) {
	if s.User != nil {
		return s.User.UID, s.User.GID, nil
	}

	uid, gid, err := img.ResolveUser(img.Config.User)
	if err != nil {
		return 0, 0, imageError(s, err)
	}

	return uid, gid, nil
}

// args follows docker: --entrypoint replaces the image entrypoint and drops the image Cmd.
func args(s runspec.Spec, img image.Config) []string {
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
func mergeEnv(imageEnv, user []string) ([]string, []string) {
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

	for _, kv := range imageEnv {
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
