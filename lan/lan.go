// Package lan is the vendor-neutral model of a LAN address plan: networks,
// the blocks their address space is divided into, DHCP pools and reservations.
//
// It is what a declaration resolves to (package spec) and what providers are
// compared against (package plan). Nothing here knows about any controller.
package lan

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// Range is an inclusive range of IPv4 addresses.
type Range struct {
	First netip.Addr
	Last  netip.Addr
}

func (r Range) Contains(a netip.Addr) bool {
	return r.First.Compare(a) <= 0 && a.Compare(r.Last) <= 0
}

func (r Range) Overlaps(o Range) bool {
	return r.First.Compare(o.Last) <= 0 && o.First.Compare(r.Last) <= 0
}

func (r Range) String() string {
	if r.First == r.Last {
		return r.First.String()
	}
	return r.First.String() + "-" + r.Last.String()
}

// Ranges is an ordered list of ranges.
type Ranges []Range

func (rs Ranges) Contains(a netip.Addr) bool {
	for _, r := range rs {
		if r.Contains(a) {
			return true
		}
	}
	return false
}

func (rs Ranges) Equal(o Ranges) bool {
	if len(rs) != len(o) {
		return false
	}
	for i := range rs {
		if rs[i] != o[i] {
			return false
		}
	}
	return true
}

func (rs Ranges) String() string {
	if len(rs) == 0 {
		return "(none)"
	}
	s := make([]string, len(rs))
	for i, r := range rs {
		s[i] = r.String()
	}
	return strings.Join(s, ",")
}

// Block is a named part of a network's address space.
type Block struct {
	Name   string
	Ranges Ranges
}

// Mode says what lansync may do to a network.
type Mode string

const (
	// DryRun reports differences and never changes the network.
	DryRun Mode = "dry-run"
	// Apply makes the network match the declaration.
	Apply Mode = "apply"
)

// DHCP is the declared DHCP server of a network.
type DHCP struct {
	// Block is the name of the block that is the address pool.
	Block string
	// Ranges are the pool, copied from the block.
	Ranges Ranges
	// LeaseTime is the lease duration. Zero means it is not managed.
	LeaseTime time.Duration
}

// Network is a declared network.
type Network struct {
	Name string
	// Selector tells the provider how to find the network. Its keys are
	// provider specific, e.g. "vlan" or "name".
	Selector map[string]string
	Subnet   netip.Prefix
	// Blocks are ordered by their first address.
	Blocks []Block
	// DHCP is nil when DHCP is not managed.
	DHCP *DHCP
	Mode Mode
}

func (n *Network) Block(name string) (Block, bool) {
	for _, b := range n.Blocks {
		if b.Name == name {
			return b, true
		}
	}
	return Block{}, false
}

// SelectorString renders the selector in a stable order, for messages.
func (n *Network) SelectorString() string {
	return FormatSelector(n.Selector)
}

// Reservation binds a MAC address to an IP address in a network.
type Reservation struct {
	Name    string
	MAC     net.HardwareAddr
	Network string
	// Block is optional. When set, IP must be inside it.
	Block string
	IP    netip.Addr
}

// Config is a resolved and validated declaration.
type Config struct {
	Networks     []Network
	Reservations []Reservation
}

// FormatSelector renders a selector as "k=v,k=v" in key order.
func FormatSelector(s map[string]string) string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%s", k, s[k])
	}
	return strings.Join(parts, ",")
}
