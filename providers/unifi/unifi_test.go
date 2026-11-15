package unifi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/lansync/lan"
	"github.com/lesomnus/lansync/provider"
	"github.com/lesomnus/lansync/providers/unifi"
)

const siteID = "88f7af54-98f8-306a-a1c7-c9349722b1f6"

// controller plays a UniFi console. It keeps one record per network, the way
// the controller does, and serves two views of it: the full record of its own
// API (rest/networkconf) and the Integration API's network.
type controller struct {
	t  *testing.T
	mu sync.Mutex
	// records, each with "_id" and "external_id" (the Integration id).
	records []map[string]any
	puts    []map[string]any
	// onPut, when set, changes a record after a PUT, to play a controller
	// that does more than it was asked.
	onPut func(map[string]any)
}

func record(n int, name string, vlan int, subnet string, dhcp bool) map[string]any {
	return map[string]any{
		"_id":                strings.Repeat("0", 23) + strconv.Itoa(n),
		"external_id":        "11111111-0000-0000-0000-00000000000" + strconv.Itoa(n),
		"name":               name,
		"vlan":               vlan,
		"ip_subnet":          subnet,
		"purpose":            "corporate",
		"gateway_type":       "switch",
		"mdns_enabled":       true,
		"igmp_snooping":      false,
		"upnp_lan_enabled":   false,
		"dhcpd_dns_enabled":  false,
		"dhcpd_enabled":      dhcp,
		"dhcp_relay_enabled": false,
		"dhcpd_start":        "192.0.2.6",
		"dhcpd_stop":         "192.0.2.254",
		"dhcpd_leasetime":    86400,
	}
}

func newController(t *testing.T) *controller {
	relay := record(3, "Relay", 3, "198.51.100.1/24", false)
	relay["dhcp_relay_enabled"] = true
	return &controller{t: t, records: []map[string]any{
		record(2, "F2", 2, "192.0.2.1/24", true),
		relay,
		record(4, "NoDHCP", 4, "203.0.113.1/24", false),
	}}
}

// integration renders the Integration API's view of a record.
func integration(r map[string]any) map[string]any {
	host, bits, _ := strings.Cut(r["ip_subnet"].(string), "/")
	prefix, _ := strconv.Atoi(bits)
	v4 := map[string]any{"hostIpAddress": host, "prefixLength": prefix, "autoScaleEnabled": true}
	switch {
	case r["dhcp_relay_enabled"] == true:
		v4["dhcpConfiguration"] = map[string]any{"mode": "RELAY", "dhcpServerIpAddresses": []any{"198.51.100.250"}}
	case r["dhcpd_enabled"] == true:
		v4["dhcpConfiguration"] = map[string]any{
			"mode":             "SERVER",
			"ipAddressRange":   map[string]any{"start": r["dhcpd_start"], "stop": r["dhcpd_stop"]},
			"leaseTimeSeconds": r["dhcpd_leasetime"],
		}
	}
	return map[string]any{
		"id": r["external_id"], "name": r["name"], "management": "SWITCH", "enabled": true,
		"vlanId": r["vlan"], "default": false, "metadata": map[string]any{"origin": "USER_DEFINED"},
		"ipv4Configuration": v4,
	}
}

