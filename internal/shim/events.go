//go:build linux

package shim

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	eventsAPI "github.com/containerd/containerd/api/services/ttrpc/events/v1"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// publisher sends task events to containerd over its TTRPC address, in order.
type publisher struct {
	namespace string
	address   string
	log       *slog.Logger

	mu     sync.Mutex
	client eventsAPI.TTRPCEventsService
	conn   *ttrpc.Client
}

func newPublisher(address, namespace string, log *slog.Logger) *publisher {
	return &publisher{namespace: namespace, address: strings.TrimPrefix(address, "unix://"), log: log, mu: sync.Mutex{}, client: nil, conn: nil}
}

// publish sends one event. A failure is logged, not returned: containerd also polls task state.
func (p *publisher) publish(topic string, event proto.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// containerd names event types by their protobuf names, without a URL prefix.
	value, err := proto.Marshal(event)
	if err != nil {
		p.log.Error("encode event", "topic", topic, "error", err)

		return
	}

	req := &eventsAPI.ForwardRequest{Envelope: &types.Envelope{
		Timestamp: timestamppb.Now(),
		Namespace: p.namespace,
		Topic:     topic,
		Event:     &anypb.Any{TypeUrl: string(proto.MessageName(event)), Value: value},
	}}

	for attempt := range 2 {
		if p.client == nil {
			if err := p.connect(); err != nil {
				p.log.Error("connect to containerd", "error", err)

				return
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = p.client.Forward(ctx, req)

		cancel()

		if err == nil {
			return
		}

		// containerd may have restarted, so reconnect once.
		_ = p.conn.Close()
		p.client, p.conn = nil, nil

		p.log.Warn("publish event", "topic", topic, "attempt", attempt, "error", err)
	}
}

func (p *publisher) connect() error {
	var d net.Dialer

	conn, err := d.DialContext(context.Background(), "unix", p.address)
	if err != nil {
		return fmt.Errorf("dial %s: %w", p.address, err)
	}

	p.conn = ttrpc.NewClient(conn)
	p.client = eventsAPI.NewTTRPCEventsClient(p.conn)

	return nil
}
