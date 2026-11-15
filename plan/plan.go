// Package plan compares a declaration with what a provider observes, decides
// what to change, and applies it.
package plan

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/lesomnus/lansync/lan"
	"github.com/lesomnus/lansync/provider"
)

// Mode says what lansync may do to a network in this run.
type Mode string

const (
	// DryRun reports differences and never changes the network.
	DryRun Mode = "dry-run"
	// Apply makes the network match the declaration.
	Apply Mode = "apply"
)

// Targets selects the networks that Apply may change. Whether to change a
// network is decided by whoever runs lansync, not by the declaration: the
// declaration says what the network should be.
type Targets struct {
	All   bool
	Names []string
}

func (t Targets) has(name string) bool {
	return t.All || slices.Contains(t.Names, name)
}

// Status is the outcome for one network.
type Status string

const (
	// InSync: the network already matches the declaration.
	InSync Status = "in-sync"
	// Drift: the network differs and was not changed (dry-run, or `plan`).
	Drift Status = "drift"
	// Applied: the network differed and was changed.
	Applied Status = "applied"
	// Failed: the network could not be checked or changed. Nothing was
	// changed unless the error says so.
	Failed Status = "failed"
)

// Change is one field that differs.
type Change struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// NetworkResult is the plan, and after Apply the outcome, for one network.
type NetworkResult struct {
	Network  string   `json:"network"`
	Selector string   `json:"selector"`
	Mode     Mode     `json:"mode"`
	ID       string   `json:"id,omitempty"`
	Status   Status   `json:"status"`
	Changes  []Change `json:"changes,omitempty"`
	Error    string   `json:"error,omitempty"`

	update *provider.NetworkUpdate
}

// ReservationResult is the plan, and after Apply the outcome, for one
// declared reservation. It never carries the MAC or address of a device that
// is not declared.
type ReservationResult struct {
	Reservation string   `json:"reservation"`
	Network     string   `json:"network"`
	IP          string   `json:"ip"`
	Mode        Mode     `json:"mode"`
	Status      Status   `json:"status"`
	Changes     []Change `json:"changes,omitempty"`
	Error       string   `json:"error,omitempty"`

	update *provider.ReservationUpdate
}

// Plan is the result for every declared network and reservation.
type Plan struct {
	Provider string `json:"provider"`
	// Errors are problems with the declaration as a whole, e.g. it needs a
	// capability the provider does not have. When set, nothing is applied.
	Errors       []string            `json:"errors,omitempty"`
	Networks     []NetworkResult     `json:"networks"`
	Reservations []ReservationResult `json:"reservations,omitempty"`
}

// Failed reports whether anything failed.
func (p *Plan) Failed() bool {
	if len(p.Errors) > 0 {
		return true
	}
	for _, n := range p.Networks {
		if n.Status == Failed {
			return true
		}
	}
	for _, r := range p.Reservations {
		if r.Status == Failed {
			return true
		}
	}
	return false
}

// HasDrift reports whether any network differs and was not changed.
func (p *Plan) HasDrift() bool {
	for _, n := range p.Networks {
		if n.Status == Drift {
			return true
		}
	}
	for _, r := range p.Reservations {
		if r.Status == Drift {
			return true
		}
	}
	return false
}

// Make reads the provider and decides the changes. It changes nothing.
// Networks in targets are marked to be changed by Apply.
func Make(ctx context.Context, c *lan.Config, p provider.Provider, targets Targets) *Plan {
	caps := p.Capabilities()
	pl := &Plan{Provider: p.Name()}

	for _, e := range checkCapabilities(c, caps, p.Name()) {
		pl.Errors = append(pl.Errors, e.Error())
	}
	for _, name := range targets.Names {
		if !slices.ContainsFunc(c.Networks, func(n lan.Network) bool { return n.Name == name }) {
			pl.Errors = append(pl.Errors, fmt.Sprintf("network %q is not declared", name))
		}
	}

	for i := range c.Networks {
		n := &c.Networks[i]
		r := NetworkResult{
			Network:  n.Name,
			Selector: n.SelectorString(),
			Mode:     DryRun,
		}
		if targets.has(n.Name) {
			r.Mode = Apply
		}
		if err := planNetwork(ctx, n, p, &r); err != nil {
			r.Status = Failed
			r.Error = err.Error()
		}
		pl.Networks = append(pl.Networks, r)
	}

	if len(c.Reservations) > 0 && caps.Reservations {
		planReservations(ctx, c, p, pl, targets)
	}
	return pl
}

