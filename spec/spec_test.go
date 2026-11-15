package spec_test

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/lansync/lan"
	"github.com/lesomnus/lansync/spec"
)

const example = `
layouts:
  office:
    blocks:
      - { name: gateway, ranges: [.1] }
      - { name: core,    ranges: [.2-.9] }
      - { name: infra,   ranges: [.10-.49] }
      - { name: devices, ranges: [.50-.59, .70-.79] }
      - { name: dhcp,    ranges: [.100-.199] }
      - { name: static,  ranges: [.200-.254] }

networks:
  - name: floor-2
    match: { vlan: 2 }
    subnet: 192.0.2.0/24
    layout: office
    dhcp: { block: dhcp, leaseTime: 8h }
  - name: wifi
    match: { name: "Office WiFi" }
    subnet: 198.51.100.0/23
    blocks:
      - { name: pool, ranges: [.10-.400] }
    dhcp: { block: pool }

reservations:
  - { name: ap-01, mac: "00:00:5e:00:53:01", network: floor-2, block: infra, ip: .11 }
  - { name: printer, mac: "00-00-5E-00-53-02", network: floor-2, ip: 192.0.2.71 }
`

func load(t *testing.T, src string) (*lan.Config, error) {
	t.Helper()
	return spec.Load(strings.NewReader(src))
}

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestLoadExample(t *testing.T) {
	c, err := load(t, example)
	require.NoError(t, err)
	require.Len(t, c.Networks, 2)

	f2 := c.Networks[0]
	require.Equal(t, "floor-2", f2.Name)
	require.Equal(t, map[string]string{"vlan": "2"}, f2.Selector)
	require.Len(t, f2.Blocks, 6)
	require.Equal(t, "gateway", f2.Blocks[0].Name)
	require.Equal(t, lan.Ranges{{First: addr("192.0.2.1"), Last: addr("192.0.2.1")}}, f2.Blocks[0].Ranges)

	dev, ok := f2.Block("devices")
	require.True(t, ok)
	require.Equal(t, lan.Ranges{
		{First: addr("192.0.2.50"), Last: addr("192.0.2.59")},
		{First: addr("192.0.2.70"), Last: addr("192.0.2.79")},
	}, dev.Ranges)

	require.NotNil(t, f2.DHCP)
	require.Equal(t, "dhcp", f2.DHCP.Block)
	require.Equal(t, 8*time.Hour, f2.DHCP.LeaseTime)
	require.Equal(t, lan.Ranges{{First: addr("192.0.2.100"), Last: addr("192.0.2.199")}}, f2.DHCP.Ranges)

	wifi := c.Networks[1]
	require.Equal(t, time.Duration(0), wifi.DHCP.LeaseTime, "lease time is unmanaged when omitted")
	// Offsets count from the network address, so they work past /24.
	require.Equal(t, lan.Ranges{{First: addr("198.51.100.10"), Last: addr("198.51.101.144")}}, wifi.DHCP.Ranges)

	require.Len(t, c.Reservations, 2)
	require.Equal(t, addr("192.0.2.11"), c.Reservations[0].IP)
	require.Equal(t, "00:00:5e:00:53:02", c.Reservations[1].MAC.String())
}

