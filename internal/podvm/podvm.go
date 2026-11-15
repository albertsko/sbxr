// Package podvm holds the contract between the phase 2 shim on the host and the guest agent in the pod VM.
//
// The shim writes a [Pod] into the pod share before the VM boots. The agent serves the containerd task API
// on [ControlPort] and carries raw stdio and port-forward streams on [StreamPort]. Each stream starts with one [Header] line.
package podvm

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Guest vsock ports.
const (
	ControlPort = 1024
	StreamPort  = 1025
)

// ShareTag is the virtio-fs tag of the pod share, which holds the pod config, the container bundles, and their mounts.
const ShareTag = "sbxr"

// GuestShare is where the agent mounts the pod share.
const GuestShare = "/run/sbxr/shared"

// PodFile is the name of the pod config in the pod share.
const PodFile = "pod.json"

// Pod is the pod config that the agent applies at boot.
type Pod struct {
	Hostname string `json:"hostname"`
	// Addresses are the pod interface addresses in CIDR form, for example 10.244.0.5/24.
	Addresses []netip.Prefix `json:"addresses"`
	Routes    []Route        `json:"routes"`
	MTU       int            `json:"mtu"`
}

// Route is one IPv4 or IPv6 route of the pod interface.
type Route struct {
	// Dst is the destination prefix. A zero value is the default route.
	Dst netip.Prefix `json:"dst,omitzero"`
	// Gateway is the next hop. A zero value is an on-link route.
	Gateway netip.Addr `json:"gateway,omitzero"`
	// OnLink marks a gateway that is reachable on the link without a covering address route.
	OnLink bool `json:"onLink,omitempty"`
}

// WritePod writes p into the pod share dir.
func WritePod(dir string, p Pod) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode pod config: %w", err)
	}

	err = os.WriteFile(filepath.Join(dir, PodFile), data, 0o644) //nolint:gosec // The guest reads it.
	if err != nil {
		return fmt.Errorf("write pod config: %w", err)
	}

	return nil
}

// ReadPod reads the pod config from the pod share dir.
func ReadPod(dir string) (Pod, error) {
	var p Pod

	data, err := os.ReadFile(filepath.Join(dir, PodFile))
	if err != nil {
		return p, fmt.Errorf("read pod config: %w", err)
	}

	err = json.Unmarshal(data, &p)
	if err != nil {
		return p, fmt.Errorf("decode pod config: %w", err)
	}

	return p, nil
}

// Stream kinds.
const (
	Stdin       = "stdin"
	Stdout      = "stdout"
	Stderr      = "stderr"
	PortForward = "port"
)

// Header opens a stream: a stdio stream of one process, or a port-forward stream to a guest TCP port.
type Header struct {
	Kind string
	// Container and Exec name the process of a stdio stream. Exec is empty for the container's init process.
	Container string
	Exec      string
	// Port is the guest TCP port of a port-forward stream.
	Port uint16
}

// String encodes h as one line without the newline.
func (h Header) String() string {
	if h.Kind == PortForward {
		return PortForward + " " + strconv.Itoa(int(h.Port))
	}

	return h.Kind + " " + h.Container + " " + cmp.Or(h.Exec, "-")
}

// ReadHeader reads one header line from r.
func ReadHeader(r *bufio.Reader) (Header, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return Header{}, fmt.Errorf("read stream header: %w", err)
	}

	f := strings.Fields(line)

	switch {
	case len(f) == 2 && f[0] == PortForward:
		port, err := strconv.ParseUint(f[1], 10, 16)
		if err != nil {
			return Header{}, fmt.Errorf("stream header %q: %w", line, err)
		}

		return Header{Kind: PortForward, Container: "", Exec: "", Port: uint16(port)}, nil
	case len(f) == 3 && (f[0] == Stdin || f[0] == Stdout || f[0] == Stderr):
		exec := f[2]
		if exec == "-" {
			exec = ""
		}

		return Header{Kind: f[0], Container: f[1], Exec: exec, Port: 0}, nil
	}

	return Header{}, errors.New("stream header " + strconv.Quote(line) + " is not valid")
}
