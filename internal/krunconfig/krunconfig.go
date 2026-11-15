// Package krunconfig writes /.krun_config.json, the workload config that the libkrun guest init reads.
//
// The guest init parser treats any string as a possible key, takes the first match, and ignores the whole file above
// MaxTokens tokens. Marshal therefore writes a fixed key order and rejects every value the parser would misread.
package krunconfig

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// FileName is the config path inside the guest root, relative to the root.
const FileName = ".krun_config.json"

// MaxTokens is the token limit of the guest init parser.
// Above it the guest init ignores the file and starts a root shell.
const MaxTokens = 16384

// fixedTokens counts the object, the six keys, the uid, gid, and WorkingDir strings, and the three arrays.
const fixedTokens = 13

// ErrReserved marks env names that the guest init reads as control settings.
var ErrReserved = errors.New("reserved by the VM init")

// Config is the workload as the guest init sees it.
type Config struct {
	UID, GID   uint32
	WorkingDir string
	Env        []string // NAME=value entries, final merged list, PATH included
	Args       []string // argv, Args[0] is the command
}

// Error describes an invalid value.
// The caller adds the source and the value.
type Error struct {
	Problem string
	Fix     string
	cause   error
}

func (e *Error) Error() string {
	if e.Fix == "" {
		return e.Problem
	}

	return e.Problem + "; " + e.Fix
}

func (e *Error) Unwrap() error {
	return e.cause
}

func invalid(problem, fix string) *Error {
	return &Error{Problem: problem, Fix: fix, cause: nil}
}

// CheckValue reports invalid UTF-8 and control characters other than tab and newline.
func CheckValue(s string) error {
	if !utf8.ValidString(s) {
		return invalid("invalid UTF-8", "use UTF-8 text")
	}

	for _, r := range s {
		if unicode.IsControl(r) && r != '\t' && r != '\n' {
			return invalid(fmt.Sprintf("control character %U", r), "only tab and newline are allowed")
		}
	}

	return nil
}

// CheckEnvName reports an env name that is not an identifier or that the guest init reserves.
// Reserved names yield an error that matches ErrReserved.
func CheckEnvName(name string) error {
	if !isIdentifier(name) {
		return invalid("not a valid variable name", "use letters, digits, and underscores, and start with a letter or underscore")
	}

	if strings.HasPrefix(name, "KRUN_") {
		return &Error{Problem: "KRUN_* names are reserved by the VM init", Fix: "rename the variable", cause: ErrReserved}
	}

	if name == "HOSTNAME" {
		return &Error{Problem: "HOSTNAME is reserved by the VM init", Fix: "rename the variable", cause: ErrReserved}
	}

	return nil
}

func isIdentifier(name string) bool {
	if name == "" {
		return false
	}

	for i, c := range []byte(name) {
		letter := c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
		digit := '0' <= c && c <= '9'

		if !letter && (i == 0 || !digit) {
			return false
		}
	}

	return true
}

// Marshal validates c against the guest init contract and returns the file content.
func Marshal(c Config) ([]byte, error) {
	err := validate(c)
	if err != nil {
		return nil, err
	}

	return encode(c), nil
}

func validate(c Config) error {
	// setuid and setgid reject (uid_t)-1, and the guest init then exits with 125.
	if c.UID == math.MaxUint32 || c.GID == math.MaxUint32 {
		return invalid("user or group ID 4294967295 is not valid", "use a lower ID")
	}

	if err := CheckValue(c.WorkingDir); err != nil {
		return fmt.Errorf("working directory %q: %w", c.WorkingDir, err)
	}

	if !path.IsAbs(c.WorkingDir) {
		return fmt.Errorf("working directory %q: %w", c.WorkingDir, invalid("not an absolute path", "start it with /"))
	}

	if err := validateEnv(c.Env); err != nil {
		return err
	}

	if len(c.Args) == 0 {
		return invalid("no command", "set a command or an image entrypoint")
	}

	for i, arg := range c.Args {
		if err := CheckValue(arg); err != nil {
			return fmt.Errorf("argument %d %q: %w", i, arg, err)
		}
	}

	if n := fixedTokens + len(c.Env) + len(c.Args); n > MaxTokens {
		limit := strconv.Itoa(MaxTokens - fixedTokens)

		return invalid("too many arguments and env entries", "use at most "+limit+" in total")
	}

	return nil
}

func validateEnv(env []string) error {
	seen := make(map[string]bool, len(env))

	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			return fmt.Errorf("env %q: %w", entry, invalid("missing =", "use NAME=value"))
		}

		if err := CheckEnvName(name); err != nil {
			return fmt.Errorf("env %q: %w", entry, err)
		}

		if err := CheckValue(value); err != nil {
			return fmt.Errorf("env %q: %w", entry, err)
		}

		// The guest init keeps the first value of a name, except for HOME and TERM.
		if seen[name] {
			return fmt.Errorf("env %q: %w", entry, invalid("duplicate name", "set each name once"))
		}

		seen[name] = true
	}

	if !seen["PATH"] {
		return invalid("PATH is not set", "set PATH in the image or the env")
	}

	return nil
}

// encode writes the keys in the contract order without validation.
func encode(c Config) []byte {
	var b strings.Builder

	b.WriteString(`{"uid":"`)
	b.WriteString(strconv.FormatUint(uint64(c.UID), 10))
	b.WriteString(`","gid":"`)
	b.WriteString(strconv.FormatUint(uint64(c.GID), 10))
	b.WriteString(`","WorkingDir":`)
	writeString(&b, c.WorkingDir)
	b.WriteString(`,"Env":`)
	writeArray(&b, c.Env)
	b.WriteString(`,"Entrypoint":[],"args":`)
	writeArray(&b, c.Args)
	b.WriteString("}\n")

	return []byte(b.String())
}

func writeArray(b *strings.Builder, items []string) {
	b.WriteByte('[')

	for i, s := range items {
		if i > 0 {
			b.WriteByte(',')
		}

		writeString(b, s)
	}

	b.WriteByte(']')
}

// writeString never writes \u escapes, because the guest init corrupts them.
func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')

	for i := range len(s) {
		switch c := s[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}

	b.WriteByte('"')
}

// Write replaces FileName in root with the config for c.
// An existing entry, such as a symlink or directory from the image, is removed first.
// The new file is created exclusively, so it never follows a symlink.
func Write(root *os.Root, c Config) error {
	data, err := Marshal(c)
	if err != nil {
		return err
	}

	err = root.RemoveAll(FileName)
	if err != nil {
		return fmt.Errorf("remove old config: %w", err)
	}

	f, err := root.OpenFile(FileName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create config: %w", err)
	}

	_, err = f.Write(data)
	closeErr := f.Close()

	if err = errors.Join(err, closeErr); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}
