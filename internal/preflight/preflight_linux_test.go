package preflight

import (
	"strings"
	"testing"
)

func TestUsernsFix(t *testing.T) {
	t.Parallel()

	installed := usernsState{restricts: true, exe: "/usr/local/bin/sbxr", profileExists: true}

	with := func(change func(*usernsState)) usernsState {
		st := installed
		change(&st)

		return st
	}

	tests := []struct {
		name  string
		state usernsState
		want  string
		not   string
	}{
		{name: "no helpers", state: with(func(s *usernsState) { s.noHelpers = true }), want: "apt-get install -y uidmap"},
		{name: "no subordinate ids", state: with(func(s *usernsState) { s.noSubIDs = true }), want: "usermod --add-subuids"},
		{name: "binary outside the profile paths", state: with(func(s *usernsState) { s.exe = "/home/u/go/bin/sbxr" }), want: "sudo install -m 755 /home/u/go/bin/sbxr /usr/local/bin/sbxr", not: "apparmor_parser"},
		{name: "unknown binary path", state: with(func(s *usernsState) { s.exe = "" }), want: "install sbxr to /usr/local/bin", not: "apparmor_parser"},
		{name: "profile missing", state: with(func(s *usernsState) { s.profileExists = false }), want: "sbxr doctor apparmor-profile | sudo tee /etc/apparmor.d/sbxr"},
		{name: "changed primary group", state: with(func(s *usernsState) { s.primaryGroupDiffers = true }), want: "log in again"},
		{name: "profile not loaded", state: installed, want: "sudo apparmor_parser -r /etc/apparmor.d/sbxr", not: "tee"},
		{name: "no restriction", state: usernsState{exe: "/tmp/sbxr"}, want: "kernel.unprivileged_userns_clone=1"},
		{name: "no restriction and changed group", state: usernsState{exe: "/tmp/sbxr", primaryGroupDiffers: true}, want: "log in again"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := usernsFix(tt.state)
			if !strings.Contains(got, tt.want) || tt.not != "" && strings.Contains(got, tt.not) {
				t.Errorf("usernsFix(%+v) = %q, want it to contain %q and not %q", tt.state, got, tt.want, tt.not)
			}
		})
	}
}
