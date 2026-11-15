package guestmount

import "embed"

// The directory always holds a README, so the build works before go generate.
//
//go:embed helper/amd64
var helper embed.FS

const helperDir = "helper/amd64"
