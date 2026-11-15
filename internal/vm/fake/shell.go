package fake

import (
	"bufio"
	"errors"
	"slices"
	"strconv"
	"strings"
)

var errUnterminated = errors.New("unterminated quoted string")

// sh runs `sh -c SCRIPT` or reads commands from stdin, one line at a time.
// It supports words with quotes and backslashes, $NAME, ${NAME}, and $?, commands separated by ; or newlines,
// and the builtin exit.
func (g *guest) sh(args []string, stdio Stdio) int {
	if len(args) >= 2 && args[0] == "-c" {
		status, _ := g.script(args[1], stdio)

		return status
	}

	if len(args) > 0 {
		sayln(stdio.Err, "sh: only -c SCRIPT or commands on stdin are supported")

		return 2
	}

	lines := bufio.NewScanner(stdio.In)
	for lines.Scan() {
		status, exited := g.script(lines.Text(), stdio)
		if exited {
			return status
		}
	}

	return g.status
}

// script runs each command in s. It reports whether the builtin exit ended the shell.
func (g *guest) script(s string, stdio Stdio) (int, bool) {
	commands, err := splitCommands(s)
	if err != nil {
		sayf(stdio.Err, "sh: syntax error: %v\n", err)

		return 2, true
	}

	for _, command := range commands {
		words, err := g.words(command)
		if err != nil {
			sayf(stdio.Err, "sh: syntax error: %v\n", err)

			return 2, true
		}

		if len(words) == 0 {
			continue
		}

		if words[0] == "exit" {
			return g.exit(words[1:], stdio), true
		}

		g.status = g.command(words, stdio)
	}

	return g.status, false
}

func (g *guest) exit(args []string, stdio Stdio) int {
	if len(args) == 0 {
		return g.status
	}

	n, err := strconv.Atoi(args[0])
	if err != nil {
		sayf(stdio.Err, "sh: exit: Illegal number: %s\n", args[0])

		return 2
	}

	return n & 0xff
}

func (g *guest) command(words []string, stdio Stdio) int {
	applet, err := g.resolve(words[0])

	switch {
	case errors.Is(err, errNotFound):
		sayf(stdio.Err, "sh: %s: not found\n", words[0])
	case err != nil:
		sayf(stdio.Err, "sh: %s: %v\n", words[0], err)
	default:
		return g.run(applet, words[1:], stdio)
	}

	return exitCode(err)
}

// splitCommands splits s at ; and newlines outside quotes.
func splitCommands(s string) ([]string, error) {
	var (
		commands []string
		quote    rune
		escaped  bool
		start    int
	)

	for i, c := range s {
		switch {
		case escaped:
			escaped = false
		case c == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ';' || c == '\n':
			commands = append(commands, s[start:i])
			start = i + 1
		}
	}

	if quote != 0 {
		return nil, errUnterminated
	}

	return append(commands, s[start:]), nil
}

// words splits one command into words and expands variables.
func (g *guest) words(command string) ([]string, error) {
	var (
		words  []string
		word   strings.Builder
		inWord bool
		quote  rune
	)

	rs := []rune(command)

	for i := 0; i < len(rs); i++ {
		c := rs[i]

		switch {
		case quote == '\'' && c != '\'':
			word.WriteRune(c)
		case c == '\'' && quote != '"' || c == '"' && quote != '\'':
			inWord = true

			if quote == 0 {
				quote = c
			} else {
				quote = 0
			}
		case c == '\\' && i+1 < len(rs) && (quote == 0 || strings.ContainsRune(`"\$`, rs[i+1])):
			i++

			word.WriteRune(rs[i])

			inWord = true
		case c == '$':
			value, n := g.expand(rs[i+1:])
			word.WriteString(value)

			i += n
			inWord = true
		case quote == 0 && (c == ' ' || c == '\t'):
			if inWord {
				words = append(words, word.String())
			}

			word.Reset()

			inWord = false
		default:
			word.WriteRune(c)

			inWord = true
		}
	}

	if quote != 0 {
		return nil, errUnterminated
	}

	if inWord {
		words = append(words, word.String())
	}

	return words, nil
}

// expand reads a variable reference after $ and returns its value and the number of runes read.
func (g *guest) expand(rs []rune) (string, int) {
	if len(rs) > 0 && rs[0] == '?' {
		return strconv.Itoa(g.status), 1
	}

	if len(rs) > 0 && rs[0] == '{' {
		end := slices.Index(rs, '}')
		if end > 0 {
			return g.getenv(string(rs[1:end])), end + 1
		}
	}

	n := 0
	for n < len(rs) && (rs[n] == '_' || 'a' <= rs[n] && rs[n] <= 'z' || 'A' <= rs[n] && rs[n] <= 'Z' || n > 0 && '0' <= rs[n] && rs[n] <= '9') {
		n++
	}

	if n == 0 {
		return "$", 0
	}

	return g.getenv(string(rs[:n])), n
}
