//go:build linux

package shim

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// podConfig is the part of the CRI PodSandboxConfig that the shim uses.
// containerd passes the config as an Any in CreateSandboxRequest.Options, and this reads it field by field,
// so the shim does not depend on the CRI API module.
type podConfig struct {
	Hostname string
	// DNS servers, searches, and options. With none, the pod uses the node's resolv.conf.
	DNSServers   []string
	DNSSearches  []string
	DNSOptions   []string
	CgroupParent string
	// MemoryBytes and the CPU quota and period are the pod's resources: the sum of the container limits.
	MemoryBytes int64
	CPUQuota    int64
	CPUPeriod   int64
}

// Field numbers from k8s.io/cri-api runtime/v1 api.proto.
const (
	fieldHostname = 2
	fieldDNS      = 4
	fieldLinux    = 8

	fieldDNSServers  = 1
	fieldDNSSearches = 2
	fieldDNSOptions  = 3

	fieldCgroupParent = 1
	fieldResources    = 5

	fieldCPUPeriod = 1
	fieldCPUQuota  = 2
	fieldMemory    = 4
)

func parsePodConfig(b []byte) (podConfig, error) {
	var c podConfig

	dns := map[protowire.Number]*[]string{fieldDNSServers: &c.DNSServers, fieldDNSSearches: &c.DNSSearches, fieldDNSOptions: &c.DNSOptions}

	err := eachField(b, func(num protowire.Number, v []byte, _ uint64) error {
		if num == fieldHostname {
			c.Hostname = string(v)
		}

		if num == fieldDNS {
			return eachField(v, func(num protowire.Number, v []byte, _ uint64) error {
				if list, ok := dns[num]; ok {
					*list = append(*list, string(v))
				}

				return nil
			})
		}

		if num != fieldLinux {
			return nil
		}

		return eachField(v, func(num protowire.Number, v []byte, _ uint64) error {
			if num == fieldCgroupParent {
				c.CgroupParent = string(v)
			}

			if num == fieldResources {
				return parseResources(v, &c.MemoryBytes, &c.CPUQuota, &c.CPUPeriod)
			}

			return nil
		})
	})

	return c, err
}

func parseResources(b []byte, memory, quota, period *int64) error {
	fields := map[protowire.Number]*int64{fieldMemory: memory, fieldCPUQuota: quota, fieldCPUPeriod: period}

	return eachField(b, func(num protowire.Number, _ []byte, n uint64) error {
		if f, ok := fields[num]; ok {
			*f = int64(n) //nolint:gosec // proto int64 fields travel as uint64 varints.
		}

		return nil
	})
}

// eachField calls fn for every field of a protobuf message: with the bytes of a length-delimited field, or the value of a varint.
func eachField(b []byte, fn func(num protowire.Number, bytes []byte, varint uint64) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return fmt.Errorf("pod config: %w", protowire.ParseError(n))
		}

		b = b[n:]

		var (
			bytes  []byte
			varint uint64
		)

		switch typ { //nolint:exhaustive // Other wire types are skipped.
		case protowire.BytesType:
			bytes, n = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			varint, n = protowire.ConsumeVarint(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}

		if n < 0 {
			return fmt.Errorf("pod config: %w", protowire.ParseError(n))
		}

		b = b[n:]

		if err := fn(num, bytes, varint); err != nil {
			return err
		}
	}

	return nil
}

// resolvConf returns the pod's resolv.conf, or nil to use the node's.
func (c podConfig) resolvConf() []byte {
	if len(c.DNSServers) == 0 && len(c.DNSSearches) == 0 && len(c.DNSOptions) == 0 {
		return nil
	}

	var b []byte

	if len(c.DNSSearches) > 0 {
		b = fmt.Appendf(b, "search %s\n", strings.Join(c.DNSSearches, " "))
	}

	for _, s := range c.DNSServers {
		b = fmt.Appendf(b, "nameserver %s\n", s)
	}

	if len(c.DNSOptions) > 0 {
		b = fmt.Appendf(b, "options %s\n", strings.Join(c.DNSOptions, " "))
	}

	return b
}
