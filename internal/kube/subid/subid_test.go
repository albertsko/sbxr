package subid_test

import (
	"strings"
	"testing"

	"github.com/albertsko/sbxr/internal/kube/subid"
)

func TestChoose(t *testing.T) {
	t.Parallel()

	const kubelet = "kubelet:65536:7208960\n"

	tests := []struct {
		name           string
		subuid, subgid string
		first          int // -1 when Choose fails.
		isNew          bool
	}{
		{name: "after the kubelet range", subuid: kubelet, subgid: kubelet, first: 65536 + 7208960, isNew: true},
		{name: "in a gap, aligned", subuid: "# comment\nalice:100000:65536\n", subgid: "kubelet:40000000:65536\n", first: 196608, isNew: true},
		{name: "existing entry", subuid: kubelet + "sbxr:1073741824:33554432\n", subgid: kubelet + "sbxr:1073741824:33554432\n", first: 1 << 30},
		{name: "existing entry overlaps kubelet", subuid: kubelet + "sbxr:65536:33554432\n", subgid: kubelet + "sbxr:65536:33554432\n", first: -1},
		{name: "entries differ", subuid: "sbxr:65536:33554432\n", subgid: "sbxr:131072:33554432\n", first: -1},
		{name: "no room", subuid: "big:0:4294000000\n", subgid: "", first: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, isNew, err := subid.Choose(tt.subuid, tt.subgid)

			switch {
			case tt.first < 0 && err == nil:
				t.Errorf("Choose = %s, want an error", r)
			case tt.first >= 0 && (err != nil || r.First != tt.first || r.Count != subid.Count || isNew != tt.isNew):
				t.Errorf("Choose = %s, new %v, %v; want first %d, new %v", r, isNew, err, tt.first, tt.isNew)
			}
		})
	}
}

func TestFind(t *testing.T) {
	t.Parallel()

	r, err := subid.Find("sbxr:1073741824:33554432\n", "x:1:2\nsbxr:1073741824:33554432\n", subid.Name)
	if err != nil || r.String() != "sbxr:1073741824:33554432" {
		t.Errorf("Find = %s, %v", r, err)
	}

	if _, err := subid.Find("", "sbxr:1:2\n", subid.Name); err == nil || !strings.Contains(err.Error(), "/etc/subuid") {
		t.Errorf("Find without a subuid entry: %v", err)
	}
}

func TestChooseAvoidsReservedRanges(t *testing.T) {
	t.Parallel()

	// A kubelet range from NSS is not in the files, but getsubids reports it.
	kubelet, err := subid.ParseGetsubids("0: kubelet 65536 7208960\n")
	if err != nil {
		t.Fatal(err)
	}

	r, isNew, err := subid.Choose("", "", kubelet)
	if err != nil || !isNew || r.First != 65536+7208960 {
		t.Errorf("Choose = %s, %v, %v; want the range after the kubelet range", r, isNew, err)
	}
}
