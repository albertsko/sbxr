package guestmount_test

import (
	"reflect"
	"testing"

	"github.com/albertsko/sbxr/internal/guestmount"
)

func TestEncodeDecode(t *testing.T) {
	t.Parallel()

	in := guestmount.Args{
		UID:        1000,
		GID:        42,
		WorkingDir: "/work",
		Mounts: []guestmount.Mount{
			{Tag: "sbxr0", Path: "/work", ReadOnly: false},
			{Tag: "sbxr1", Path: "/work/a b", ReadOnly: true},
		},
		Argv: []string{"sh", "-c", "echo 1:rw:/x", "--"},
	}

	argv := guestmount.Encode(in)
	if argv[0] != guestmount.Path {
		t.Fatalf("argv[0] = %q, want %q", argv[0], guestmount.Path)
	}

	got, err := guestmount.Decode(argv)
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("Decode(Encode()) = %+v, %v, want %+v", got, err, in)
	}
}

func TestDecodeRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	for _, argv := range [][]string{
		{guestmount.Path},
		{guestmount.Path, "0:0", "/", "0"},
		{guestmount.Path, "0", "/", "0", "true"},
		{guestmount.Path, "0:x", "/", "0", "true"},
		{guestmount.Path, "0:0", "/", "1", "true"},
		{guestmount.Path, "0:0", "/", "1", "tag:xx:/p", "true"},
		{guestmount.Path, "0:0", "/", "-1", "true"},
	} {
		if _, err := guestmount.Decode(argv); err == nil {
			t.Errorf("Decode(%q) succeeded", argv)
		}
	}
}
