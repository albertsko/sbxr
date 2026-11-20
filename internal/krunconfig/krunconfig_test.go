package krunconfig

import (
	"encoding/json"
	"errors"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

const badName = "not a valid variable name"

// configTokens counts the fixed tokens of a config file: the object, 6 keys, 3 strings, and 3 arrays.
const configTokens = 13

func validConfig() Config {
	return Config{
		UID:        1000,
		GID:        1000,
		WorkingDir: "/home/app",
		Env:        []string{"PATH=/usr/bin:/bin", "HOME=/home/app"},
		Args:       []string{"sh", "-c", "echo hi"},
	}
}

func TestMarshalFormat(t *testing.T) {
	t.Parallel()

	c := Config{
		UID:        1000,
		GID:        0,
		WorkingDir: "/a b",
		Env:        []string{"PATH=/bin", "Q=\"x\\y\tz\n"},
		Args:       []string{"sh", "é", `\u00e9`},
	}

	got, err := Marshal(c)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	want := `{"uid":"1000","gid":"0","WorkingDir":"/a b","Env":["PATH=/bin","Q=\"x\\y\tz\n"],` +
		`"Entrypoint":[],"args":["sh","é","\\u00e9"]}` + "\n"
	if string(got) != want {
		t.Errorf("Marshal() = %s, want %s", got, want)
	}
}

func TestMarshalRejects(t *testing.T) {
	t.Parallel()

	tooMany := make([]string, jsmnMaxTokens-configTokens-len(validConfig().Env)+1)
	for i := range tooMany {
		tooMany[i] = "x"
	}

	tests := []struct {
		name   string
		modify func(c *Config)
		want   string
	}{
		{name: "max uid", modify: func(c *Config) { c.UID = 1<<32 - 1 }, want: "user or group ID 4294967295"},
		{name: "max gid", modify: func(c *Config) { c.GID = 1<<32 - 1 }, want: "user or group ID 4294967295"},
		{name: "empty workdir", modify: func(c *Config) { c.WorkingDir = "" }, want: "not an absolute path"},
		{name: "relative workdir", modify: func(c *Config) { c.WorkingDir = "app" }, want: "not an absolute path"},
		{name: "control in workdir", modify: func(c *Config) { c.WorkingDir = "/a\rb" }, want: "control character U+000D"},
		{name: "env without =", modify: func(c *Config) { c.Env = append(c.Env, "FOO") }, want: "missing ="},
		{name: "empty env name", modify: func(c *Config) { c.Env = append(c.Env, "=x") }, want: badName},
		{name: "env name with digit first", modify: func(c *Config) { c.Env = append(c.Env, "1A=x") }, want: badName},
		{name: "env name with dash", modify: func(c *Config) { c.Env = append(c.Env, "A-B=x") }, want: badName},
		{name: "env name with space", modify: func(c *Config) { c.Env = append(c.Env, " A=x") }, want: badName},
		{name: "reserved KRUN_INIT", modify: func(c *Config) { c.Env = append(c.Env, "KRUN_INIT=/x") }, want: "KRUN_* names are reserved"},
		{name: "KRUN_WORKDIR", modify: func(c *Config) { c.Env = append(c.Env, "KRUN_WORKDIR=/") }, want: "KRUN_* names are reserved"},
		{name: "reserved HOSTNAME", modify: func(c *Config) { c.Env = append(c.Env, "HOSTNAME=h") }, want: "HOSTNAME is reserved"},
		{name: "duplicate env", modify: func(c *Config) { c.Env = append(c.Env, "PATH=/x") }, want: "duplicate name"},
		{name: "control in env value", modify: func(c *Config) { c.Env = append(c.Env, "A=\x1b") }, want: "control character U+001B"},
		{name: "NUL in env value", modify: func(c *Config) { c.Env = append(c.Env, "A=\x00") }, want: "control character U+0000"},
		{name: "invalid UTF-8 in env", modify: func(c *Config) { c.Env = append(c.Env, "A=\xff") }, want: "invalid UTF-8"},
		{name: "no PATH", modify: func(c *Config) { c.Env = []string{"HOME=/"} }, want: "PATH is not set"},
		{name: "no args", modify: func(c *Config) { c.Args = nil }, want: "no command"},
		{name: "control in arg", modify: func(c *Config) { c.Args = []string{"a\x1bb"} }, want: `argument 0 "a\x1bb": control character U+001B`},
		{name: "DEL in arg", modify: func(c *Config) { c.Args = []string{"\x7f"} }, want: "control character U+007F"},
		{name: "C1 control in arg", modify: func(c *Config) { c.Args = []string{"\u0085"} }, want: "control character U+0085"},
		{name: "invalid UTF-8 in arg", modify: func(c *Config) { c.Args = []string{"\xc3"} }, want: "invalid UTF-8"},
		{name: "too many tokens", modify: func(c *Config) { c.Args = tooMany }, want: "too many arguments and env entries"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := validConfig()
			tt.modify(&c)

			_, err := Marshal(c)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Marshal() error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestTokenLimit(t *testing.T) {
	t.Parallel()

	c := validConfig()
	c.Args = make([]string, jsmnMaxTokens-configTokens-len(c.Env))

	for i := range c.Args {
		c.Args[i] = "x"
	}

	data, err := Marshal(c)
	if err != nil {
		t.Fatalf("Marshal() at the limit error = %v", err)
	}

	if n := newJsmnParser().parse(data, make([]jsmnTok, 2*jsmnMaxTokens)); n != jsmnMaxTokens {
		t.Fatalf("config at the writer limit has %d tokens, want %d", n, jsmnMaxTokens)
	}

	if res := guestInit(data); !res.parsed || len(res.argv) != len(c.Args) {
		t.Fatalf("guest init at the limit: parsed = %v, %d args, want %d", res.parsed, len(res.argv), len(c.Args))
	}

	c.Args = append(c.Args, "x")

	if _, err = Marshal(c); err == nil {
		t.Fatal("Marshal() above the limit succeeded")
	}

	res := guestInit(encode(c))
	if res.parsed || res.uid != 0 || !slices.Equal(res.argv, []string{defaultInit}) {
		t.Errorf("guest init above the limit = %+v, want a root shell", res)
	}
}

func TestCheckValue(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", "plain", "tab\there", "line\nbreak", "é日本", "\uFFFD", `"\`} {
		if err := CheckValue(s); err != nil {
			t.Errorf("CheckValue(%q) error = %v", s, err)
		}
	}

	err := CheckValue("a\x1bb")
	if got, want := err.Error(), "control character U+001B; only tab and newline are allowed"; got != want {
		t.Errorf("CheckValue() error = %q, want %q", got, want)
	}
}

func TestCheckEnvName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		reserved bool
		problem  string
	}{
		{name: "PATH"},
		{name: "_"},
		{name: "a1_B"},
		{name: "krun_init"},
		{name: "KRUN", problem: ""},
		{name: "KRUN_", reserved: true, problem: "KRUN_* names are reserved by the VM init"},
		{name: "KRUN_INIT", reserved: true, problem: "KRUN_* names are reserved by the VM init"},
		{name: "HOSTNAME", reserved: true, problem: "HOSTNAME is reserved by the VM init"},
		{name: "", problem: badName},
		{name: "1A", problem: badName},
		{name: "A.B", problem: badName},
		{name: "É", problem: badName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := CheckEnvName(tt.name)
			if tt.problem == "" {
				if err != nil {
					t.Fatalf("CheckEnvName(%q) error = %v", tt.name, err)
				}

				return
			}

			e, ok := errors.AsType[*Error](err)
			if !ok || e.Problem != tt.problem || e.Fix == "" {
				t.Fatalf("CheckEnvName(%q) error = %#v, want problem %q and a fix", tt.name, err, tt.problem)
			}

			if got := errors.Is(err, ErrReserved); got != tt.reserved {
				t.Errorf("errors.Is(CheckEnvName(%q), ErrReserved) = %v, want %v", tt.name, got, tt.reserved)
			}
		})
	}
}

// TestGuestInitQuirks pins the guest init behavior that the writer contract guards against.
func TestGuestInitQuirks(t *testing.T) {
	t.Parallel()

	rootShell := []string{defaultInit}

	tests := []struct {
		name  string
		file  string
		check func(r guestResult) bool
	}{
		{name: "keys ignore case", file: `{"ARGS":["x"],"UID":"7"}`, check: func(r guestResult) bool {
			return slices.Equal(r.argv, []string{"x"}) && r.uid == 7
		}},
		{name: "value matched as key", file: `{"a":"uid","b":"7","uid":"1000"}`, check: func(r guestResult) bool {
			return r.idFailed
		}},
		{name: "first match wins", file: `{"uid":"1","uid":"2"}`, check: func(r guestResult) bool { return r.uid == 1 }},
		{name: "empty uid runs as root", file: `{"uid":"","args":["id"]}`, check: func(r guestResult) bool {
			return r.uid == 0 && !r.idFailed
		}},
		{name: "Cmd and Cwd aliases", file: `{"Cmd":["c"],"Cwd":"/w"}`, check: func(r guestResult) bool {
			return slices.Equal(r.argv, []string{"c"}) && r.cwd == "/w"
		}},
		{name: "non-ASCII \\u escape starts a root shell", file: `{"uid":"1000","args":["\u00e9"]}`, check: func(r guestResult) bool {
			return !r.parsed && r.uid == 0 && slices.Equal(r.argv, rootShell)
		}},
		{name: "ASCII \\u escape decodes by accident", file: `{"args":["a\u0041b"]}`, check: func(r guestResult) bool {
			return slices.Equal(r.argv, []string{"aAb"})
		}},
		{name: "KRUN_INIT replaces the command", file: `{"Env":["KRUN_INIT=/x"],"args":["sh"]}`, check: func(r guestResult) bool {
			return slices.Equal(r.argv, []string{"/x"})
		}},
		{name: "KRUN_WORKDIR overrides WorkingDir", file: `{"WorkingDir":"/a","Env":["KRUN_WORKDIR=/b"]}`, check: func(r guestResult) bool {
			return r.cwd == "/b"
		}},
		{name: "HOSTNAME sets the host name", file: `{"Env":["HOSTNAME=h"]}`, check: func(r guestResult) bool { return r.hostname == "h" }},
		{name: "first duplicate wins", file: `{"Env":["A=1","A=2"]}`, check: func(r guestResult) bool {
			return slices.Contains(r.env, "A=1") && !slices.Contains(r.env, "A=2")
		}},
		{name: "last HOME wins", file: `{"Env":["HOME=/1","HOME=/2"]}`, check: func(r guestResult) bool {
			return slices.Contains(r.env, "HOME=/2")
		}},
		{name: "uid 4294967295 fails", file: `{"uid":"4294967295"}`, check: func(r guestResult) bool { return r.idFailed }},
		{name: "invalid JSON starts a root shell", file: `{"args":["x"`, check: func(r guestResult) bool {
			return !r.parsed && slices.Equal(r.argv, rootShell)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if r := guestInit([]byte(tt.file)); !tt.check(r) {
				t.Errorf("guestInit(%s) = %+v", tt.file, r)
			}
		})
	}
}

var (
	forbiddenControl = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f\x{80}-\x{9f}]`)
	envEntry         = regexp.MustCompile(`(?s)^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	reservedName     = regexp.MustCompile(`^(KRUN_.*|HOSTNAME)$`)
)

// meetsContract restates the guest init config contract independently of the package code.
func meetsContract(c Config) bool {
	okValue := func(s string) bool { return utf8.ValidString(s) && !forbiddenControl.MatchString(s) }

	if c.UID == 1<<32-1 || c.GID == 1<<32-1 || !strings.HasPrefix(c.WorkingDir, "/") || !okValue(c.WorkingDir) {
		return false
	}

	names := map[string]bool{}

	for _, e := range c.Env {
		m := envEntry.FindStringSubmatch(e)
		if len(m) != 3 || reservedName.MatchString(m[1]) || names[m[1]] || !okValue(m[2]) {
			return false
		}

		names[m[1]] = true
	}

	if !names["PATH"] || len(c.Args) == 0 || configTokens+len(c.Env)+len(c.Args) > jsmnMaxTokens {
		return false
	}

	return !slices.ContainsFunc(c.Args, func(a string) bool { return !okValue(a) })
}

// checkMarshal asserts that Marshal accepts exactly the contract and that the guest init reads back c.
func checkMarshal(t *testing.T, c Config) {
	t.Helper()

	data, err := Marshal(c)

	if want := meetsContract(c); (err == nil) != want {
		t.Fatalf("Marshal(%#v) error = %v, contract met = %v", c, err, want)
	}

	if err != nil {
		return
	}

	var decoded struct {
		UID, GID   string
		WorkingDir string
		Env, Args  []string
	}

	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Marshal(%#v) = %s, not JSON: %v", c, data, err)
	}

	if decoded.WorkingDir != c.WorkingDir || !slices.Equal(decoded.Env, c.Env) || !slices.Equal(decoded.Args, c.Args) {
		t.Fatalf("encoding/json reads %s as %+v, want %#v", data, decoded, c)
	}

	res := guestInit(data)
	if !res.parsed || res.idFailed || res.uid != c.UID || res.gid != c.GID || res.cwd != c.WorkingDir {
		t.Fatalf("guest init of %s = %+v, want %#v", data, res, c)
	}

	if !slices.Equal(res.argv, c.Args) {
		t.Fatalf("guest init argv = %q, want %q", res.argv, c.Args)
	}

	want := envMap(kernelEnv)
	maps.Copy(want, envMap(c.Env))

	if got := envMap(res.env); !maps.Equal(got, want) {
		t.Fatalf("guest init env = %q, want %q", got, want)
	}

	if res.hostname != "localhost" || res.rlimits != "" || res.pid1 {
		t.Fatalf("guest init of %s = %+v, want default init settings", data, res)
	}
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))

	for _, e := range env {
		name, value, _ := strings.Cut(e, "=")
		m[name] = value
	}

	return m
}

// fragments are the pieces that random strings are built from.
var fragments = []string{
	"a", "Z", "_", "0", "9", " ", "=", "/", ":", ",", "[", "]", "{", "}", `"`, `\`, `\"`, `\\`, `\u00e9`, `\u0041`,
	"\t", "\n", "\r", "\x00", "\x1b", "\x7f", "\u0085", "\xff", "é", "日本", "😀",
	"uid", "gid", "Env", "env", "ARGS", "args", "Cmd", "Cwd", "WorkingDir", "Entrypoint",
	"PATH", "HOME", "TERM", "HOSTNAME", "KRUN_INIT", "KRUN_", "krun_init",
}

func randomString(r *rand.Rand) string {
	var b strings.Builder

	for range r.IntN(6) {
		b.WriteString(fragments[r.IntN(len(fragments))])
	}

	return b.String()
}

func randomConfig(r *rand.Rand) Config {
	pick := func(valid, random string) string {
		if r.IntN(8) == 0 {
			return random
		}

		return valid
	}

	ids := []uint32{0, 1, 1000, 65534, 1<<32 - 2, 1<<32 - 1}
	c := Config{
		UID:        ids[r.IntN(len(ids))],
		GID:        ids[r.IntN(len(ids))],
		WorkingDir: pick("/"+randomString(r), randomString(r)),
		Env:        []string{pick("PATH=/bin", randomString(r))},
		Args:       nil,
	}

	for range r.IntN(5) {
		name := pick("V"+string(rune('A'+r.IntN(26))), randomString(r))
		c.Env = append(c.Env, pick(name+"="+randomString(r), randomString(r)))
	}

	for range r.IntN(5) {
		c.Args = append(c.Args, randomString(r))
	}

	return c
}

func TestMarshalProperty(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(1, 2))

	for range 20000 {
		checkMarshal(t, randomConfig(r))
	}
}

// listSep splits fuzz strings into lists. NUL inside values is covered by TestMarshalRejects.
const listSep = "\x00"

func splitList(s string) []string {
	if s == "" {
		return nil
	}

	return strings.Split(s, listSep)
}

// FuzzMarshal checks Marshal against the guest init parser copy.
// The seed corpus is in testdata/fuzz/FuzzMarshal. The vm tests of internal/libkrun also boot it through the real guest init.
func FuzzMarshal(f *testing.F) {
	f.Fuzz(func(t *testing.T, uid, gid uint32, workdir, env, args string) {
		checkMarshal(t, Config{UID: uid, GID: gid, WorkingDir: workdir, Env: splitList(env), Args: splitList(args)})
	})
}

func TestWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		plant func(t *testing.T, root, outside string)
	}{
		{name: "no file", plant: func(*testing.T, string, string) {}},
		{name: "regular file", plant: func(t *testing.T, root, _ string) {
			t.Helper()
			writeFile(t, filepath.Join(root, FileName), "{}")
		}},
		{name: "symlink to outside file", plant: func(t *testing.T, root, _ string) {
			t.Helper()
			symlink(t, "../outside/file", filepath.Join(root, FileName))
		}},
		{name: "symlink to outside directory", plant: func(t *testing.T, root, _ string) {
			t.Helper()
			symlink(t, "../outside", filepath.Join(root, FileName))
		}},
		{name: "absolute symlink", plant: func(t *testing.T, root, outside string) {
			t.Helper()
			symlink(t, filepath.Join(outside, "file"), filepath.Join(root, FileName))
		}},
		{name: "dangling symlink", plant: func(t *testing.T, root, _ string) {
			t.Helper()
			symlink(t, "../outside/new", filepath.Join(root, FileName))
		}},
		{name: "directory", plant: func(t *testing.T, root, _ string) {
			t.Helper()
			writeFile(t, filepath.Join(root, FileName, "inner"), "x")
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			rootDir := filepath.Join(dir, "root")
			outside := filepath.Join(dir, "outside")

			writeFile(t, filepath.Join(outside, "file"), "keep")
			mkdir(t, rootDir)
			tt.plant(t, rootDir, outside)

			before := snapshot(t, outside)

			root, err := os.OpenRoot(rootDir)
			if err != nil {
				t.Fatal(err)
			}

			defer root.Close()

			c := validConfig()
			if err := Write(root, c); err != nil {
				t.Fatalf("Write() error = %v", err)
			}

			if after := snapshot(t, outside); !maps.Equal(before, after) {
				t.Errorf("outside changed: before %q, after %q", before, after)
			}

			info, err := os.Lstat(filepath.Join(rootDir, FileName))
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
				t.Fatalf("config is not a regular file with mode 0600: %v, %v", info, err)
			}

			want, _ := Marshal(c)
			if got := readFile(t, filepath.Join(rootDir, FileName)); got != string(want) {
				t.Errorf("config = %q, want %q", got, want)
			}
		})
	}
}

func TestWriteRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()

	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}

	defer root.Close()

	if err := Write(root, Config{}); err == nil {
		t.Fatal("Write(Config{}) succeeded")
	}

	if _, err := os.Lstat(filepath.Join(rootDir, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config exists after a failed Write: %v", err)
	}
}

// snapshot maps each file under dir to its content.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()

	files := map[string]string{}

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		files[path] = readFile(t, path)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return files
}

func mkdir(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func symlink(t *testing.T, target, link string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
