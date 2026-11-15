package krunconfig

// This file ports the libkrun v1.19.8 guest init config parser to Go, for tests only.
// Sources, tag v1.19.8 of https://github.com/libkrun/libkrun:
//   - src/init_blob/init/jsmn.h, built without JSMN_STRICT and JSMN_PARENT_LINKS.
//   - src/init_blob/init/init.c: hexToDigit, Utf32toUtf8, unescape_string, config_parse_env, config_parse_args,
//     config_parse_string, jsoneq, config_parse_file, and the part of main that reads the result.
// The port keeps the C quirks on purpose: case-insensitive keys, values matched as keys, first match wins,
// in-place rewrites of the file buffer, the broken \u decoding, and the root shell fallback.
//
// The jsmn port carries this notice from jsmn.h:
//
//	MIT License
//
//	Copyright (c) 2010 Serge Zaitsev
//
//	Permission is hereby granted, free of charge, to any person obtaining a copy
//	of this software and associated documentation files (the "Software"), to deal
//	in the Software without restriction, including without limitation the rights
//	to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
//	copies of the Software, and to permit persons to whom the Software is
//	furnished to do so, subject to the following conditions:
//
//	The above copyright notice and this permission notice shall be included in
//	all copies or substantial portions of the Software.
//
//	THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
//	IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
//	FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
//	AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
//	LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
//	OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
//	SOFTWARE.
//
// The ports of hexToDigit, Utf32toUtf8, and parts of unescape_string carry this notice from init.c,
// where these functions are taken from libyajl:
//
//	Copyright (c) 2007-2014, Lloyd Hilaiel <me@lloyd.io>
//
//	Permission to use, copy, modify, and/or distribute this software for any
//	purpose with or without fee is hereby granted, provided that the above
//	copyright notice and this permission notice appear in all copies.
//
//	THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
//	WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
//	MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
//	ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
//	WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
//	ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
//	OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

import (
	"bytes"
	"strconv"
	"strings"
)

// jsmnMaxTokens is kept apart from the writer limit, so a wrong writer limit fails the tests.
const jsmnMaxTokens = 16384 // MAX_TOKENS in init.c

type jsmnType int

const (
	jsmnObject    jsmnType = 1 << 0
	jsmnArray     jsmnType = 1 << 1
	jsmnString    jsmnType = 1 << 2
	jsmnPrimitive jsmnType = 1 << 3
)

const (
	jsmnErrorNoMem = -1
	jsmnErrorInval = -2
	jsmnErrorPart  = -3
)

type jsmnTok struct {
	typ        jsmnType
	start, end int
	size       int
}

type jsmnParser struct {
	pos, toknext, toksuper int
}

func newJsmnParser() *jsmnParser {
	return &jsmnParser{pos: 0, toknext: 0, toksuper: -1}
}

func (p *jsmnParser) alloc(tokens []jsmnTok) *jsmnTok {
	if p.toknext >= len(tokens) {
		return nil
	}

	tok := &tokens[p.toknext]
	p.toknext++
	tok.start, tok.end, tok.size = -1, -1, 0

	return tok
}

func fillToken(tok *jsmnTok, typ jsmnType, start, end int) {
	tok.typ, tok.start, tok.end, tok.size = typ, start, end, 0
}

func (p *jsmnParser) parsePrimitive(js []byte, tokens []jsmnTok) int {
	start := p.pos

loop:
	for ; p.pos < len(js) && js[p.pos] != 0; p.pos++ {
		switch js[p.pos] {
		case ':', '\t', '\r', '\n', ' ', ',', ']', '}':
			break loop
		}
	}

	tok := p.alloc(tokens)
	if tok == nil {
		p.pos = start

		return jsmnErrorNoMem
	}

	fillToken(tok, jsmnPrimitive, start, p.pos)
	p.pos--

	return 0
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'A' <= c && c <= 'F' || 'a' <= c && c <= 'f'
}

