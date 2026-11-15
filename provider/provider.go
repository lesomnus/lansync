// Package provider is the interface between lansync and a network controller.
//
// A provider only reads and writes the controller. Deciding what to change,
// checking that a change is safe and reporting it is done by package plan, so
// every provider gets the same behavior.
package provider

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/lesomnus/lansync/lan"
)

// Capabilities describe what a provider can manage. plan refuses a
// declaration that needs more.
type Capabilities struct {
	// SelectorKeys are the keys a network's match may use, e.g. "vlan".
	SelectorKeys []string
	// DHCPRanges is the number of ranges one DHCP pool may have. Zero means
	// the provider cannot manage DHCP pools.
	DHCPRanges int
	// LeaseTime is true when the provider can manage DHCP lease time.
	LeaseTime bool
	// Reservations is true when the provider can manage reservations.
	Reservations bool
}

// Network is a network as observed on the controller.
type Network struct {
	// ID identifies the network to the provider. It is passed back in
	// NetworkUpdate.
	ID   string
	Name string
	// Subnet is invalid when the controller does not know it.
	Subnet netip.Prefix
	// DHCP is nil when the controller does not run a DHCP server for the
	// network (disabled, relayed, or not managed by it).
	DHCP *DHCP
}

// DHCP is a DHCP server as observed on the controller.
type DHCP struct {
	Ranges    lan.Ranges
	LeaseTime time.Duration
}

// NetworkUpdate is a change to one network. Only the fields that are set are
// changed; the provider must leave everything else on the controller as it
// was.
type NetworkUpdate struct {
	ID string
	// DHCPRanges replaces the DHCP pool when not nil.
	DHCPRanges lan.Ranges
	// LeaseTime replaces the DHCP lease time when not zero.
	LeaseTime time.Duration
}

// Provider reads and writes a network controller.
type Provider interface {
	// Name is a short name for messages, e.g. "unifi".
	Name() string
	Capabilities() Capabilities
	// FindNetworks returns every network that matches all entries of the
	// selector. It returns an error for a key it does not support.
	FindNetworks(ctx context.Context, selector map[string]string) ([]Network, error)
	// UpdateNetwork applies u. It must fail rather than change anything
	// that u does not ask for.
	UpdateNetwork(ctx context.Context, u NetworkUpdate) error
}

// Reservation is a fixed address as observed on the controller.
type Reservation struct {
	MAC net.HardwareAddr
	IP  netip.Addr
}

// ReservationState is what the controller knows about fixed addresses.
//
// It holds addresses and MACs of devices that are not declared, so that plan
// can tell a conflict. plan never prints those; providers must not either.
type ReservationState struct {
	// Reservations are all the fixed-address reservations of the site.
	Reservations []Reservation
	// InUse maps the addresses of connected clients to their MAC.
	InUse map[netip.Addr]net.HardwareAddr
}

// ReservationUpdate makes MAC's fixed address IP in a network.
type ReservationUpdate struct {
	// NetworkID is the provider's ID of the network, as in Network.ID.
	NetworkID string
	MAC       net.HardwareAddr
	IP        netip.Addr
	// Name is the declared name. A provider may use it to label a client
	// that has no name; it must not rename one that has.
	Name string
}

// ReservationProvider is a Provider that manages reservations. Capabilities
// Reservations must be true exactly when a provider implements it.
type ReservationProvider interface {
	Provider
	ObserveReservations(ctx context.Context) (*ReservationState, error)
	// SetReservation creates or changes the reservation of u.MAC. It must
	// leave every other reservation and setting as it was.
	SetReservation(ctx context.Context, u ReservationUpdate) error
}
