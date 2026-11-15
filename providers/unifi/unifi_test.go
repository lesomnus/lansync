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

// controller is a minimal Integration API: one site, networks kept as raw
// JSON objects the way the controller returns them.
type controller struct {
	t        *testing.T
	mu       sync.Mutex
	networks []map[string]any
	puts     []map[string]any
	// mutate, when set, changes the stored network after a PUT, to play a
	// controller that does more than it was asked.
	mutate func(map[string]any)
}

func network(id, name string, vlan int, host string, prefix int, dhcp map[string]any) map[string]any {
	v4 := map[string]any{
		"autoScaleEnabled": false,
		"hostIpAddress":    host,
		"prefixLength":     prefix,
		"natOutboundIpAddressConfiguration": []any{
			map[string]any{"type": "AUTO", "wanInterfaceId": "aaaaaaaa-0000-0000-0000-000000000001"},
		},
	}
	if dhcp != nil {
		v4["dhcpConfiguration"] = dhcp
	}
	return map[string]any{
		"id":                    id,
		"name":                  name,
		"management":            "GATEWAY",
		"enabled":               true,
		"default":               false,
		"vlanId":                vlan,
		"metadata":              map[string]any{"origin": "USER_DEFINED"},
		"isolationEnabled":      false,
		"internetAccessEnabled": true,
		"cellularBackupEnabled": true,
		"zoneId":                "bbbbbbbb-0000-0000-0000-000000000001",
		"ipv4Configuration":     v4,
	}
}

func server() map[string]any {
	return map[string]any{
		"mode":                         "SERVER",
		"ipAddressRange":               map[string]any{"start": "192.0.2.6", "stop": "192.0.2.254"},
		"leaseTimeSeconds":             86400,
		"pingConflictDetectionEnabled": true,
	}
}

func newController(t *testing.T) *controller {
	return &controller{t: t, networks: []map[string]any{
		network("11111111-0000-0000-0000-000000000002", "F2", 2, "192.0.2.1", 24, server()),
		network("11111111-0000-0000-0000-000000000003", "Relay", 3, "198.51.100.1", 24,
			map[string]any{"mode": "RELAY", "dhcpServerIpAddresses": []any{"198.51.100.250"}}),
		network("11111111-0000-0000-0000-000000000004", "NoDHCP", 4, "203.0.113.1", 24, nil),
	}}
}

