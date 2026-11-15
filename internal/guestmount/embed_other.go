//go:build !arm64 && !amd64

package guestmount

import "embed"

// sbxr supports only arm64 and amd64 hosts, so other builds have no helper.
var helper embed.FS

const helperDir = "helper"
