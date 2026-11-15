package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/albertsko/sbxr/internal/netstack/guestsim"
	"github.com/albertsko/sbxr/internal/vm"
)

// dhcpTimeout bounds the network setup at boot.
const dhcpTimeout = 10 * time.Second

var errNoNetwork = errors.New("Network is unreachable") //nolint:staticcheck // Matches libc strerror.

// attachNetwork configures the guest network on the device of the machine, like the libkrun init does with DHCP.
// It returns nil without a device.
func attachNetwork(dev *vm.Net) (*guestsim.Guest, error) {
	if dev == nil {
		return nil, nil //nolint:nilnil // No device means no network, which is not an error.
	}

	mac, err := net.ParseMAC(dev.MAC)
	if err != nil {
		return nil, fmt.Errorf("network device: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dhcpTimeout)
	defer cancel()

	g, err := guestsim.Start(ctx, os.NewFile(uintptr(dev.FD), "net"), mac)
	if err != nil {
		return nil, fmt.Errorf("network device: %w", err)
	}

	return g, nil
}

// wget supports `wget [-q] [-O -] [-T SECONDS] URL` and writes the body of a GET to stdout.
func (g *guest) wget(args []string, stdio Stdio) int {
	timeout := 10 * time.Second
	url := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-q", "-qO-", "-O-":
		case "-O":
			i++
		case "-T":
			i++
			if i < len(args) {
				if n, err := strconv.Atoi(args[i]); err == nil {
					timeout = time.Duration(n) * time.Second
				}
			}
		default:
			url = args[i]
		}
	}

	if g.net == nil {
		sayf(stdio.Err, "wget: can't connect to remote host: %v\n", errNoNetwork)

		return 1
	}

	client := &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return g.net.DialTCP(ctx, addr) }},
		Timeout:   timeout,
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		sayf(stdio.Err, "wget: bad address '%s'\n", url)

		return 1
	}

	resp, err := client.Do(req)
	if err != nil {
		sayf(stdio.Err, "wget: can't connect to remote host: %v\n", err)

		return 1
	}
	defer resp.Body.Close() //nolint:errcheck // The body was only read.

	if resp.StatusCode != http.StatusOK {
		sayf(stdio.Err, "wget: server returned error: %s\n", resp.Status)

		return 1
	}

	_, _ = io.Copy(stdio.Out, resp.Body) // The guest ignores a failed console write.

	return 0
}

// nc supports `nc -l -p PORT` for one connection and `nc HOST PORT`.
// It copies stdin to the connection and the connection to stdout until the peer closes it.
func (g *guest) nc(args []string, stdio Stdio) int {
	if g.net == nil {
		sayf(stdio.Err, "nc: %v\n", errNoNetwork)

		return 1
	}

	conn, err := g.ncConnect(args)
	if err != nil {
		sayf(stdio.Err, "nc: %v\n", err)

		return 1
	}
	defer conn.Close() //nolint:errcheck // The copy below reports the outcome.

	go func() {
		_, _ = io.Copy(conn, stdio.In) // The peer decides when the session ends.
	}()

	_, _ = io.Copy(stdio.Out, conn) // The guest ignores a failed console write.

	return 0
}

func (g *guest) ncConnect(args []string) (net.Conn, error) {
	if len(args) == 3 && args[0] == "-l" && args[1] == "-p" {
		port, err := strconv.ParseUint(args[2], 10, 16)
		if err != nil {
			return nil, fmt.Errorf("bad port '%s'", args[2])
		}

		ln, err := g.net.ListenTCP(uint16(port))
		if err != nil {
			return nil, err //nolint:wrapcheck // The guest prints the error.
		}
		defer ln.Close() //nolint:errcheck // One connection is enough.

		return ln.Accept() //nolint:wrapcheck // The guest prints the error.
	}

	if len(args) == 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		return g.net.DialTCP(ctx, net.JoinHostPort(args[0], args[1])) //nolint:wrapcheck // The guest prints the error.
	}

	return nil, errors.New("usage: nc -l -p PORT, or nc HOST PORT")
}

// nslookup supports `nslookup [-type=a] NAME` and prints the IPv4 addresses that the DNS server of the guest returns.
func (g *guest) nslookup(args []string, stdio Stdio) int {
	name := ""

	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			name = a
		}
	}

	if g.net == nil {
		sayf(stdio.Err, "nslookup: %v\n", errNoNetwork)

		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addrs, err := g.net.LookupHost(ctx, name)
	if err != nil {
		sayf(stdio.Err, "** server can't find %s: %v\n", name, err)

		return 1
	}

	sayf(stdio.Out, "Name:\t%s\n", name)

	for _, a := range addrs {
		sayf(stdio.Out, "Address: %s\n", a)
	}

	return 0
}