func planReservations(ctx context.Context, c *lan.Config, p provider.Provider, pl *Plan, targets Targets) {
	rp, ok := p.(provider.ReservationProvider)
	var state *provider.ReservationState
	var err error
	if !ok {
		err = fmt.Errorf("%s says it manages reservations but does not implement them", p.Name())
	} else {
		state, err = rp.ObserveReservations(ctx)
	}

	declared := map[string]string{} // MAC -> declared name
	for _, d := range c.Reservations {
		declared[d.MAC.String()] = d.Name
	}
	networks := map[string]*NetworkResult{}
	for i := range pl.Networks {
		networks[pl.Networks[i].Network] = &pl.Networks[i]
	}

	for _, d := range c.Reservations {
		r := ReservationResult{
			Reservation: d.Name,
			Network:     d.Network,
			IP:          d.IP.String(),
			Mode:        DryRun,
		}
		if targets.has(d.Network) {
			r.Mode = Apply
		}
		n := networks[d.Network]
		switch {
		case err != nil:
			r.Status, r.Error = Failed, fmt.Sprintf("observe reservations: %v", err)
		case n == nil || n.Status == Failed:
			r.Status, r.Error = Failed, "its network could not be checked"
		default:
			planReservation(d, n.ID, state, declared, &r)
		}
		pl.Reservations = append(pl.Reservations, r)
	}
}

func planReservation(d lan.Reservation, networkID string, s *provider.ReservationState, declared map[string]string, r *ReservationResult) {
	mac := d.MAC.String()
	var current *provider.Reservation
	for i, o := range s.Reservations {
		if o.MAC.String() == mac {
			current = &s.Reservations[i]
			break
		}
	}
	if current != nil && current.IP == d.IP {
		r.Status = InSync
		return
	}

	// The address must be free. Say who holds it only when that is
	// declared too; an undeclared device stays anonymous.
	holder := func(other string) string {
		if name, ok := declared[other]; ok {
			return "reservation " + name
		}
		return "a device that is not declared"
	}
	for _, o := range s.Reservations {
		if o.IP == d.IP && o.MAC.String() != mac {
			r.Status = Failed
			r.Error = fmt.Sprintf("%s is already reserved for %s", d.IP, holder(o.MAC.String()))
			return
		}
	}
	if o, ok := s.InUse[d.IP]; ok && o.String() != mac {
		r.Status = Failed
		r.Error = fmt.Sprintf("%s is in use by %s; wait for its lease to end or move it first", d.IP, holder(o.String()))
		return
	}

	from := "(none)"
	if current != nil {
		from = current.IP.String()
	}
	r.Changes = []Change{{Field: "ip", From: from, To: d.IP.String()}}
	r.Status = Drift
	r.update = &provider.ReservationUpdate{NetworkID: networkID, MAC: d.MAC, IP: d.IP, Name: d.Name}
}

func checkCapabilities(c *lan.Config, caps provider.Capabilities, name string) []error {
	var errs []error
	for _, n := range c.Networks {
		for k := range n.Selector {
			if !slices.Contains(caps.SelectorKeys, k) {
				errs = append(errs, fmt.Errorf("network %s: %s cannot match by %q (supported: %v)", n.Name, name, k, caps.SelectorKeys))
			}
		}
		if n.DHCP == nil {
			continue
		}
		if caps.DHCPRanges == 0 {
			errs = append(errs, fmt.Errorf("network %s: %s cannot manage DHCP pools", n.Name, name))
		} else if len(n.DHCP.Ranges) > caps.DHCPRanges {
			errs = append(errs, fmt.Errorf("network %s: DHCP block %s has %d ranges but %s supports %d per pool",
				n.Name, n.DHCP.Block, len(n.DHCP.Ranges), name, caps.DHCPRanges))
		}
		if n.DHCP.LeaseTime != 0 && !caps.LeaseTime {
			errs = append(errs, fmt.Errorf("network %s: %s cannot manage DHCP lease time", n.Name, name))
		}
	}
	if len(c.Reservations) > 0 && !caps.Reservations {
		errs = append(errs, fmt.Errorf("%s cannot manage reservations yet; remove them from the declaration", name))
	}
	return errs
}

