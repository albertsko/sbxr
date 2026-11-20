// Command dnsgo resolves each argument with the pure Go resolver and exits 1 when any lookup fails.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	code := 0
	resolver := &net.Resolver{PreferGo: true}

	for _, host := range os.Args[1:] {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		addrs, err := resolver.LookupHost(ctx, host)

		cancel()

		if err != nil {
			fmt.Printf("FAIL %s: %v\n", host, err)

			code = 1

			continue
		}

		fmt.Printf("OK %s %v\n", host, addrs)
	}

	os.Exit(code)
}
