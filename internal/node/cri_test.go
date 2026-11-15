package node

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
)

// statusResponse encodes a CRI StatusResponse with the runtime handlers and a status and info that the reader skips.
func statusResponse(handlers ...string) []byte {
	b := protowire.AppendTag(nil, 1, protowire.BytesType) // status
	b = protowire.AppendBytes(b, protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 1))

	for _, h := range handlers {
		features := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 1)
		handler := protowire.AppendString(protowire.AppendTag(nil, fieldHandlerName, protowire.BytesType), h)
		handler = protowire.AppendBytes(protowire.AppendTag(handler, 2, protowire.BytesType), features)

		b = protowire.AppendBytes(protowire.AppendTag(b, fieldRuntimeHandlers, protowire.BytesType), handler)
	}

	info := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "config")

	return protowire.AppendBytes(protowire.AppendTag(b, 2, protowire.BytesType), info)
}

// TestCRIHandlers serves the CRI status call on a Unix socket, as containerd does, and reads its runtime handlers.
func TestCRIHandlers(t *testing.T) {
	t.Parallel()

	dir, err := os.MkdirTemp("/tmp", "cri-") //nolint:usetesting // Unix socket paths must be short, and macOS temporary directories are long.
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "containerd.sock")

	var lc net.ListenConfig

	l, err := lc.Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}

	srv := grpc.NewServer(grpc.ForceServerCodecV2(rawCodec{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		var req []byte
		if err := stream.RecvMsg(&req); err != nil {
			return err //nolint:wrapcheck // The test server passes it on.
		}

		if method, _ := grpc.MethodFromServerStream(stream); method != criStatus {
			t.Errorf("method %q, want %q", method, criStatus)
		}

		resp := statusResponse("", "runc", "sbxr")

		return stream.SendMsg(&resp)
	}))

	go func() { _ = srv.Serve(l) }()

	t.Cleanup(srv.Stop)

	handlers, err := criHandlers(t.Context(), socket)
	if err != nil || !slices.Equal(handlers, []string{"", "runc", "sbxr"}) {
		t.Errorf("criHandlers = %q, %v", handlers, err)
	}

	if _, err := criHandlers(t.Context(), filepath.Join(dir, "missing.sock")); err == nil {
		t.Error("criHandlers without a server: no error")
	}
}

func TestParseHandlersRejectsBadInput(t *testing.T) {
	t.Parallel()

	nameless := protowire.AppendBytes(protowire.AppendTag(nil, fieldRuntimeHandlers, protowire.BytesType), nil)

	for name, b := range map[string][]byte{"truncated": {0x1a, 0x05, 0x0a}, "no name": nameless} {
		if got, err := parseHandlers(b); err == nil {
			t.Errorf("%s: parseHandlers = %q, want an error", name, got)
		}
	}
}
