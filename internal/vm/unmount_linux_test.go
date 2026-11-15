package vm

import "testing"

func TestUnescapeMountPath(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		`/plain`:              "/plain",
		`/a\040b`:             "/a b",
		`/tab\011new\012line`: "/tab\tnew\nline",
		`/back\134slash`:      `/back\slash`,
		`/short\04`:           `/short\04`,
		`/not\089octal`:       `/not\089octal`,
		`/runs/1\040x/root`:   "/runs/1 x/root",
	}

	for in, want := range tests {
		if got := unescapeMountPath(in); got != want {
			t.Errorf("unescapeMountPath(%q) = %q, want %q", in, got, want)
		}
	}
}