func (c *controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.Header.Get("X-API-KEY") != "secret" {
		http.Error(w, `{"statusCode":401}`, http.StatusUnauthorized)
		return
	}
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	paged := func(items []any) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit == 0 || limit > 2 {
			limit = 2 // force pagination
		}
		end := min(offset+limit, len(items))
		write(map[string]any{"offset": offset, "limit": limit, "count": end - offset, "totalCount": len(items), "data": items[offset:end]})
	}

	const integ = "/proxy/network/integration"
	const own = "/proxy/network/api/s/default/rest/networkconf"
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && p == own:
		data := make([]any, len(c.records))
		for i, rec := range c.records {
			data[i] = rec
		}
		write(map[string]any{"meta": map[string]any{"rc": "ok"}, "data": data})

	case r.Method == http.MethodPut && strings.HasPrefix(p, own+"/"):
		id := strings.TrimPrefix(p, own+"/")
		for _, rec := range c.records {
			if rec["_id"] != id {
				continue
			}
			var body map[string]any
			require.NoError(c.t, json.NewDecoder(r.Body).Decode(&body))
			c.puts = append(c.puts, body)
			for k, v := range body {
				rec[k] = v
			}
			if c.onPut != nil {
				c.onPut(rec)
			}
			write(map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{rec}})
			return
		}
		write(map[string]any{"meta": map[string]any{"rc": "error", "msg": "api.err.IdInvalid"}, "data": []any{}})

	case r.Method == http.MethodGet && p == integ+"/v1/sites":
		paged([]any{
			map[string]any{"id": "99999999-0000-0000-0000-000000000000", "internalReference": "other", "name": "Other"},
			map[string]any{"id": siteID, "internalReference": "default", "name": "Default"},
		})
	case r.Method == http.MethodGet && p == integ+"/v1/sites/"+siteID+"/networks":
		items := make([]any, len(c.records))
		for i, rec := range c.records {
			v := integration(rec)
			items[i] = map[string]any{"id": v["id"], "name": v["name"], "management": v["management"], "vlanId": v["vlanId"]}
		}
		paged(items)
	case r.Method == http.MethodGet && strings.HasPrefix(p, integ+"/v1/sites/"+siteID+"/networks/"):
		id := strings.TrimPrefix(p, integ+"/v1/sites/"+siteID+"/networks/")
		for _, rec := range c.records {
			if rec["external_id"] == id {
				write(integration(rec))
				return
			}
		}
		http.Error(w, `{"statusCode":404}`, http.StatusNotFound)
	default:
		c.t.Errorf("unexpected %s %s", r.Method, p)
		http.Error(w, `{"statusCode":404}`, http.StatusNotFound)
	}
}

func setup(t *testing.T) (*controller, *unifi.Provider) {
	c := newController(t)
	srv := httptest.NewTLSServer(c)
	t.Cleanup(srv.Close)
	p, err := unifi.New(unifi.Config{
		BaseURL: srv.URL + "/proxy/network/integration",
		APIKey:  "secret",
		Client:  srv.Client(),
	})
	require.NoError(t, err)
	return c, p
}

func rng(a, b string) lan.Range {
	return lan.Range{First: netip.MustParseAddr(a), Last: netip.MustParseAddr(b)}
}

const f2 = "11111111-0000-0000-0000-000000000002"

func TestFindNetworks(t *testing.T) {
	ctx := context.Background()
	_, p := setup(t)

	ns, err := p.FindNetworks(ctx, map[string]string{"vlan": "2"})
	require.NoError(t, err)
	require.Equal(t, []provider.Network{{
		ID:     f2,
		Name:   "F2",
		Subnet: netip.MustParsePrefix("192.0.2.0/24"),
		DHCP:   &provider.DHCP{Ranges: lan.Ranges{rng("192.0.2.6", "192.0.2.254")}, LeaseTime: 24 * time.Hour},
	}}, ns)

	ns, err = p.FindNetworks(ctx, map[string]string{"name": "Relay"})
	require.NoError(t, err)
	require.Len(t, ns, 1)
	require.Nil(t, ns[0].DHCP, "a relay is not a DHCP server")

	ns, err = p.FindNetworks(ctx, map[string]string{"vlan": "4"})
	require.NoError(t, err)
	require.Len(t, ns, 1)
	require.Nil(t, ns[0].DHCP)

	ns, err = p.FindNetworks(ctx, map[string]string{"vlan": "2", "name": "F3"})
	require.NoError(t, err)
	require.Empty(t, ns, "every selector entry must match")

	_, err = p.FindNetworks(ctx, map[string]string{"colour": "blue"})
	require.ErrorContains(t, err, `unsupported match key "colour"`)
}

