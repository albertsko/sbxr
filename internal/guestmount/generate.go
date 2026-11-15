package guestmount

// The helper is built for both architectures, so that one generate step serves every release build.
//go:generate env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath "-ldflags=-s -w" -o helper/arm64/sbxr-mount github.com/albertsko/sbxr/cmd/sbxr-mount
//go:generate env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath "-ldflags=-s -w" -o helper/amd64/sbxr-mount github.com/albertsko/sbxr/cmd/sbxr-mount
