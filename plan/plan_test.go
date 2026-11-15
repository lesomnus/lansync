package plan_test

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/lansync/lan"
	"github.com/lesomnus/lansync/plan"
	"github.com/lesomnus/lansync/provider"
	"github.com/lesomnus/lansync/spec"
)

type fake struct {
	caps     provider.Capabilities
	networks map[string]provider.Network // by vlan
	updates  []provider.NetworkUpdate
	failOn   string
}

func (f *fake) Name() string                        { return "fake" }
func (f *fake) Capabilities() provider.Capabilities { return f.caps }

func (f *fake) FindNetworks(_ context.Context, sel map[string]string) ([]provider.Network, error) {
	if v, ok := sel["vlan"]; ok {
		if n, ok := f.networks[v]; ok {
			return []provider.Network{n}, nil
		}
		return nil, nil
	}
	if sel["name"] == "dup" {
		return []provider.Network{{ID: "x", Name: "dup"}, {ID: "y", Name: "dup"}}, nil
	}
	return nil, nil
}

func (f *fake) UpdateNetwork(_ context.Context, u provider.NetworkUpdate) error {
	if u.ID == f.failOn {
		return errors.New("boom")
	}
	f.updates = append(f.updates, u)
	return nil
}

func r(a, b string) lan.Range {
	return lan.Range{First: netip.MustParseAddr(a), Last: netip.MustParseAddr(b)}
}

func newFake() *fake {
	return &fake{
		caps: provider.Capabilities{SelectorKeys: []string{"vlan", "name"}, DHCPRanges: 1, LeaseTime: true},
		networks: map[string]provider.Network{
			"2": {ID: "n2", Name: "F2", Subnet: netip.MustParsePrefix("192.0.2.1/24"),
				DHCP: &provider.DHCP{Ranges: lan.Ranges{r("192.0.2.6", "192.0.2.254")}, LeaseTime: 24 * time.Hour}},
			"3": {ID: "n3", Name: "F3", Subnet: netip.MustParsePrefix("198.51.100.1/24"),
				DHCP: &provider.DHCP{Ranges: lan.Ranges{r("198.51.100.100", "198.51.100.199")}, LeaseTime: 8 * time.Hour}},
			"4": {ID: "n4", Name: "F4", Subnet: netip.MustParsePrefix("203.0.113.1/24"),
				DHCP: &provider.DHCP{Ranges: lan.Ranges{r("203.0.113.6", "203.0.113.254")}, LeaseTime: 24 * time.Hour}},
			"5": {ID: "n5", Name: "relay", Subnet: netip.MustParsePrefix("192.0.2.1/24")},
		},
	}
}

func cfg(t *testing.T, src string) *lan.Config {
	t.Helper()
	c, err := spec.Load(strings.NewReader(src))
	require.NoError(t, err)
	return c
}

const twoNetworks = `
layouts:
  office:
    blocks:
      - { name: dhcp, ranges: [.100-.199] }
networks:
  - { name: F2, match: { vlan: 2 }, subnet: 192.0.2.0/24, layout: office, dhcp: { block: dhcp, leaseTime: 8h }, mode: apply }
  - { name: F3, match: { vlan: 3 }, subnet: 198.51.100.0/24, layout: office, dhcp: { block: dhcp, leaseTime: 8h }, mode: apply }
  - { name: F4, match: { vlan: 4 }, subnet: 203.0.113.0/24, layout: office, dhcp: { block: dhcp, leaseTime: 8h } }
`

func TestPlanAndApply(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	pl := plan.Make(ctx, cfg(t, twoNetworks), f)
	require.Empty(t, pl.Errors)
	require.Len(t, pl.Networks, 3)

	f2, f3, f4 := pl.Networks[0], pl.Networks[1], pl.Networks[2]
	require.Equal(t, plan.Drift, f2.Status)
	require.Equal(t, []plan.Change{
		{Field: "dhcp.ranges", From: "192.0.2.6-192.0.2.254", To: "192.0.2.100-192.0.2.199"},
		{Field: "dhcp.leaseTime", From: "24h0m0s", To: "8h0m0s"},
	}, f2.Changes)
	require.Equal(t, plan.InSync, f3.Status)
	require.Equal(t, plan.Drift, f4.Status)
	require.True(t, pl.HasDrift())
	require.Empty(t, f.updates, "Make changes nothing")

	pl.Apply(ctx, f)
	require.Equal(t, plan.Applied, pl.Networks[0].Status)
	require.Equal(t, plan.InSync, pl.Networks[1].Status)
	require.Equal(t, plan.Drift, pl.Networks[2].Status, "dry-run network is left as is")
	require.Equal(t, []provider.NetworkUpdate{{
		ID:         "n2",
		DHCPRanges: lan.Ranges{r("192.0.2.100", "192.0.2.199")},
		LeaseTime:  8 * time.Hour,
	}}, f.updates)
	require.False(t, pl.Failed())
}

func TestApplyFailure(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.failOn = "n2"
	pl := plan.Make(ctx, cfg(t, twoNetworks), f)
	pl.Apply(ctx, f)
	require.Equal(t, plan.Failed, pl.Networks[0].Status)
	require.Contains(t, pl.Networks[0].Error, "boom")
	require.True(t, pl.Failed())
}

func TestPlanNetworkProblems(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		net  string
		want string
	}{
		{"not found", "{ name: x, match: { vlan: 9 }, subnet: 192.0.2.0/24 }", "no network matches vlan=9"},
		{"ambiguous", "{ name: x, match: { name: dup }, subnet: 192.0.2.0/24 }", "2 networks match"},
		{"subnet differs", "{ name: x, match: { vlan: 2 }, subnet: 198.51.100.0/24 }", "subnet differs"},
		{"no dhcp server", "{ name: x, match: { vlan: 5 }, subnet: 192.0.2.0/24, blocks: [{ name: p, ranges: [.10-.20] }], dhcp: { block: p } }", "does not enable one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pl := plan.Make(ctx, cfg(t, "networks:\n  - "+tt.net+"\n"), newFake())
			require.Equal(t, plan.Failed, pl.Networks[0].Status)
			require.Contains(t, pl.Networks[0].Error, tt.want)
		})
	}
}

func TestCapabilities(t *testing.T) {
	ctx := context.Background()
	c := cfg(t, `
networks:
  - name: x
    match: { vlan: 2, colour: blue }
    subnet: 192.0.2.0/24
    blocks: [{ name: p, ranges: [.10-.20, .30-.40] }]
    dhcp: { block: p }
    mode: apply
reservations:
  - { name: r, mac: '00:00:5e:00:53:01', network: x, ip: .50 }
`)
	f := newFake()
	pl := plan.Make(ctx, c, f)
	all := strings.Join(pl.Errors, "\n")
	require.Contains(t, all, `cannot match by "colour"`)
	require.Contains(t, all, "has 2 ranges but fake supports 1")
	require.Contains(t, all, "cannot manage reservations")

	pl.Apply(ctx, f)
	require.Empty(t, f.updates, "nothing is applied when the declaration needs more than the provider has")
}
