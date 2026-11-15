package fake_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/albertsko/sbxr/internal/image"
	"github.com/albertsko/sbxr/internal/krunconfig"
	"github.com/albertsko/sbxr/internal/vm/fake"
)

const testPath = "PATH=/usr/bin:/bin"

func TestBoot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cfg        krunconfig.Config
		stdin      string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "echo", cfg: config("echo", "hi"), wantStdout: "hi\n"},
		{name: "echo without newline", cfg: config("echo", "-n", "hi"), wantStdout: "hi"},
		{name: "absolute path", cfg: config("/bin/echo", "hi"), wantStdout: "hi\n"},
		{name: "busybox applet", cfg: config("busybox", "echo", "hi"), wantStdout: "hi\n"},
		{name: "exit code", cfg: config("sh", "-c", "exit 3"), wantCode: 3},
		{name: "false", cfg: config("false"), wantCode: 1},
		{name: "missing command", cfg: config("nope"), wantCode: 127, wantStdout: "Couldn't execute 'nope' inside the vm: No such file or directory\n"},
		{name: "file without execute permission", cfg: config("/etc/passwd"), wantCode: 126, wantStdout: "Couldn't execute '/etc/passwd' inside the vm: Permission denied\n"},
		{name: "directory", cfg: config("/bin"), wantCode: 126, wantStdout: "Couldn't execute '/bin' inside the vm: Permission denied\n"},
		{name: "env", cfg: config("env"), wantStdout: "PATH=/usr/bin:/bin\nA=1\n"},
		{name: "pwd in working directory", cfg: withWorkdir(config("pwd"), "/tmp"), wantStdout: "/tmp\n"},
		{name: "pwd in missing working directory", cfg: withWorkdir(config("pwd"), "/nope"), wantStdout: "/\n"},
		{name: "relative path from working directory", cfg: withWorkdir(config("./echo", "hi"), "/bin"), wantStdout: "hi\n"},
		{name: "id", cfg: krunconfig.Config{UID: 1000, GID: 42, WorkingDir: "/", Env: []string{testPath}, Args: []string{"id"}}, wantStdout: "uid=1000 gid=42\n"},
		{name: "id -u", cfg: krunconfig.Config{UID: 1000, GID: 42, WorkingDir: "/", Env: []string{testPath}, Args: []string{"id", "-u"}}, wantStdout: "1000\n"},
		{name: "cat file", cfg: config("cat", "/etc/passwd"), wantStdout: "root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/home:/bin/false\n"},
		{name: "cat stdin", cfg: config("cat"), stdin: "in\n", wantStdout: "in\n"},
		{name: "shell sequence and status", cfg: config("sh", "-c", "false; echo $?; true\necho $A ${A}x"), wantStdout: "1\n1 1x\n"},
		{name: "shell quoting", cfg: config("sh", "-c", `echo 'a  "$A"' "b  \"$A\"" c\ d`), wantStdout: "a  \"$A\" b  \"1\" c d\n"},
		{name: "shell empty word", cfg: config("sh", "-c", `echo '' x`), wantStdout: " x\n"},
		{name: "shell missing command", cfg: config("sh", "-c", "nope; echo $?"), wantStdout: "127\n", wantStderr: "sh: nope: not found\n"},
		{name: "shell syntax error", cfg: config("sh", "-c", "echo 'a"), wantCode: 2, wantStderr: "sh: syntax error: unterminated quoted string\n"},
		{name: "shell reads stdin", cfg: config("sh"), stdin: "echo one\nexit 4\necho two\n", wantCode: 4, wantStdout: "one\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := newRoot(t, tt.cfg)

			var stdout, stderr bytes.Buffer

			code := fake.Boot(root, fake.Stdio{In: strings.NewReader(tt.stdin), Out: &stdout, Err: &stderr})
			if code != tt.wantCode {
				t.Errorf("Boot() = %d, want %d", code, tt.wantCode)
			}

			if got := stdout.String(); got != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tt.wantStdout)
			}

			if got := stderr.String(); got != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", got, tt.wantStderr)
			}
		})
	}
}

func config(args ...string) krunconfig.Config {
	return krunconfig.Config{UID: 0, GID: 0, WorkingDir: "/", Env: []string{testPath, "A=1"}, Args: args}
}

func withWorkdir(c krunconfig.Config, dir string) krunconfig.Config {
	c.WorkingDir = dir

	return c
}

func newRoot(t *testing.T, cfg krunconfig.Config) *os.Root {
	t.Helper()

	img, err := fake.Image()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	err = image.Unpacker{Ownership: image.OwnershipXattr}.Unpack(img, dir)
	if err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { root.Close() })

	err = krunconfig.Write(root, cfg)
	if err != nil {
		t.Fatal(err)
	}

	return root
}
