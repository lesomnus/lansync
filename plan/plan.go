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
	Mode     lan.Mode `json:"mode"`
	ID       string   `json:"id,omitempty"`
	Status   Status   `json:"status"`
	Changes  []Change `json:"changes,omitempty"`
	Error    string   `json:"error,omitempty"`

	update *provider.NetworkUpdate
}

// Plan is the result for every declared network.
type Plan struct {
	Provider string `json:"provider"`
	// Errors are problems with the declaration as a whole, e.g. it needs a
	// capability the provider does not have. When set, nothing is applied.
	Errors   []string        `json:"errors,omitempty"`
	Networks []NetworkResult `json:"networks"`
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
	return false
}

// HasDrift reports whether any network differs and was not changed.
func (p *Plan) HasDrift() bool {
	for _, n := range p.Networks {
		if n.Status == Drift {
			return true
		}
	}
	return false
}

// Make reads the provider and decides the changes. It changes nothing.
func Make(ctx context.Context, c *lan.Config, p provider.Provider) *Plan {
	caps := p.Capabilities()
	pl := &Plan{Provider: p.Name()}

	for _, e := range checkCapabilities(c, caps, p.Name()) {
		pl.Errors = append(pl.Errors, e.Error())
	}

	for i := range c.Networks {
		n := &c.Networks[i]
		r := NetworkResult{
			Network:  n.Name,
			Selector: n.SelectorString(),
			Mode:     n.Mode,
		}
		if err := planNetwork(ctx, n, p, &r); err != nil {
			r.Status = Failed
			r.Error = err.Error()
		}
		pl.Networks = append(pl.Networks, r)
	}
	return pl
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

// Apply changes the networks whose mode is apply and that drifted. It does
// nothing when the plan has declaration-level errors.
func (pl *Plan) Apply(ctx context.Context, p provider.Provider) {
	if len(pl.Errors) > 0 {
		return
	}
	for i := range pl.Networks {
		r := &pl.Networks[i]
		if r.Status != Drift || r.Mode != lan.Apply || r.update == nil {
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
