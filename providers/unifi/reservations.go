package unifi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/lesomnus/lansync/provider"
)

// Reservations live in the controller's own API, in its record of a client:
//
//	GET /api/s/{site}/rest/user            every client the controller has seen
//	PUT /api/s/{site}/rest/user/{_id}      {"use_fixedip", "fixed_ip", "network_id"}
//	POST /api/s/{site}/rest/user           a client it has not seen yet
//	GET /api/s/{site}/stat/sta             connected clients, for addresses in use
//
// The Integration API has clients but no fixed addresses. As with networks,
// a write carries only the fields it changes and the record is compared
// before and after.

var _ provider.ReservationProvider = (*Provider)(nil)

// managedUser are the fields of a client record that SetReservation may change.
var managedUser = []string{"use_fixedip", "fixed_ip", "network_id"}

// volatileUser reports fields of a client record that change on their own as
// the client comes and goes, so a before/after comparison must skip them.
func volatileUser(k string) bool {
	for _, p := range []string{"last_", "disconnect_", "dev_", "fingerprint_"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	switch k {
	case "hostname", "confidence", "os_name", "uptime", "first_seen":
		return true
	}
	return false
}

func (p *Provider) ObserveReservations(ctx context.Context) (*provider.ReservationState, error) {
	if p.legacy == nil {
		return nil, errors.New("unifi: reservations need the controller's own API; set the legacy base URL")
	}
	users, err := p.users(ctx)
	if err != nil {
		return nil, err
	}
	s := &provider.ReservationState{InUse: map[netip.Addr]net.HardwareAddr{}}
	for _, u := range users {
		if u["use_fixedip"] != true {
			continue
		}
		mac, err1 := net.ParseMAC(fmt.Sprint(u["mac"]))
		ip, err2 := netip.ParseAddr(fmt.Sprint(u["fixed_ip"]))
		if err1 != nil || err2 != nil {
			continue
		}
		s.Reservations = append(s.Reservations, provider.Reservation{MAC: mac, IP: ip})
	}

	ref, err := p.siteReference(ctx)
	if err != nil {
		return nil, err
	}
	var sta struct {
		legacyResult
		Data []struct {
			MAC string `json:"mac"`
			IP  string `json:"ip"`
		} `json:"data"`
	}
	if err := p.doURL(ctx, http.MethodGet, p.legacy, "/api/s/"+url.PathEscape(ref)+"/stat/sta", nil, &sta); err != nil {
		return nil, fmt.Errorf("unifi: connected clients: %w", err)
	}
	for _, c := range sta.Data {
		mac, err1 := net.ParseMAC(c.MAC)
		ip, err2 := netip.ParseAddr(c.IP)
		if err1 != nil || err2 != nil {
			continue
		}
		s.InUse[ip] = mac
	}
	return s, nil
}

func (p *Provider) SetReservation(ctx context.Context, u provider.ReservationUpdate) error {
	if p.legacy == nil {
		return errors.New("unifi: reservations need the controller's own API; set the legacy base URL")
	}
	conf, err := p.networkConf(ctx, u.NetworkID)
	if err != nil {
		return fmt.Errorf("unifi: read the network record (nothing was changed): %w", err)
	}
	confID, _ := conf["_id"].(string)
	if confID == "" {
		return errors.New("unifi: the network record has no _id")
	}
	ref, err := p.siteReference(ctx)
	if err != nil {
		return err
	}
	mac := strings.ToLower(u.MAC.String())

	users, err := p.users(ctx)
	if err != nil {
		return fmt.Errorf("unifi: read clients (nothing was changed): %w", err)
	}
	before := findUser(users, mac)

	body := map[string]any{"use_fixedip": true, "fixed_ip": u.IP.String(), "network_id": confID}
	managed := managedUser
	path := "/api/s/" + url.PathEscape(ref) + "/rest/user"
	method := http.MethodPost
	if before != nil {
		id, _ := before["_id"].(string)
		if id == "" {
			return errors.New("unifi: the client record has no _id")
		}
		path += "/" + url.PathEscape(id)
		method = http.MethodPut
		if name, _ := before["name"].(string); name == "" && u.Name != "" {
			body["name"] = u.Name
			// Naming a client also sets its "noted" flag (UniFi Network
			// 10.6). It is part of naming, not a side effect.
			managed = append(append([]string{}, managedUser...), "name", "noted")
		}
	} else {
		body["mac"] = mac
		if u.Name != "" {
			body["name"] = u.Name
		}
	}

	var res legacyResult
	err = p.doURL(ctx, method, p.legacy, path, body, &res)
	if err == nil {
		err = res.err()
	}
	if err != nil {
		return fmt.Errorf("unifi: write the reservation (check the client on the controller): %w", err)
	}

	users, err = p.users(ctx)
	if err != nil {
		return fmt.Errorf("unifi: read clients after the update (the update was sent): %w", err)
	}
	after := findUser(users, mac)
	if after == nil {
		return errors.New("unifi: after the update the controller has no record of the client")
	}

	var problems []string
	if before != nil {
		for _, k := range diffPaths(stable(before, managed), stable(after, managed), "") {
			problems = append(problems, fmt.Sprintf("%s: %s -> %s", k, show(lookup(before, k)), show(lookup(after, k))))
		}
	}
	for k, want := range body {
		if fmt.Sprint(after[k]) != fmt.Sprint(want) {
			problems = append(problems, fmt.Sprintf("%s is %s, want %s", k, show(after[k]), show(want)))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("unifi: after the update the client record is not what was asked; check these on the controller: %s", strings.Join(problems, "; "))
	}
	return nil
}

func (p *Provider) users(ctx context.Context) ([]map[string]any, error) {
	ref, err := p.siteReference(ctx)
	if err != nil {
		return nil, err
	}
	var res struct {
		legacyResult
		Data []map[string]any `json:"data"`
	}
	if err := p.doURL(ctx, http.MethodGet, p.legacy, "/api/s/"+url.PathEscape(ref)+"/rest/user", nil, &res); err != nil {
		return nil, fmt.Errorf("unifi: clients: %w", err)
	}
	return res.Data, nil
}

func findUser(users []map[string]any, mac string) map[string]any {
	for _, u := range users {
		if strings.EqualFold(fmt.Sprint(u["mac"]), mac) {
			return u
		}
	}
	return nil
}

// stable is a client record without the fields an update may change and the
// fields that change on their own.
func stable(m map[string]any, managed []string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if volatileUser(k) {
			continue
		}
		skip := false
		for _, x := range managed {
			if x == k {
				skip = true
			}
		}
		if !skip {
			out[k] = v
		}
	}
	return out
}
