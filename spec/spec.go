// Package spec reads a lansync declaration and resolves it into a validated
// lan.Config.
//
// A declaration has three parts:
//
//	layouts:        named address plans, reusable across networks
//	networks:       the networks to manage, each found by a provider selector
//	reservations:   MAC to IP bindings
//
// See the README for the full syntax.
package spec

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/lesomnus/lansync/lan"
)

// File is the declaration as written.
type File struct {
	Layouts      map[string]Layout `yaml:"layouts"`
	Networks     []Network         `yaml:"networks"`
	Reservations []Reservation     `yaml:"reservations"`
}

type Layout struct {
	Blocks []Block `yaml:"blocks"`
}

type Block struct {
	Name   string `yaml:"name"`
	Ranges []Text `yaml:"ranges"`
}

type Network struct {
	Name   string          `yaml:"name"`
	Match  map[string]Text `yaml:"match"`
	Subnet string          `yaml:"subnet"`
	Layout string          `yaml:"layout"`
	// Blocks are added to the layout's blocks.
	Blocks []Block `yaml:"blocks"`
	DHCP   *DHCP   `yaml:"dhcp"`
	Mode   string  `yaml:"mode"`
}

type DHCP struct {
	Block     string `yaml:"block"`
	LeaseTime string `yaml:"leaseTime"`
}

type Reservation struct {
	Name    string `yaml:"name"`
	MAC     string `yaml:"mac"`
	Network string `yaml:"network"`
	Block   string `yaml:"block"`
	IP      Text   `yaml:"ip"`
}

// Text keeps a scalar as written. Without it ".1" is read as the float 0.1 and
// a VLAN number as an int.
type Text string

func (t *Text) UnmarshalYAML(b []byte) error {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, `"`) || strings.HasPrefix(s, `'`) {
		var v string
		if err := yaml.Unmarshal(b, &v); err != nil {
			return err
		}
		s = v
	}
	*t = Text(s)
	return nil
}

// LoadFile reads and resolves a declaration file.
func LoadFile(path string) (*lan.Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Load(f)
}