// parseString rewrites the last digit of a \u escape in js, like the C code.
func (p *jsmnParser) parseString(js []byte, tokens []jsmnTok) int {
	start := p.pos
	p.pos++

	for ; p.pos < len(js) && js[p.pos] != 0; p.pos++ {
		c := js[p.pos]

		if c == '"' {
			tok := p.alloc(tokens)
			if tok == nil {
				p.pos = start

				return jsmnErrorNoMem
			}

			fillToken(tok, jsmnString, start+1, p.pos)

			return 0
		}

		if c != '\\' || p.pos+1 >= len(js) {
			continue
		}

		p.pos++

		switch js[p.pos] {
		case '"', '/', '\\', 'b', 'f', 'r', 'n', 't':
		case 'u':
			// The C code leaves missing digits uninitialized; the port uses NUL.
			var digits []byte

			p.pos++

			for i := 0; i < 4 && p.pos < len(js) && js[p.pos] != 0; i++ {
				if !isHex(js[p.pos]) {
					p.pos = start

					return jsmnErrorInval
				}

				digits = append(digits, js[p.pos])
				p.pos++
			}

			ascii, _ := strconv.ParseInt("0"+string(digits), 16, 64)
			if ascii > 127 {
				p.pos = start

				return jsmnErrorInval
			}

			p.pos--
			js[p.pos] = byte(ascii)
		default:
			p.pos = start

			return jsmnErrorInval
		}
	}

	p.pos = start

	return jsmnErrorPart
}

func (p *jsmnParser) closeContainer(c byte, tokens []jsmnTok) int {
	typ := jsmnArray
	if c == '}' {
		typ = jsmnObject
	}

	i := p.toknext - 1
	for ; i >= 0; i-- {
		tok := &tokens[i]
		if tok.start != -1 && tok.end == -1 {
			if tok.typ != typ {
				return jsmnErrorInval
			}

			p.toksuper = -1
			tok.end = p.pos + 1

			break
		}
	}

	if i == -1 {
		return jsmnErrorInval
	}

	for ; i >= 0; i-- {
		if tokens[i].start != -1 && tokens[i].end == -1 {
			p.toksuper = i

			break
		}
	}

	return 0
}

func (p *jsmnParser) parse(js []byte, tokens []jsmnTok) int {
	count := p.toknext

	for ; p.pos < len(js) && js[p.pos] != 0; p.pos++ {
		switch c := js[p.pos]; c {
		case '{', '[':
			count++

			tok := p.alloc(tokens)
			if tok == nil {
				return jsmnErrorNoMem
			}

			if p.toksuper != -1 {
				tokens[p.toksuper].size++
			}

			tok.typ = jsmnArray
			if c == '{' {
				tok.typ = jsmnObject
			}

			tok.start = p.pos
			p.toksuper = p.toknext - 1
		case '}', ']':
			if r := p.closeContainer(c, tokens); r < 0 {
				return r
			}
		case '"':
			if r := p.parseString(js, tokens); r < 0 {
				return r
			}

			count++

			if p.toksuper != -1 {
				tokens[p.toksuper].size++
			}
		case '\t', '\r', '\n', ' ':
		case ':':
			p.toksuper = p.toknext - 1
		case ',':
			if p.toksuper == -1 || tokens[p.toksuper].typ == jsmnArray || tokens[p.toksuper].typ == jsmnObject {
				continue
			}

			for i := p.toknext - 1; i >= 0; i-- {
				open := tokens[i].start != -1 && tokens[i].end == -1
				if (tokens[i].typ == jsmnArray || tokens[i].typ == jsmnObject) && open {
					p.toksuper = i

					break
				}
			}
		default:
			if r := p.parsePrimitive(js, tokens); r < 0 {
				return r
			}

			count++

			if p.toksuper != -1 {
				tokens[p.toksuper].size++
			}
		}
	}

	for i := p.toknext - 1; i >= 0; i-- {
		if tokens[i].start != -1 && tokens[i].end == -1 {
			return jsmnErrorPart
		}
	}

	return count
}

// byteAt reads past the end as NUL, like the terminated C buffers.
func byteAt(s []byte, i int) byte {
	if i < len(s) {
		return s[i]
	}

	return 0
}

func cString(s []byte) string {
	before, _, _ := bytes.Cut(s, []byte{0})

	return string(before)
}

func hexToDigit(val *uint32, s []byte, at int) {
	for i := range 4 {
		c := byteAt(s, at+i)
		if c >= 'A' {
			c = (c &^ 0x20) - 7
		}

		c -= '0'
		*val = *val<<4 | uint32(c)
	}
}

func utf32ToUTF8(cp uint32) []byte {
	switch {
	case cp < 0x80:
		return []byte{byte(cp)}
	case cp < 0x800:
		return []byte{byte(cp>>6 | 0xC0), byte(cp&0x3F | 0x80)}
	case cp < 0x10000:
		return []byte{byte(cp>>12 | 0xE0), byte(cp>>6&0x3F | 0x80), byte(cp&0x3F | 0x80)}
	case cp < 0x200000:
		return []byte{byte(cp>>18 | 0xF0), byte(cp>>12&0x3F | 0x80), byte(cp>>6&0x3F | 0x80), byte(cp&0x3F | 0x80)}
	default:
		return []byte{'?'}
	}
}

