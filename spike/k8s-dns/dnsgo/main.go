package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	conf, _ := os.ReadFile("/etc/resolv.conf")
	fmt.Printf("resolv.conf:\n%s", conf)

	resolver := &net.Resolver{PreferGo: true}
	failed := false
	for _, host := range os.Args[1:] {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()
		addrs, err := resolver.LookupHost(ctx, host)
		cancel()
		took := time.Since(start).Round(time.Millisecond)
		if err != nil {
			fmt.Printf("FAIL %s (%s): %v\n", host, took, err)
			failed = true
			continue
		}
		fmt.Printf("OK %s (%s): %v\n", host, took, addrs)
	}

	if failed {
		os.Exit(1)
	}
}
