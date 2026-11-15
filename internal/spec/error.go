package spec

import (
	"fmt"
	"strings"
)

// Error is a user-facing problem with one input value.
//
// Its message has the form `<source> "<value>": <problem>; <fix>`.
// The value and the fix are optional.
type Error struct {
	Source  string
	Value   string
	Problem string
	Fix     string
}

func (e *Error) Error() string {
	var b strings.Builder

	b.WriteString(e.Source)

	if e.Value != "" {
		fmt.Fprintf(&b, " %q", e.Value)
	}

	b.WriteString(": ")
	b.WriteString(e.Problem)

	if e.Fix != "" {
		b.WriteString("; ")
		b.WriteString(e.Fix)
	}

	return b.String()
}
