package runspec_test

import (
	"errors"
	"net/netip"
	"reflect"
	"testing"

	"github.com/albertsko/sbxr/internal/runspec"
)

func TestParseAcceptsValidInput(t *testing.T) {
	t.Parallel()

	entrypoint := ""
	in := runspec.Input{
		Image:       "alpine:3.22",
		Args:        []string{"sh", "-c", "echo 'a\tb'\né uid Env args"},
		CPUs:        "1",
		Memory:      "512m",
		Env:         []string{"A=1", "EMPTY="},
		EnvFiles:    []runspec.EnvFile{{Path: "app.env", Data: []byte("# comment\n\nB=2\n")}},
		Volumes:     []string{"./src:/src/:ro", "/data:/data"},
		Dir:         "/home/u",
		Publish:     []string{"8080:80", "0.0.0.0:0:443"},
		Net:         true,
		AllowLAN:    true,
		Workdir:     "/app",
		User:        "1000:42",
		Entrypoint:  &entrypoint,
		Pull:        "never",
		Interactive: true,
		TTY:         true,
	}

	got, err := runspec.Parse(in, 4)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := runspec.Spec{
		Image:     "alpine:3.22",
		Args:      in.Args,
		CPUs:      1,
		MemoryMiB: 512,
		Env:       []string{"B=2", "A=1", "EMPTY="},
		Volumes: []runspec.Volume{
			{Host: "/home/u/src", Guest: "/src", ReadOnly: true},
			{Host: "/data", Guest: "/data", ReadOnly: false},
		},
		Publish: []runspec.Port{
			{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: 8080, GuestPort: 80},
			{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 0, GuestPort: 443},
		},
		Net:         true,
		AllowLAN:    true,
		Workdir:     "/app",
		User:        &runspec.User{UID: 1000, GID: 42},
		Entrypoint:  &entrypoint,
		Pull:        runspec.PullNever,
		Interactive: true,
		TTY:         true,
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		hostCPUs int
		wantCPUs int
	}{
		{name: "two CPUs", hostCPUs: 8, wantCPUs: 2},
		{name: "capped at the host CPU count", hostCPUs: 1, wantCPUs: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := runspec.Parse(runspec.Input{Image: "alpine"}, tt.hostCPUs)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			if got.CPUs != tt.wantCPUs || got.MemoryMiB != 1024 || got.Pull != runspec.PullMissing || got.User != nil {
				t.Errorf("Parse() = %+v, want %d CPUs, 1024 MiB, pull missing, no user", got, tt.wantCPUs)
			}
		})
	}
}

func TestParseRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   runspec.Input
		want string
	}{
		{name: "no image", in: runspec.Input{}, want: `IMAGE: missing; for example: sbxr run alpine echo hi`},
		{name: "bad image", in: runspec.Input{Image: "Alpine"}, want: `IMAGE "Alpine": invalid image reference; for example alpine or ghcr.io/org/app:1.0`},
		{name: "control character in argument", in: runspec.Input{Image: "alpine", Args: []string{"echo", "a\x1bb"}}, want: `argument 2 "a\x1bb": control character U+001B; only tab and newline are allowed`},
		{name: "zero CPUs", in: runspec.Input{Image: "alpine", CPUs: "0"}, want: `--cpus "0": must be a number from 1 to 4; for example --cpus 2`},
		{name: "more CPUs than the host", in: runspec.Input{Image: "alpine", CPUs: "5"}, want: `--cpus "5": must be a number from 1 to 4; for example --cpus 2`},
		{name: "zero memory", in: runspec.Input{Image: "alpine", Memory: "0"}, want: `--memory "0": must be a size of at least 1M in whole MiB; for example 512M or 2G`},
		{name: "memory below 1 MiB", in: runspec.Input{Image: "alpine", Memory: "1k"}, want: `--memory "1k": must be a size of at least 1M in whole MiB; for example 512M or 2G`},
		{name: "memory unit only", in: runspec.Input{Image: "alpine", Memory: "G"}, want: `--memory "G": must be a size of at least 1M in whole MiB; for example 512M or 2G`},
		{name: "KRUN env", in: runspec.Input{Image: "alpine", Env: []string{"KRUN_INIT=/x"}}, want: `-e "KRUN_INIT=/x": KRUN_* names are reserved by the VM init; rename the variable`},
		{name: "HOSTNAME env", in: runspec.Input{Image: "alpine", Env: []string{"HOSTNAME=x"}}, want: `-e "HOSTNAME=x": HOSTNAME is reserved by the VM init; rename the variable`},
		{name: "env name is not an identifier", in: runspec.Input{Image: "alpine", Env: []string{"1A=x"}}, want: `-e "1A=x": not a valid variable name; use letters, digits, and underscores, and start with a letter or underscore`},
		{name: "env without value", in: runspec.Input{Image: "alpine", Env: []string{"HOME"}}, want: `-e "HOME": must be KEY=VALUE; the host environment is never passed to the guest`},
		{name: "control character in env", in: runspec.Input{Image: "alpine", Env: []string{"A=\r"}}, want: `-e "A=\r": control character U+000D; only tab and newline are allowed`},
		{name: "env file line", in: runspec.Input{Image: "alpine", EnvFiles: []runspec.EnvFile{{Path: "a.env", Data: []byte("A=1\nKRUN_X=1\n")}}}, want: `--env-file a.env:2 "KRUN_X=1": KRUN_* names are reserved by the VM init; rename the variable`},
		{name: "volume without guest", in: runspec.Input{Image: "alpine", Volumes: []string{"./src"}}, want: `-v "./src": must be HOST:GUEST or HOST:GUEST:ro; for example -v ./src:/src:ro`},
		{name: "volume with bad mode", in: runspec.Input{Image: "alpine", Volumes: []string{"./src:/src:rw"}}, want: `-v "./src:/src:rw": must be HOST:GUEST or HOST:GUEST:ro; for example -v ./src:/src:ro`},
		{name: "volume on guest root", in: runspec.Input{Image: "alpine", Volumes: []string{"./src:/"}}, want: `-v "./src:/": the guest path must be absolute and not /; for example /work`},
		{name: "relative volume guest", in: runspec.Input{Image: "alpine", Volumes: []string{"./src:src"}}, want: `-v "./src:src": the guest path must be absolute and not /; for example /work`},
		{name: "volume on /proc", in: runspec.Input{Image: "alpine", Volumes: []string{"/x:/proc/x"}}, want: `-v "/x:/proc/x": the guest path /proc is reserved; choose another guest path`},
		{name: "volume on the helper directory", in: runspec.Input{Image: "alpine", Volumes: []string{"/x:/.sbxr-mount"}}, want: `-v "/x:/.sbxr-mount": the guest path /.sbxr-mount is reserved; choose another guest path`},
		{name: "volume on the guest config", in: runspec.Input{Image: "alpine", Volumes: []string{"/x:/.krun_config.json"}}, want: `-v "/x:/.krun_config.json": the guest path /.krun_config.json is reserved; choose another guest path`},
		{name: "volume guest path twice", in: runspec.Input{Image: "alpine", Volumes: []string{"/x:/w", "/y:/w/"}}, want: `-v "/y:/w/": the guest path is used twice`},
		{name: "publish without guest", in: runspec.Input{Image: "alpine", Net: true, Publish: []string{"8080"}}, want: `-p "8080": must be [IP:]HOST:GUEST; for example -p 8080:80`},
		{name: "publish guest port 0", in: runspec.Input{Image: "alpine", Net: true, Publish: []string{"8080:0"}}, want: `-p "8080:0": must be [IP:]HOST:GUEST; for example -p 8080:80`},
		{name: "publish port too large", in: runspec.Input{Image: "alpine", Net: true, Publish: []string{"70000:80"}}, want: `-p "70000:80": must be [IP:]HOST:GUEST; for example -p 8080:80`},
		{name: "publish IPv6", in: runspec.Input{Image: "alpine", Net: true, Publish: []string{"[::1]:8080:80"}}, want: `-p "[::1]:8080:80": IPv6 bind addresses are not supported; use an IPv4 address`},
		{name: "publish bad address", in: runspec.Input{Image: "alpine", Net: true, Publish: []string{"host:8080:80"}}, want: `-p "host:8080:80": the bind address must be an IPv4 address; for example 127.0.0.1:8080:80`},
		{name: "publish without net", in: runspec.Input{Image: "alpine", Publish: []string{"8080:80"}}, want: `-p "8080:80": requires --net`},
		{name: "allow-lan without net", in: runspec.Input{Image: "alpine", AllowLAN: true}, want: `--allow-lan: requires --net`},
		{name: "relative workdir", in: runspec.Input{Image: "alpine", Workdir: "app"}, want: `-w "app": must be absolute; for example /app`},
		{name: "control character in workdir", in: runspec.Input{Image: "alpine", Workdir: "/a\x00"}, want: `-w "/a\x00": control character U+0000; only tab and newline are allowed`},
		{name: "user name", in: runspec.Input{Image: "alpine", User: "root"}, want: `-u "root": must be UID or UID:GID with decimal numbers below 4294967295; for example -u 1000:1000`},
		{name: "uid -1", in: runspec.Input{Image: "alpine", User: "4294967295"}, want: `-u "4294967295": must be UID or UID:GID with decimal numbers below 4294967295; for example -u 1000:1000`},
		{name: "gid -1", in: runspec.Input{Image: "alpine", User: "0:4294967295"}, want: `-u "0:4294967295": must be UID or UID:GID with decimal numbers below 4294967295; for example -u 1000:1000`},
		{name: "empty group", in: runspec.Input{Image: "alpine", User: "1000:"}, want: `-u "1000:": must be UID or UID:GID with decimal numbers below 4294967295; for example -u 1000:1000`},
		{name: "control character in entrypoint", in: runspec.Input{Image: "alpine", Entrypoint: new("\x7f")}, want: `--entrypoint "\x7f": control character U+007F; only tab and newline are allowed`},
		{name: "unknown pull policy", in: runspec.Input{Image: "alpine", Pull: "sometimes"}, want: `--pull "sometimes": unknown policy; use missing, always, or never`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := runspec.Parse(tt.in, 4)
			if err == nil {
				t.Fatalf("Parse() error = nil, want %q", tt.want)
			}

			if got := err.Error(); got != tt.want {
				t.Errorf("Parse() error =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestEnvFileMatchesEnvFlag(t *testing.T) {
	t.Parallel()

	entries := []string{"A=1", "A= spaced value ", "A=tab\tinside", "A=", "A==", "A=#not a comment", " A=1", "KRUN_X=1", "HOSTNAME=x", "1A=x", "A=\x1b", "A"}

	for _, kv := range entries {
		t.Run(kv, func(t *testing.T) {
			t.Parallel()

			fromFlag, flagErr := runspec.Parse(runspec.Input{Image: "alpine", Env: []string{kv}}, 1)
			fromFile, fileErr := runspec.Parse(runspec.Input{Image: "alpine", EnvFiles: []runspec.EnvFile{{Path: "a.env", Data: []byte(kv + "\n")}}}, 1)

			if !reflect.DeepEqual(fromFlag.Env, fromFile.Env) {
				t.Errorf("env from -e = %q, from --env-file = %q", fromFlag.Env, fromFile.Env)
			}

			if problem(flagErr) != problem(fileErr) {
				t.Errorf("error from -e = %v, from --env-file = %v", flagErr, fileErr)
			}
		})
	}
}

func TestEnvFileSkipsBlankAndCommentLinesAndCR(t *testing.T) {
	t.Parallel()

	data := "# comment\r\n  # indented comment\n \t\nA=1\r\nB=two words \n"

	got, err := runspec.Parse(runspec.Input{Image: "alpine", EnvFiles: []runspec.EnvFile{{Path: "a.env", Data: []byte(data)}}}, 1)
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"A=1", "B=two words "}; !reflect.DeepEqual(got.Env, want) {
		t.Errorf("Env = %q, want %q", got.Env, want)
	}
}

// problem returns the error without its source, so that -e and --env-file errors compare equal.
func problem(err error) string {
	if e, ok := errors.AsType[*runspec.Error](err); ok {
		return e.Value + ": " + e.Problem + "; " + e.Fix
	}

	return ""
}
