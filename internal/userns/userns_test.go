//go:build linux

package userns

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestSubIDs(t *testing.T) {
	t.Parallel()

	uid := strconv.Itoa(os.Getuid())

	tests := []struct {
		name    string
		content string
		want    idRange
		wantErr error
	}{
		{name: "match by uid", content: "other:1:2\n" + uid + ":100000:65536\n", want: idRange{start: 100000, count: 65536}},
		{name: "first match wins", content: uid + ":200000:10\n" + uid + ":100000:65536\n", want: idRange{start: 200000, count: 10}},
		{name: "invalid entry is skipped", content: uid + ":x:10\n" + uid + ":5:6\n", want: idRange{start: 5, count: 6}},
		{name: "no entry", content: "other:1:2\n", wantErr: ErrNoSubIDs},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "subuid")

			err := os.WriteFile(path, []byte(tt.content), 0o600)
			if err != nil {
				t.Fatal(err)
			}

			got, err := subIDs(path)
			if got != tt.want || !errors.Is(err, tt.wantErr) {
				t.Errorf("subIDs() = %+v, %v; want %+v, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}

	_, err := subIDs(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, ErrNoSubIDs) {
		t.Errorf("subIDs(missing file) error = %v, want ErrNoSubIDs", err)
	}
}
