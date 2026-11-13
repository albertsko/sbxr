//go:build baseline

// The baseline build has no network stack. It measures binary size and boot time with the default TSI backend.
package main

import (
	"log"
	"os"
)

func main() {
	err := runVMM(vmmConfig{rootfs: os.Args[1], sockFD: -1, argv: os.Args[2:], env: []string{"PATH=/bin:/sbin:/usr/bin"}, logLevel: 1})
	log.Fatal(err)
}
