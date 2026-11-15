package guestmount

import "embed"

// The directory always holds a README, so the build works before go generate.
//
//go:embed helper/arm64
var helper embed.FS

const helperDir = "helper/arm64"
