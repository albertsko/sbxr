package cli

import (
	"context"
	"net/netip"

	"github.com/albertsko/sbxr/internal/netstack"
	"github.com/albertsko/sbxr/internal/run"
)

// startNetwork starts the userspace network stack of one run in this process.
func startNetwork(ctx context.Context, cfg run.NetworkConfig) (run.Network, error) {
	publish := make([]netstack.Publish, 0, len(cfg.Publish))
	for _, p := range cfg.Publish {
		publish = append(publish, netstack.Publish{Host: netip.AddrPortFrom(p.HostIP, p.HostPort), Guest: p.GuestPort})
	}

	n, err := netstack.Start(ctx, netstack.Config{
		AllowLAN: cfg.AllowLAN,
		Publish:  publish,
		MaxTCP:   0,
		MaxUDP:   0,
		Logger:   cfg.Log,
		Dial:     nil,
		Hosts:    nil,
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // The runner names the network.
	}

	return n, nil
}