func TestLoadRejects(t *testing.T) {
	base := func(network string) string {
		return "networks:\n" + network
	}
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "unknown field",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, purpose: x }\n"),
			want: `unknown field "purpose"`,
		},
		{
			name: "host bits",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.1/24 }\n"),
			want: "host bits set",
		},
		{
			name: "no match",
			src:  base("  - { name: a, subnet: 192.0.2.0/24 }\n"),
			want: "match is required",
		},
		{
			name: "mode is not part of the declaration",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, mode: apply }\n"),
			want: `unknown field "mode"`,
		},
		{
			name: "offset outside",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, blocks: [{ name: b, ranges: [.10-.256] }] }\n"),
			want: "outside 192.0.2.0/24",
		},
		{
			name: "broadcast",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, blocks: [{ name: b, ranges: [.200-.255] }] }\n"),
			want: "broadcast",
		},
		{
			name: "network address",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, blocks: [{ name: b, ranges: [.0] }] }\n"),
			want: "network address",
		},
		{
			name: "full address outside",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, blocks: [{ name: b, ranges: [198.51.100.1] }] }\n"),
			want: "outside",
		},
		{
			name: "reversed",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, blocks: [{ name: b, ranges: [.20-.10] }] }\n"),
			want: "ends before it starts",
		},
		{
			name: "leading zero",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, blocks: [{ name: b, ranges: [.010] }] }\n"),
			want: "leading zeros",
		},
		{
			name: "blocks overlap",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, blocks: [{ name: b, ranges: [.10-.20] }, { name: c, ranges: [.20-.30] }] }\n"),
			want: "b (192.0.2.10-192.0.2.20) and c (192.0.2.20-192.0.2.30) overlap",
		},
		{
			name: "ranges overlap within a block",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, blocks: [{ name: b, ranges: [.10-.20, .15] }] }\n"),
			want: "blocks.b: ranges",
		},
		{
			name: "dhcp block missing",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, dhcp: { block: pool } }\n"),
			want: `dhcp.block: "pool" is not a block`,
		},
		{
			name: "lease time fraction",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, blocks: [{ name: p, ranges: [.10-.20] }], dhcp: { block: p, leaseTime: 1500ms } }\n"),
			want: "whole seconds",
		},
		{
			name: "unknown layout",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, layout: nope }\n"),
			want: `layout: "nope" is not defined`,
		},
		{
			name: "duplicate network",
			src:  base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24 }\n  - { name: a, match: { vlan: 2 }, subnet: 198.51.100.0/24 }\n"),
			want: "name is used by another network",
		},
		{
			name: "reservation in pool",
			src: base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, blocks: [{ name: p, ranges: [.100-.199] }], dhcp: { block: p } }\n") +
				"reservations:\n  - { name: r, mac: '00:00:5e:00:53:01', network: a, ip: .150 }\n",
			want: "inside the DHCP pool",
		},
		{
			name: "reservation outside its block",
			src: base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24, blocks: [{ name: i, ranges: [.10-.19] }] }\n") +
				"reservations:\n  - { name: r, mac: '00:00:5e:00:53:01', network: a, block: i, ip: .20 }\n",
			want: "not in block i",
		},
		{
			name: "duplicate reservation ip",
			src: base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24 }\n") +
				"reservations:\n  - { name: r, mac: '00:00:5e:00:53:01', network: a, ip: .20 }\n  - { name: s, mac: '00:00:5e:00:53:02', network: a, ip: .20 }\n",
			want: "also reserved by r",
		},
		{
			name: "bad mac",
			src: base("  - { name: a, match: { vlan: 1 }, subnet: 192.0.2.0/24 }\n") +
				"reservations:\n  - { name: r, mac: 'nope', network: a, ip: .20 }\n",
			want: "mac:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, tt.src)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestLoadReportsAllProblems(t *testing.T) {
	_, err := load(t, `
networks:
  - { name: a, subnet: 192.0.2.0/24 }
  - { name: b, match: { vlan: 2 }, subnet: 192.0.2.1/24 }
`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "match is required")
	require.Contains(t, err.Error(), "host bits set")
}

func TestQuotedScalars(t *testing.T) {
	c, err := load(t, `
networks:
  - name: a
    match: { vlan: "7" }
    subnet: 192.0.2.0/24
    blocks: [{ name: b, ranges: [".1", '.2-.3'] }]
`)
	require.NoError(t, err)
	require.Equal(t, "7", c.Networks[0].Selector["vlan"])
	b, _ := c.Networks[0].Block("b")
	require.Equal(t, lan.Ranges{
		{First: addr("192.0.2.1"), Last: addr("192.0.2.1")},
		{First: addr("192.0.2.2"), Last: addr("192.0.2.3")},
	}, b.Ranges)
}
