package node

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/encoding/protowire"
)

// criStatus is the CRI method that reports, among other things, the runtime handlers that containerd serves.
const criStatus = "/runtime.v1.RuntimeService/Status"

// Field numbers from k8s.io/cri-api runtime/v1 api.proto: StatusResponse.runtime_handlers and RuntimeHandler.name.
const (
	fieldRuntimeHandlers = 3
	fieldHandlerName     = 1
)

// criHandlers asks the CRI at the Unix socket for the runtime handlers that it serves. The request is an empty
// StatusRequest, and the response is decoded field by field, so the installer does not depend on the CRI API module.
// containerd fills runtime_handlers from its loaded config, so the answer shows what the running containerd serves.
func criHandlers(ctx context.Context, socket string) ([]string, error) {
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("CRI client for %s: %w", socket, err)
	}
	defer conn.Close() //nolint:errcheck // One call only.

	var resp []byte

	err = conn.Invoke(ctx, criStatus, &[]byte{}, &resp, grpc.ForceCodecV2(rawCodec{}))
	if err != nil {
		return nil, fmt.Errorf("CRI status on %s: %w", socket, err)
	}

	return parseHandlers(resp)
}

// parseHandlers returns the runtime handler names of a CRI StatusResponse.
func parseHandlers(b []byte) ([]string, error) {
	var names []string

	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, fmt.Errorf("CRI status: %w", protowire.ParseError(n))
		}

		b = b[n:]

		if num == fieldRuntimeHandlers && typ == protowire.BytesType {
			handler, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return nil, fmt.Errorf("CRI status: %w", protowire.ParseError(m))
			}

			name, err := handlerName(handler)
			if err != nil {
				return nil, err
			}

			names = append(names, name)
			b = b[m:]

			continue
		}

		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return nil, fmt.Errorf("CRI status: %w", protowire.ParseError(m))
		}

		b = b[m:]
	}

	return names, nil
}

func handlerName(b []byte) (string, error) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return "", fmt.Errorf("CRI runtime handler: %w", protowire.ParseError(n))
		}

		b = b[n:]

		if num == fieldHandlerName && typ == protowire.BytesType {
			name, m := protowire.ConsumeString(b)
			if m < 0 {
				return "", fmt.Errorf("CRI runtime handler: %w", protowire.ParseError(m))
			}

			return name, nil
		}

		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return "", fmt.Errorf("CRI runtime handler: %w", protowire.ParseError(m))
		}

		b = b[m:]
	}

	return "", errors.New("CRI runtime handler has no name")
}

// rawCodec sends and receives protobuf messages that are already encoded.
type rawCodec struct{}

func (rawCodec) Marshal(v any) (mem.BufferSlice, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("raw codec cannot marshal %T", v)
	}

	return mem.BufferSlice{mem.SliceBuffer(*b)}, nil
}

func (rawCodec) Unmarshal(data mem.BufferSlice, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("raw codec cannot unmarshal into %T", v)
	}

	*b = data.Materialize()

	return nil
}

func (rawCodec) Name() string { return "proto" }