func planNetwork(ctx context.Context, n *lan.Network, p provider.Provider, r *NetworkResult) error {
	found, err := p.FindNetworks(ctx, n.Selector)
	if err != nil {
		return fmt.Errorf("find: %w", err)
	}
	switch len(found) {
	case 0:
		return fmt.Errorf("no network matches %s", n.SelectorString())
	case 1:
	default:
		ids := make([]string, len(found))
		for i, f := range found {
			ids[i] = fmt.Sprintf("%s (%s)", f.Name, f.ID)
		}
		return fmt.Errorf("%d networks match %s: %v; make the match narrower", len(found), n.SelectorString(), ids)
	}
	o := found[0]
	r.ID = o.ID

	// The subnet is how we know the declaration and the controller talk
	// about the same address space. Never guess past a mismatch.
	if !o.Subnet.IsValid() {
		return errors.New("the controller does not report a subnet for this network")
	}
	if o.Subnet.Masked() != n.Subnet {
		return fmt.Errorf("subnet differs: declared %s, controller has %s", n.Subnet, o.Subnet.Masked())
	}

	u := provider.NetworkUpdate{ID: o.ID}
	if d := n.DHCP; d != nil {
		if o.DHCP == nil {
			// Turning a DHCP server on (or off a relay) changes who answers
			// on the segment. That is not a range change; do it by hand.
			return errors.New("the controller does not run a DHCP server for this network; lansync does not enable one")
		}
		if !d.Ranges.Equal(o.DHCP.Ranges) {
			r.Changes = append(r.Changes, Change{Field: "dhcp.ranges", From: o.DHCP.Ranges.String(), To: d.Ranges.String()})
			u.DHCPRanges = d.Ranges
		}
		if d.LeaseTime != 0 && d.LeaseTime != o.DHCP.LeaseTime {
			r.Changes = append(r.Changes, Change{Field: "dhcp.leaseTime", From: formatDuration(o.DHCP.LeaseTime), To: formatDuration(d.LeaseTime)})
			u.LeaseTime = d.LeaseTime
		}
	}

	if len(r.Changes) == 0 {
		r.Status = InSync
		return nil
	}
	r.Status = Drift
	r.update = &u
	return nil
}

// Apply changes the targeted networks that drifted. It does nothing when the
// plan has declaration-level errors.
//
// A network's reservations are applied before its DHCP pool, and the pool is
// left alone if any of its reservations failed: shrinking a pool moves the
// devices outside it, and their reservations are what keep them in place.
func (pl *Plan) Apply(ctx context.Context, p provider.Provider) {
	if len(pl.Errors) > 0 {
		return
	}
	blocked := map[string]string{}
	rp, _ := p.(provider.ReservationProvider)
	for i := range pl.Reservations {
		r := &pl.Reservations[i]
		if r.Mode != Apply {
			continue
		}
		if r.Status == Failed {
			blocked[r.Network] = r.Reservation
			continue
		}
		if r.Status != Drift || r.update == nil || rp == nil {
			continue
		}
		if err := rp.SetReservation(ctx, *r.update); err != nil {
			r.Status = Failed
			r.Error = fmt.Sprintf("apply: %v", err)
			blocked[r.Network] = r.Reservation
			continue
		}
		r.Status = Applied
	}
	for i := range pl.Networks {
		r := &pl.Networks[i]
		if r.Status != Drift || r.Mode != Apply || r.update == nil {
			continue
		}
		if name, ok := blocked[r.Network]; ok {
			r.Error = fmt.Sprintf("not applied: reservation %s failed", name)
			continue
		}
		if err := p.UpdateNetwork(ctx, *r.update); err != nil {
			r.Status = Failed
			r.Error = fmt.Sprintf("apply: %v", err)
			continue
		}
		r.Status = Applied
	}
}

func formatDuration(d time.Duration) string {
	if d == 0 {
		return "(unset)"
	}
	return d.String()
}
