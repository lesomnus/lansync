package spec

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/lesomnus/lansync/lan"
)

// parseAddr reads an address in the context of subnet.
//
// ".N" is the N-th address of the subnet counted from the network address, so
// in 192.0.2.0/24 ".11" is 192.0.2.11 and in 192.0.2.0/23 ".300" is
// 192.0.3.44. A full dotted address is taken as is. Either way the result must
// be a host address of the subnet: not the network address and, for prefixes
// shorter than /31, not the broadcast address.
func parseAddr(subnet netip.Prefix, s string) (netip.Addr, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, fmt.Errorf("empty address")
	}

	var a netip.Addr
	if rest, ok := strings.CutPrefix(s, "."); ok {
		n, err := strconv.ParseUint(rest, 10, 32)
		if err != nil || rest != strconv.FormatUint(n, 10) {
			return netip.Addr{}, fmt.Errorf("%q: offset must be a decimal number without leading zeros", s)
		}
		size := uint64(1) << (32 - subnet.Bits())
		if n >= size {
			return netip.Addr{}, fmt.Errorf("%q: offset is outside %s (%d addresses)", s, subnet, size)
		}
		base := subnet.Masked().Addr().As4()
		v := uint64(base[0])<<24 | uint64(base[1])<<16 | uint64(base[2])<<8 | uint64(base[3])
		v += n
		a = netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	} else {
		var err error
		a, err = netip.ParseAddr(s)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("%q: not an address or .N offset", s)
		}
		if !a.Is4() {
			return netip.Addr{}, fmt.Errorf("%q: only IPv4 is supported", s)
		}
		if !subnet.Contains(a) {
			return netip.Addr{}, fmt.Errorf("%q: outside %s", s, subnet)
		}
	}

	if err := checkHost(subnet, a); err != nil {
		return netip.Addr{}, fmt.Errorf("%q: %w", s, err)
	}
	return a, nil
}

func checkHost(subnet netip.Prefix, a netip.Addr) error {
	if subnet.Bits() >= 31 {
		return nil
	}
	if a == subnet.Masked().Addr() {
		return fmt.Errorf("is the network address of %s", subnet)
	}
	if a == broadcast(subnet) {
		return fmt.Errorf("is the broadcast address of %s", subnet)
	}
	return nil
}

func broadcast(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// parseRange reads "A" or "A-B" where A and B are addresses as in parseAddr.
func parseRange(subnet netip.Prefix, s string) (lan.Range, error) {
	first, last, found := strings.Cut(s, "-")
	a, err := parseAddr(subnet, first)
	if err != nil {
		return lan.Range{}, err
	}
	if !found {
		return lan.Range{First: a, Last: a}, nil
	}
	b, err := parseAddr(subnet, last)
	if err != nil {
		return lan.Range{}, err
	}
	if b.Less(a) {
		return lan.Range{}, fmt.Errorf("%q: range ends before it starts", s)
	}
	return lan.Range{First: a, Last: b}, nil
}
