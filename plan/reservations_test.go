package plan_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/lansync/plan"
	"github.com/lesomnus/lansync/provider"
)

type fakeR struct {
	*fake
	state  provider.ReservationState
	sets   []provider.ReservationUpdate
	failOn string // MAC
}

func (f *fakeR) ObserveReservations(context.Context) (*provider.ReservationState, error) {
	return &f.state, nil
}

func (f *fakeR) SetReservation(_ context.Context, u provider.ReservationUpdate) error {
	if u.MAC.String() == f.failOn {
		return errors.New("nope")
	}
	f.sets = append(f.sets, u)
	return nil
}

func mac(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

func newFakeR() *fakeR {
	f := newFake()
	f.caps.Reservations = true
	return &fakeR{fake: f, state: provider.ReservationState{
		Reservations: []provider.Reservation{
			{MAC: mac("00:00:5e:00:53:01"), IP: ip("192.0.2.11")}, // ap-01, in place
			{MAC: mac("00:00:5e:00:53:02"), IP: ip("192.0.2.36")}, // ap-02, elsewhere
			{MAC: mac("00:00:5e:00:53:99"), IP: ip("192.0.2.14")}, // undeclared
		},
		InUse: map[netip.Addr]net.HardwareAddr{
			ip("192.0.2.15"): mac("00:00:5e:00:53:98"), // undeclared, dynamic
			ip("192.0.2.16"): mac("00:00:5e:00:53:04"), // ap-04 itself
		},
	}}
}

const withReservations = `
layouts:
  office:
    blocks:
      - { name: infra, ranges: [.10-.49] }
      - { name: dhcp, ranges: [.100-.199] }
networks:
  - { name: F2, match: { vlan: 2 }, subnet: 192.0.2.0/24, layout: office, dhcp: { block: dhcp, leaseTime: 8h } }
reservations:
  - { name: ap-01, mac: "00:00:5e:00:53:01", network: F2, block: infra, ip: .11 }
  - { name: ap-02, mac: "00:00:5e:00:53:02", network: F2, block: infra, ip: .12 }
  - { name: ap-03, mac: "00:00:5e:00:53:03", network: F2, block: infra, ip: .13 }
  - { name: ap-04, mac: "00:00:5e:00:53:04", network: F2, block: infra, ip: .16 }
`

func byName(pl *plan.Plan) map[string]plan.ReservationResult {
	m := map[string]plan.ReservationResult{}
	for _, r := range pl.Reservations {
		m[r.Reservation] = r
	}
	return m
}

func TestReservationsPlanAndApply(t *testing.T) {
	ctx := context.Background()
	f := newFakeR()
	pl := plan.Make(ctx, cfg(t, withReservations), f, plan.Targets{Names: []string{"F2"}})
	require.Empty(t, pl.Errors)
	rs := byName(pl)

	require.Equal(t, plan.InSync, rs["ap-01"].Status)
	require.Equal(t, plan.Drift, rs["ap-02"].Status)
	require.Equal(t, []plan.Change{{Field: "ip", From: "192.0.2.36", To: "192.0.2.12"}}, rs["ap-02"].Changes)
	require.Equal(t, []plan.Change{{Field: "ip", From: "(none)", To: "192.0.2.13"}}, rs["ap-03"].Changes)
	require.Equal(t, plan.Drift, rs["ap-04"].Status, "the device itself holding the address is fine")

	pl.Apply(ctx, f)
	require.Len(t, f.sets, 3)
	require.Equal(t, "n2", f.sets[0].NetworkID)
	require.Equal(t, plan.Applied, pl.Networks[0].Status, "the pool changes after its reservations")
}

func TestReservationConflicts(t *testing.T) {
	ctx := context.Background()
	f := newFakeR()
	src := withReservations + `  - { name: taken, mac: "00:00:5e:00:53:05", network: F2, block: infra, ip: .14 }
  - { name: busy,  mac: "00:00:5e:00:53:06", network: F2, block: infra, ip: .15 }
  - { name: swap,  mac: "00:00:5e:00:53:07", network: F2, block: infra, ip: .36 }
`
	pl := plan.Make(ctx, cfg(t, src), f, plan.Targets{All: true})
	rs := byName(pl)

	require.Equal(t, plan.Failed, rs["taken"].Status)
	require.Equal(t, "192.0.2.14 is already reserved for a device that is not declared", rs["taken"].Error)
	require.Equal(t, plan.Failed, rs["busy"].Status)
	require.Contains(t, rs["busy"].Error, "in use by a device that is not declared")
	require.Equal(t, plan.Failed, rs["swap"].Status)
	require.Contains(t, rs["swap"].Error, "reserved for reservation ap-02")

	// Undeclared devices are never named.
	for _, r := range pl.Reservations {
		require.NotContains(t, r.Error, ":53:9")
	}

	pl.Apply(ctx, f)
	require.Equal(t, plan.Drift, pl.Networks[0].Status)
	require.Contains(t, pl.Networks[0].Error, "not applied: reservation")
	require.Empty(t, f.updates, "the pool is left alone when a reservation of the network failed")
}

func TestReservationFailureBlocksThePool(t *testing.T) {
	ctx := context.Background()
	f := newFakeR()
	f.failOn = "00:00:5e:00:53:03"
	pl := plan.Make(ctx, cfg(t, withReservations), f, plan.Targets{All: true})
	pl.Apply(ctx, f)
	require.Equal(t, plan.Failed, byName(pl)["ap-03"].Status)
	require.Empty(t, f.updates)
	require.True(t, pl.Failed())
}

func TestReservationsDryRun(t *testing.T) {
	ctx := context.Background()
	f := newFakeR()
	pl := plan.Make(ctx, cfg(t, withReservations), f, plan.Targets{})
	pl.Apply(ctx, f)
	require.Empty(t, f.sets)
	require.Equal(t, plan.DryRun, pl.Reservations[0].Mode)
	require.True(t, pl.HasDrift())
	require.False(t, strings.Contains(strings.Join(pl.Errors, ""), "reservations"))
}