// Load reads and resolves a declaration. All problems found are returned
// together.
func Load(r io.Reader) (*lan.Config, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.UnmarshalWithOptions(b, &f, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return f.Resolve()
}

// Resolve validates the declaration and expands layouts and offsets.
func (f *File) Resolve() (*lan.Config, error) {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	for name, l := range f.Layouts {
		if name == "" {
			fail("layouts: empty layout name")
		}
		if len(l.Blocks) == 0 {
			fail("layouts.%s: no blocks", name)
		}
	}

	c := &lan.Config{}
	names := map[string]bool{}
	for i, n := range f.Networks {
		at := fmt.Sprintf("networks[%d]", i)
		if n.Name != "" {
			at = fmt.Sprintf("networks[%d] (%s)", i, n.Name)
		}
		nw, nerrs := f.resolveNetwork(n)
		for _, e := range nerrs {
			fail("%s: %w", at, e)
		}
		if n.Name != "" {
			if names[n.Name] {
				fail("%s: name is used by another network", at)
			}
			names[n.Name] = true
		}
		if nw != nil {
			c.Networks = append(c.Networks, *nw)
		}
	}

	byName := map[string]*lan.Network{}
	for i := range c.Networks {
		byName[c.Networks[i].Name] = &c.Networks[i]
	}

	seenMAC := map[string]string{}
	seenIP := map[netip.Addr]string{}
	seenName := map[string]bool{}
	for i, r := range f.Reservations {
		at := fmt.Sprintf("reservations[%d]", i)
		if r.Name != "" {
			at = fmt.Sprintf("reservations[%d] (%s)", i, r.Name)
		}
		res, rerrs := resolveReservation(r, byName)
		for _, e := range rerrs {
			fail("%s: %w", at, e)
		}
		if r.Name != "" {
			if seenName[r.Name] {
				fail("%s: name is used by another reservation", at)
			}
			seenName[r.Name] = true
		}
		if res == nil {
			continue
		}
		if other, ok := seenMAC[res.MAC.String()]; ok {
			fail("%s: MAC is also reserved by %s", at, other)
		}
		seenMAC[res.MAC.String()] = res.Name
		if other, ok := seenIP[res.IP]; ok {
			fail("%s: %s is also reserved by %s", at, res.IP, other)
		}
		seenIP[res.IP] = res.Name
		c.Reservations = append(c.Reservations, *res)
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func (f *File) resolveNetwork(n Network) (*lan.Network, []error) {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if n.Name == "" {
		fail("name is required")
	}
	if len(n.Match) == 0 {
		fail("match is required: it tells the provider which network this is")
	}
	sel := map[string]string{}
	for k, v := range n.Match {
		if k == "" || v == "" {
			fail("match: empty key or value")
		}
		sel[k] = string(v)
	}

	mode := lan.DryRun
	switch n.Mode {
	case "", string(lan.DryRun):
	case string(lan.Apply):
		mode = lan.Apply
	default:
		fail("mode: %q is not one of %q, %q", n.Mode, lan.DryRun, lan.Apply)
	}

	subnet, err := netip.ParsePrefix(n.Subnet)
	switch {
	case n.Subnet == "":
		fail("subnet is required")
		return nil, errs
	case err != nil:
		fail("subnet: %v", err)
		return nil, errs
	case !subnet.Addr().Is4():
		fail("subnet: only IPv4 is supported")
		return nil, errs
	case subnet != subnet.Masked():
		fail("subnet: %s has host bits set; did you mean %s?", subnet, subnet.Masked())
		return nil, errs
	}

	var decl []Block
	if n.Layout != "" {
		l, ok := f.Layouts[n.Layout]
		if !ok {
			fail("layout: %q is not defined", n.Layout)
		}
		decl = append(decl, l.Blocks...)
	}
	decl = append(decl, n.Blocks...)

	var blocks []lan.Block
	seen := map[string]bool{}
	for _, b := range decl {
		if b.Name == "" {
			fail("blocks: a block has no name")
			continue
		}
		if seen[b.Name] {
			fail("blocks: %q is defined twice", b.Name)
			continue
		}
		seen[b.Name] = true
		if len(b.Ranges) == 0 {
			fail("blocks.%s: no ranges", b.Name)
			continue
		}
		var rs lan.Ranges
		for _, t := range b.Ranges {
			r, err := parseRange(subnet, string(t))
			if err != nil {
				fail("blocks.%s: %v", b.Name, err)
				continue
			}
			rs = append(rs, r)
		}
		slices.SortFunc(rs, func(a, b lan.Range) int { return a.First.Compare(b.First) })
		blocks = append(blocks, lan.Block{Name: b.Name, Ranges: rs})
	}

	// No address may belong to two blocks, nor twice to one block.
	type owned struct {
		block string
		r     lan.Range
	}
	var all []owned
	for _, b := range blocks {
		for _, r := range b.Ranges {
			all = append(all, owned{b.Name, r})
		}
	}
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[i].r.Overlaps(all[j].r) {
				if all[i].block == all[j].block {
					fail("blocks.%s: ranges %s and %s overlap", all[i].block, all[i].r, all[j].r)
				} else {
					fail("blocks: %s (%s) and %s (%s) overlap", all[i].block, all[i].r, all[j].block, all[j].r)
				}
			}
		}
	}
	slices.SortFunc(blocks, func(a, b lan.Block) int {
		if len(a.Ranges) == 0 || len(b.Ranges) == 0 {
			return 0
		}
		return a.Ranges[0].First.Compare(b.Ranges[0].First)
	})

	out := &lan.Network{
		Name:     n.Name,
		Selector: sel,
		Subnet:   subnet,
		Blocks:   blocks,
		Mode:     mode,
	}

	if n.DHCP != nil {
		d := &lan.DHCP{Block: n.DHCP.Block}
		if d.Block == "" {
			fail("dhcp.block is required: name the block that is the address pool")
		} else if b, ok := out.Block(d.Block); !ok {
			fail("dhcp.block: %q is not a block of this network", d.Block)
		} else {
			d.Ranges = b.Ranges
		}
		if n.DHCP.LeaseTime != "" {
			t, err := time.ParseDuration(n.DHCP.LeaseTime)
			switch {
			case err != nil:
				fail("dhcp.leaseTime: %v", err)
			case t <= 0:
				fail("dhcp.leaseTime: must be positive")
			case t%time.Second != 0:
				fail("dhcp.leaseTime: must be whole seconds")
			default:
				d.LeaseTime = t
			}
		}
		out.DHCP = d
	}

	return out, errs
}

func resolveReservation(r Reservation, networks map[string]*lan.Network) (*lan.Reservation, []error) {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if r.Name == "" {
		fail("name is required")
	}
	mac, err := net.ParseMAC(r.MAC)
	if err != nil {
		fail("mac: %v", err)
	} else if len(mac) != 6 {
		fail("mac: %q is not a 48-bit MAC address", r.MAC)
	}
	n, ok := networks[r.Network]
	if r.Network == "" {
		fail("network is required")
	} else if !ok {
		fail("network: %q is not a declared network", r.Network)
	}
	if len(errs) > 0 || n == nil {
		return nil, errs
	}

	ip, err := parseAddr(n.Subnet, string(r.IP))
	if err != nil {
		fail("ip: %v", err)
		return nil, errs
	}
	if r.Block != "" {
		b, ok := n.Block(r.Block)
		if !ok {
			fail("block: %q is not a block of network %s", r.Block, n.Name)
		} else if !b.Ranges.Contains(ip) {
			fail("ip: %s is not in block %s (%s)", ip, b.Name, b.Ranges)
		}
	}
	if n.DHCP != nil && n.DHCP.Ranges.Contains(ip) {
		fail("ip: %s is inside the DHCP pool (%s); reserve it outside the pool", ip, n.DHCP.Block)
	}

	return &lan.Reservation{
		Name:    r.Name,
		MAC:     mac,
		Network: n.Name,
		Block:   r.Block,
		IP:      ip,
	}, errs
}