func TestUpdateNetworkSendsOnlyTheChangedFields(t *testing.T) {
	ctx := context.Background()
	c, p := setup(t)

	err := p.UpdateNetwork(ctx, provider.NetworkUpdate{
		ID:         f2,
		DHCPRanges: lan.Ranges{rng("192.0.2.100", "192.0.2.199")},
		LeaseTime:  8 * time.Hour,
	})
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{
		"dhcpd_start":     "192.0.2.100",
		"dhcpd_stop":      "192.0.2.199",
		"dhcpd_leasetime": float64(28800),
	}}, c.puts)

	ns, err := p.FindNetworks(ctx, map[string]string{"vlan": "2"})
	require.NoError(t, err)
	require.Equal(t, lan.Ranges{rng("192.0.2.100", "192.0.2.199")}, ns[0].DHCP.Ranges)
	require.Equal(t, 8*time.Hour, ns[0].DHCP.LeaseTime)
	require.Equal(t, true, c.records[0]["mdns_enabled"], "settings not sent are kept")
}

func TestUpdateNetworkLeaseOnly(t *testing.T) {
	ctx := context.Background()
	c, p := setup(t)
	require.NoError(t, p.UpdateNetwork(ctx, provider.NetworkUpdate{ID: f2, LeaseTime: time.Hour}))
	require.Equal(t, []map[string]any{{"dhcpd_leasetime": float64(3600)}}, c.puts)
}

func TestUpdateNetworkDetectsSideEffects(t *testing.T) {
	ctx := context.Background()
	c, p := setup(t)
	// What the Integration API's PUT did on a real console.
	c.onPut = func(r map[string]any) {
		r["mdns_enabled"] = false
		delete(r, "igmp_snooping")
	}
	err := p.UpdateNetwork(ctx, provider.NetworkUpdate{ID: f2, DHCPRanges: lan.Ranges{rng("192.0.2.100", "192.0.2.199")}})
	require.ErrorContains(t, err, "igmp_snooping: false -> (none)")
	require.ErrorContains(t, err, "mdns_enabled: true -> false")
}

func TestUpdateNetworkChecksTheValues(t *testing.T) {
	ctx := context.Background()
	c, p := setup(t)
	c.onPut = func(r map[string]any) { r["dhcpd_stop"] = "192.0.2.254" }
	err := p.UpdateNetwork(ctx, provider.NetworkUpdate{ID: f2, DHCPRanges: lan.Ranges{rng("192.0.2.100", "192.0.2.199")}})
	require.ErrorContains(t, err, `dhcpd_stop is "192.0.2.254", want 192.0.2.199`)
}

func TestUpdateNetworkRefuses(t *testing.T) {
	ctx := context.Background()
	c, p := setup(t)
	err := p.UpdateNetwork(ctx, provider.NetworkUpdate{ID: "11111111-0000-0000-0000-000000000003", LeaseTime: time.Hour})
	require.ErrorContains(t, err, "relays DHCP")
	err = p.UpdateNetwork(ctx, provider.NetworkUpdate{ID: "11111111-0000-0000-0000-000000000004", LeaseTime: time.Hour})
	require.ErrorContains(t, err, "DHCP server is off")
	err = p.UpdateNetwork(ctx, provider.NetworkUpdate{ID: "nope", LeaseTime: time.Hour})
	require.ErrorContains(t, err, "0 networkconf records")
	require.Empty(t, c.puts)
}

func TestUpdateNetworkNeedsLegacyRoot(t *testing.T) {
	c := newController(t)
	srv := httptest.NewTLSServer(c)
	defer srv.Close()
	p, err := unifi.New(unifi.Config{BaseURL: srv.URL + "/somewhere", APIKey: "secret", Client: srv.Client()})
	require.NoError(t, err)
	err = p.UpdateNetwork(context.Background(), provider.NetworkUpdate{ID: f2, LeaseTime: time.Hour})
	require.ErrorContains(t, err, "legacy base URL")
	require.Empty(t, c.puts)
}

func TestWrongKey(t *testing.T) {
	c := newController(t)
	srv := httptest.NewTLSServer(c)
	defer srv.Close()
	p, err := unifi.New(unifi.Config{BaseURL: srv.URL + "/proxy/network/integration", APIKey: "nope", Client: srv.Client()})
	require.NoError(t, err)
	_, err = p.FindNetworks(context.Background(), map[string]string{"vlan": "2"})
	require.ErrorContains(t, err, "401")
}