var simpleEscapes = map[byte]byte{'n': '\n', 't': '\t', 'r': '\r', 'b': '\b', 'f': '\f', '\\': '\\', '"': '"', '/': '/'}

// unescapeString decodes s[:n] in place and writes a NUL terminator.
// The \u branch reads its digits from the 'u' and never advances the output index, like the C code.
func unescapeString(s []byte, n int) {
	i, v := 0, 0

	for v < n {
		if s[v] != '\\' {
			s[i] = s[v]
			i++
			v++

			continue
		}

		v++

		c := byteAt(s, v)
		if out, ok := simpleEscapes[c]; ok {
			s[i] = out
			i++
			v++

			continue
		}

		if c != 'u' {
			continue
		}

		var cp uint32

		hexToDigit(&cp, s, v)
		v += 4

		if cp&0xFC00 == 0xD800 {
			v++

			if byteAt(s, v) != '\\' || byteAt(s, v+1) != 'u' {
				continue
			}

			var surrogate uint32

			hexToDigit(&surrogate, s, v+2)
			cp = (cp&0x3F)<<10 | ((cp>>6&0xF)+1)<<16 | surrogate&0x3FF
			v += 5
		}

		if cp == 0 {
			s[i] = 0
			i++

			continue
		}

		copy(s[i:], utf32ToUTF8(cp))
	}

	s[i] = 0
}

func jsoneq(data []byte, tok jsmnTok, key string) bool {
	if tok.typ != jsmnString || tok.end-tok.start != len(key) {
		return false
	}

	// strncasecmp stops at the first NUL.
	for k := range len(key) {
		a, b := lower(data[tok.start+k]), lower(key[k])
		if a != b {
			return false
		}

		if a == 0 {
			return true
		}
	}

	return true
}

func lower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}

	return c
}

// environ models the C environment of the guest init.
type environ struct {
	entries []string
}

func (e *environ) get(name string) (string, bool) {
	for _, entry := range e.entries {
		if n, v, _ := strings.Cut(entry, "="); n == name {
			return v, true
		}
	}

	return "", false
}

func (e *environ) set(name, value string, overwrite bool) {
	if name == "" || strings.Contains(name, "=") {
		return
	}

	for i, entry := range e.entries {
		if n, _, _ := strings.Cut(entry, "="); n == name {
			if overwrite {
				e.entries[i] = name + "=" + value
			}

			return
		}
	}

	e.entries = append(e.entries, name+"="+value)
}

func configParseString(data []byte, tok jsmnTok) *string {
	n := tok.end - tok.start
	if n == 0 {
		return nil
	}

	s := append([]byte(nil), data[tok.start:tok.end]...)
	s = append(s, 0)
	unescapeString(s, n)

	out := cString(s)

	return &out
}

func configParseArgs(data []byte, tokens []jsmnTok, at int) []string {
	argv := make([]string, 0, tokens[at].size)

	for i := range tokens[at].size {
		targ := tokens[at+1+i]
		s := append([]byte(nil), data[targ.start:targ.end]...)
		s = append(s, 0)
		unescapeString(s, targ.end-targ.start)
		argv = append(argv, cString(s))
	}

	if len(argv) == 0 {
		return nil
	}

	return argv
}

// configParseEnv unescapes each entry in the file buffer itself, like the C code.
func configParseEnv(data []byte, tokens []jsmnTok, at int, env *environ) {
	for i := range tokens[at].size {
		tenv := tokens[at+1+i]
		unescapeString(data[tenv.start:], tenv.end-tenv.start)

		entry := cString(data[tenv.start:])

		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}

		data[tenv.end] = 0

		env.set(name, value, name == "HOME" || name == "TERM")
	}
}

type parsedConfig struct {
	argv, entrypoint []string
	workdir          *string
	uid, gid         *string
}