func (c *controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.Header.Get("X-API-KEY") != "secret" {
		http.Error(w, `{"statusCode":401}`, http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/proxy/network/integration")
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

	switch {
	case r.Method == http.MethodGet && path == "/v1/sites":
		paged([]any{
			map[string]any{"id": "99999999-0000-0000-0000-000000000000", "internalReference": "other", "name": "Other"},
			map[string]any{"id": siteID, "internalReference": "default", "name": "Default"},
		})
	case r.Method == http.MethodGet && path == "/v1/sites/"+siteID+"/networks":
		var items []any
		for _, n := range c.networks {
			items = append(items, map[string]any{
				"id": n["id"], "name": n["name"], "management": n["management"], "vlanId": n["vlanId"],
				"enabled": n["enabled"], "default": n["default"], "metadata": n["metadata"],
			})
		}
		paged(items)
	case strings.HasPrefix(path, "/v1/sites/"+siteID+"/networks/"):
		id := strings.TrimPrefix(path, "/v1/sites/"+siteID+"/networks/")
		i := c.find(id)
		if i < 0 {
			http.Error(w, `{"statusCode":404}`, http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			write(c.networks[i])
		case http.MethodPut:
			var body map[string]any
			require.NoError(c.t, json.NewDecoder(r.Body).Decode(&body))
			c.puts = append(c.puts, body)
			stored := map[string]any{}
			for k, v := range body {
				stored[k] = v
			}
			for _, k := range []string{"id", "default", "metadata"} {
				stored[k] = c.networks[i][k]
			}
			if c.mutate != nil {
				c.mutate(stored)
			}
			c.networks[i] = stored
			write(stored)
		default:
			http.Error(w, "", http.StatusMethodNotAllowed)
		}
	default:
		http.Error(w, `{"statusCode":404}`, http.StatusNotFound)
	}
}

func (c *controller) find(id string) int {
	for i, n := range c.networks {
		if n["id"] == id {
			return i
		}
	}
	return -1
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

func TestFindNetworks(t *testing.T) {
	ctx := context.Background()
	_, p := setup(t)

	ns, err := p.FindNetworks(ctx, map[string]string{"vlan": "2"})
	require.NoError(t, err)
	require.Equal(t, []provider.Network{{
		ID:     "11111111-0000-0000-0000-000000000002",
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

func TestUpdateNetworkChangesOnlyDHCP(t *testing.T) {
	ctx := context.Background()
	c, p := setup(t)
	before, _ := json.Marshal(c.networks[0])

	err := p.UpdateNetwork(ctx, provider.NetworkUpdate{
		ID:         "11111111-0000-0000-0000-000000000002",
		DHCPRanges: lan.Ranges{rng("192.0.2.100", "192.0.2.199")},
		LeaseTime:  8 * time.Hour,
	})
	require.NoError(t, err)
	require.Len(t, c.puts, 1)

	put := c.puts[0]
	for _, k := range []string{"id", "default", "metadata"} {
		require.NotContains(t, put, k, "read-only field is not sent")
	}

	// Everything else is sent back unchanged.
	var want map[string]any
	require.NoError(t, json.Unmarshal(before, &want))
	for _, k := range []string{"id", "default", "metadata"} {
		delete(want, k)
	}
	dc := want["ipv4Configuration"].(map[string]any)["dhcpConfiguration"].(map[string]any)
	dc["ipAddressRange"] = map[string]any{"start": "192.0.2.100", "stop": "192.0.2.199"}
	dc["leaseTimeSeconds"] = float64(28800)
	require.Equal(t, want, put)

	ns, err := p.FindNetworks(ctx, map[string]string{"vlan": "2"})
	require.NoError(t, err)
	require.Equal(t, lan.Ranges{rng("192.0.2.100", "192.0.2.199")}, ns[0].DHCP.Ranges)
	require.Equal(t, 8*time.Hour, ns[0].DHCP.LeaseTime)
}

func TestUpdateNetworkLeaseOnly(t *testing.T) {
	ctx := context.Background()
	c, p := setup(t)
	err := p.UpdateNetwork(ctx, provider.NetworkUpdate{ID: "11111111-0000-0000-0000-000000000002", LeaseTime: time.Hour})
	require.NoError(t, err)
	dc := c.puts[0]["ipv4Configuration"].(map[string]any)["dhcpConfiguration"].(map[string]any)
	require.Equal(t, map[string]any{"start": "192.0.2.6", "stop": "192.0.2.254"}, dc["ipAddressRange"])
	require.Equal(t, float64(3600), dc["leaseTimeSeconds"])
}

func TestUpdateNetworkDetectsSideEffects(t *testing.T) {
	ctx := context.Background()
	c, p := setup(t)
	c.mutate = func(n map[string]any) { n["internetAccessEnabled"] = false }

	err := p.UpdateNetwork(ctx, provider.NetworkUpdate{
		ID:         "11111111-0000-0000-0000-000000000002",
		DHCPRanges: lan.Ranges{rng("192.0.2.100", "192.0.2.199")},
	})
	require.ErrorContains(t, err, "internetAccessEnabled")
}

func TestUpdateNetworkRefusesWithoutServer(t *testing.T) {
	ctx := context.Background()
	c, p := setup(t)
	for _, id := range []string{"11111111-0000-0000-0000-000000000003", "11111111-0000-0000-0000-000000000004"} {
		err := p.UpdateNetwork(ctx, provider.NetworkUpdate{ID: id, DHCPRanges: lan.Ranges{rng("198.51.100.100", "198.51.100.199")}})
		require.Error(t, err)
	}
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