func configParseFile(file []byte, env *environ) (parsedConfig, bool) {
	var cfg parsedConfig

	// The extra NUL keeps in-place writes one past a token at the end of the file inside the buffer.
	data := make([]byte, len(file)+1)
	copy(data, file)

	tokens := make([]jsmnTok, jsmnMaxTokens)

	n := newJsmnParser().parse(data[:len(file)], tokens)
	if n < 1 || tokens[0].typ != jsmnObject {
		return cfg, false
	}

	var parsedEnv, parsedArgs, parsedWorkdir, parsedEntrypoint, parsedUID, parsedGID bool

	for i := 1; i < n && (!parsedEnv || !parsedArgs || !parsedWorkdir || !parsedEntrypoint || !parsedUID || !parsedGID); i++ {
		next := i+1 < n
		match := func(key string) bool { return next && jsoneq(data, tokens[i], key) }

		if !parsedEnv && match("Env") && tokens[i+1].typ == jsmnArray {
			configParseEnv(data, tokens, i+1, env)

			parsedEnv = true
		}

		if !parsedArgs && match("args") {
			cfg.argv = configParseArgs(data, tokens, i+1)
			parsedArgs = true
		}

		if !parsedArgs && match("Cmd") {
			cfg.argv = configParseArgs(data, tokens, i+1)
			parsedArgs = true
		}

		if !parsedWorkdir && match("WorkingDir") {
			cfg.workdir = configParseString(data, tokens[i+1])
			parsedWorkdir = true
		}

		if !parsedWorkdir && match("Cwd") {
			cfg.workdir = configParseString(data, tokens[i+1])
			parsedWorkdir = true
		}

		if !parsedEntrypoint && match("Entrypoint") {
			cfg.entrypoint = configParseArgs(data, tokens, i+1)
			parsedEntrypoint = true
		}

		if !parsedGID && match("gid") {
			cfg.gid = configParseString(data, tokens[i+1])
			parsedGID = true
		}

		if !parsedUID && match("uid") {
			cfg.uid = configParseString(data, tokens[i+1])
			parsedUID = true
		}
	}

	if cfg.argv != nil && cfg.entrypoint != nil {
		cfg.argv = append(cfg.entrypoint, cfg.argv...)
	}

	return cfg, true
}

// guestResult is what the guest init does with a config file.
type guestResult struct {
	parsed   bool
	argv     []string
	cwd      string // empty means no chdir
	env      []string
	hostname string
	rlimits  string
	pid1     bool
	uid, gid uint32
	idFailed bool // the init exits with 125 before exec
}

// defaultInit is DEFAULT_KRUN_INIT, the command that runs when the config has none.
const defaultInit = "/bin/sh"

// kernelEnv is the environment that the Linux kernel gives to init.
var kernelEnv = []string{"HOME=/", "TERM=linux"}

func guestInit(file []byte) guestResult {
	env := &environ{entries: append([]string(nil), kernelEnv...)}
	cfg, parsed := configParseFile(file, env)

	var res guestResult

	res.parsed = parsed

	if v, ok := env.get("KRUN_HOME"); ok {
		env.set("HOME", v, true)
	}

	if v, ok := env.get("KRUN_TERM"); ok {
		env.set("TERM", v, true)
	}

	res.hostname = "localhost"
	if v, ok := env.get("HOSTNAME"); ok {
		res.hostname = v
	}

	res.rlimits, _ = env.get("KRUN_RLIMITS")

	if v, ok := env.get("KRUN_WORKDIR"); ok {
		res.cwd = v
	} else if cfg.workdir != nil {
		res.cwd = *cfg.workdir
	}

	initArgv := []string{"/init"}

	switch v, ok := env.get("KRUN_INIT"); {
	case ok:
		initArgv[0] = v
		res.argv = initArgv
	case cfg.argv != nil:
		res.argv = cfg.argv
	default:
		initArgv[0] = defaultInit
		res.argv = initArgv
	}

	pid1, _ := env.get("KRUN_INIT_PID1")
	res.pid1 = strings.HasPrefix(pid1, "1")
	res.env = env.entries

	ok := true
	if cfg.gid != nil {
		res.gid, ok = setID(*cfg.gid)
	}

	if ok && cfg.uid != nil {
		res.uid, ok = setID(*cfg.uid)
	}

	res.idFailed = !ok

	return res
}

// setID models strtoul plus setuid or setgid, which reject the value (uid_t)-1.
func setID(s string) (uint32, bool) {
	val, ok := strtoul(s)
	if !ok || val >= 1<<32-1 {
		return 0, false
	}

	return uint32(val), true
}

// strtoul models strtoul(s, &end, 10) followed by the check *end == '\0'.
func strtoul(s string) (uint64, bool) {
	rest := strings.TrimLeft(s, " \t\n\v\f\r")

	negative := false
	if rest != "" && (rest[0] == '+' || rest[0] == '-') {
		negative = rest[0] == '-'
		rest = rest[1:]
	}

	digits := len(rest) - len(strings.TrimLeft(rest, "0123456789"))
	if digits == 0 {
		return 0, s == ""
	}

	if digits != len(rest) {
		return 0, false
	}

	val, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return 0, false
	}

	if negative {
		val = -val
	}

	return val, true
}
