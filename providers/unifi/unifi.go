// Package unifi is the lansync provider for UniFi Network.
//
// Networks are found and read through the official Integration API
// (UniFi Network 10.x):
//
//	GET /v1/sites
//	GET /v1/sites/{siteId}/networks
//	GET /v1/sites/{siteId}/networks/{networkId}
//
// They are NOT written through it. Its PUT replaces the whole network, and the
// network has settings the Integration API does not show; on a UniFi Network
// 10.6 console a PUT that changed only the DHCP range also turned the network's
// mDNS flag off and dropped a dozen other fields from the record.
//
// Writes go to the controller's own API instead, the one its web UI uses, and
// carry only the fields being changed:
//
//	GET /api/s/{site}/rest/networkconf            (matched by external_id)
//	PUT /api/s/{site}/rest/networkconf/{_id}      {"dhcpd_start", "dhcpd_stop", "dhcpd_leasetime"}
//
// That API is undocumented, so every write is checked: the full record is read
// before and after, and the update fails, naming each setting with its old and
// new value, if anything but those fields changed or they did not take the
// values sent. The Integration view is then read again and must show the same.
//
// The base URL is the Integration root, the part before /v1, e.g.
// https://192.0.2.1/proxy/network/integration on a UniFi OS console. The
// controller's own API root is that without /integration unless set.
package unifi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/lansync/lan"
	"github.com/lesomnus/lansync/provider"
)

// Config configures the provider.
type Config struct {
	// BaseURL is the Integration API root, without the trailing /v1.
	BaseURL string
	// Site is the site's internal reference ("default"), name or ID.
	Site string
	// APIKey is sent as X-API-KEY.
	APIKey string
	// LegacyBaseURL is the root of the controller's own API, the part
	// before /api/s/{site}. Empty means BaseURL without its trailing
	// /integration, e.g. https://192.0.2.1/proxy/network.
	LegacyBaseURL string
	// Client is the HTTP client. It decides TLS trust; nil uses
	// http.DefaultClient.
	Client *http.Client
}

type Provider struct {
	cfg    Config
	base   *url.URL
	legacy *url.URL // nil when it cannot be derived; updates then refuse

	mu      sync.Mutex
	siteID  string
	siteRef string
}

var _ provider.Provider = (*Provider)(nil)

func New(cfg Config) (*Provider, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("unifi: base URL is required")
	}
	u, err := url.Parse(strings.TrimSuffix(cfg.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("unifi: base URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("unifi: base URL must be http(s), got %q", cfg.BaseURL)
	}
	if cfg.APIKey == "" {
		return nil, errors.New("unifi: API key is required")
	}
	if cfg.Site == "" {
		cfg.Site = "default"
	}
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	p := &Provider{cfg: cfg, base: u}
	switch {
	case cfg.LegacyBaseURL != "":
		l, err := url.Parse(strings.TrimSuffix(cfg.LegacyBaseURL, "/"))
		if err != nil {
			return nil, fmt.Errorf("unifi: legacy base URL: %w", err)
		}
		p.legacy = l
	case strings.HasSuffix(u.Path, "/integration"):
		l := *u
		l.Path = strings.TrimSuffix(u.Path, "/integration")
		p.legacy = &l
	}
	return p, nil
}

func (p *Provider) Name() string { return "unifi" }

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		SelectorKeys: []string{"id", "name", "vlan"},
		DHCPRanges:   1,
		LeaseTime:    true,
	}
}

type page[T any] struct {
	Offset     int64 `json:"offset"`
	Limit      int   `json:"limit"`
	Count      int   `json:"count"`
	TotalCount int64 `json:"totalCount"`
	Data       []T   `json:"data"`
}

type site struct {
	ID                string `json:"id"`
	InternalReference string `json:"internalReference"`
	Name              string `json:"name"`
}

type networkOverview struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Management string `json:"management"`
	VlanID     int    `json:"vlanId"`
}

type networkDetail struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Management string `json:"management"`
	VlanID     int    `json:"vlanId"`
	IPv4       *struct {
		HostIPAddress string `json:"hostIpAddress"`
		PrefixLength  int    `json:"prefixLength"`
		DHCP          *struct {
			Mode           string `json:"mode"`
			IPAddressRange *struct {
				Start string `json:"start"`
				Stop  string `json:"stop"`
			} `json:"ipAddressRange"`
			LeaseTimeSeconds *int64 `json:"leaseTimeSeconds"`
		} `json:"dhcpConfiguration"`
	} `json:"ipv4Configuration"`
}

func (p *Provider) FindNetworks(ctx context.Context, sel map[string]string) ([]provider.Network, error) {
	for k := range sel {
		if !slices.Contains(p.Capabilities().SelectorKeys, k) {
			return nil, fmt.Errorf("unifi: unsupported match key %q", k)
		}
	}
	var vlan *int
	if v, ok := sel["vlan"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("unifi: match vlan: %q is not a number", v)
		}
		vlan = &n
	}

	siteID, err := p.site(ctx)
	if err != nil {
		return nil, err
	}
	all, err := list[networkOverview](ctx, p, "/v1/sites/"+url.PathEscape(siteID)+"/networks")
	if err != nil {
		return nil, fmt.Errorf("unifi: list networks: %w", err)
	}

	var out []provider.Network
	for _, o := range all {
		if v, ok := sel["id"]; ok && o.ID != v {
			continue
		}
		if v, ok := sel["name"]; ok && o.Name != v {
			continue
		}
		if vlan != nil && o.VlanID != *vlan {
			continue
		}
		var d networkDetail
		if err := p.getJSON(ctx, p.networkPath(siteID, o.ID), &d); err != nil {
			return nil, fmt.Errorf("unifi: get network %s: %w", o.Name, err)
		}
		n, err := observe(d)
		if err != nil {
			return nil, fmt.Errorf("unifi: network %s: %w", o.Name, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func observe(d networkDetail) (provider.Network, error) {
	n := provider.Network{ID: d.ID, Name: d.Name}
	if d.IPv4 == nil {
		return n, nil
	}
	if d.IPv4.HostIPAddress != "" {
		a, err := netip.ParseAddr(d.IPv4.HostIPAddress)
		if err != nil {
			return n, fmt.Errorf("hostIpAddress: %w", err)
		}
		pfx, err := a.Prefix(d.IPv4.PrefixLength)
		if err != nil {
			return n, fmt.Errorf("prefixLength: %w", err)
		}
		n.Subnet = pfx
	}
	dc := d.IPv4.DHCP
	if dc == nil || dc.Mode != "SERVER" || dc.IPAddressRange == nil {
		return n, nil
	}
	first, err := netip.ParseAddr(dc.IPAddressRange.Start)
	if err != nil {
		return n, fmt.Errorf("dhcp start: %w", err)
	}
	last, err := netip.ParseAddr(dc.IPAddressRange.Stop)
	if err != nil {
		return n, fmt.Errorf("dhcp stop: %w", err)
	}
	dhcp := &provider.DHCP{Ranges: lan.Ranges{{First: first, Last: last}}}
	if dc.LeaseTimeSeconds != nil {
		dhcp.LeaseTime = time.Duration(*dc.LeaseTimeSeconds) * time.Second
	}
	n.DHCP = dhcp
	return n, nil
}

func (p *Provider) UpdateNetwork(ctx context.Context, u provider.NetworkUpdate) error {
	if u.DHCPRanges == nil && u.LeaseTime == 0 {
		return nil
	}
	if len(u.DHCPRanges) > 1 {
		return fmt.Errorf("unifi: a DHCP pool has one range, got %d", len(u.DHCPRanges))
	}
	if len(u.DHCPRanges) == 0 && u.DHCPRanges != nil {
		return errors.New("unifi: an empty DHCP pool is not supported")
	}
	if p.legacy == nil {
		return errors.New("unifi: cannot tell the controller's own API root from the base URL; set the legacy base URL")
	}

	before, err := p.networkConf(ctx, u.ID)
	if err != nil {
		return fmt.Errorf("unifi: read the network record (nothing was changed): %w", err)
	}
	if before["dhcp_relay_enabled"] == true {
		return errors.New("unifi: the network relays DHCP; lansync does not change that")
	}
	if before["dhcpd_enabled"] != true {
		return errors.New("unifi: the network's DHCP server is off; lansync does not turn it on")
	}
	confID, ok := before["_id"].(string)
	if !ok || confID == "" {
		return errors.New("unifi: the network record has no _id")
	}

	body := map[string]any{}
	if len(u.DHCPRanges) == 1 {
		body["dhcpd_start"] = u.DHCPRanges[0].First.String()
		body["dhcpd_stop"] = u.DHCPRanges[0].Last.String()
	}
	if u.LeaseTime != 0 {
		body["dhcpd_leasetime"] = int64(u.LeaseTime / time.Second)
	}

	ref, err := p.siteReference(ctx)
	if err != nil {
		return err
	}
	var res legacyResult
	err = p.doURL(ctx, http.MethodPut, p.legacy, "/api/s/"+url.PathEscape(ref)+"/rest/networkconf/"+url.PathEscape(confID), body, &res)
	if err == nil {
		err = res.err()
	}
	if err != nil {
		return fmt.Errorf("unifi: update (check the network on the controller): %w", err)
	}

	after, err := p.networkConf(ctx, u.ID)
	if err != nil {
		return fmt.Errorf("unifi: read the network record after the update (the update was sent): %w", err)
	}
	if err := checkConf(before, after, u); err != nil {
		return fmt.Errorf("unifi: after the update the network record is not what was asked; check these on the controller: %w", err)
	}

	// The Integration view must agree, or later plans would see drift that
	// is not there (or miss drift that is).
	siteID, err := p.site(ctx)
	if err != nil {
		return err
	}
	var d networkDetail
	if err := p.getJSON(ctx, p.networkPath(siteID, u.ID), &d); err != nil {
		return fmt.Errorf("unifi: read the network after the update (the update was applied): %w", err)
	}
	n, err := observe(d)
	if err != nil {
		return fmt.Errorf("unifi: read the network after the update (the update was applied): %w", err)
	}
	switch {
	case n.DHCP == nil:
		return errors.New("unifi: after the update the Integration API shows no DHCP server")
	case len(u.DHCPRanges) == 1 && !n.DHCP.Ranges.Equal(u.DHCPRanges):
		return fmt.Errorf("unifi: after the update the Integration API shows the pool %s, want %s", n.DHCP.Ranges, u.DHCPRanges)
	case u.LeaseTime != 0 && n.DHCP.LeaseTime != u.LeaseTime:
		return fmt.Errorf("unifi: after the update the Integration API shows lease time %s, want %s", n.DHCP.LeaseTime, u.LeaseTime)
	}
	return nil
}

// legacyResult is the envelope of the controller's own API.
type legacyResult struct {
	Meta struct {
		RC  string `json:"rc"`
		Msg string `json:"msg"`
	} `json:"meta"`
}

func (r legacyResult) err() error {
	if r.Meta.RC != "ok" {
		return fmt.Errorf("controller answered rc=%q msg=%q", r.Meta.RC, r.Meta.Msg)
	}
	return nil
}

// managedConf are the fields of a networkconf record that an update may change.
var managedConf = []string{"dhcpd_start", "dhcpd_stop", "dhcpd_leasetime"}

func checkConf(before, after map[string]any, u provider.NetworkUpdate) error {
	var problems []string
	for _, k := range diffPaths(shallowWithout(before, managedConf), shallowWithout(after, managedConf), "") {
		problems = append(problems, fmt.Sprintf("%s: %s -> %s", k, show(lookup(before, k)), show(lookup(after, k))))
	}
	expect := func(k, want string) {
		if got := fmt.Sprint(after[k]); got != want {
			problems = append(problems, fmt.Sprintf("%s is %s, want %s", k, show(after[k]), want))
		}
	}
	if len(u.DHCPRanges) == 1 {
		expect("dhcpd_start", u.DHCPRanges[0].First.String())
		expect("dhcpd_stop", u.DHCPRanges[0].Last.String())
	}
	if u.LeaseTime != 0 {
		expect("dhcpd_leasetime", strconv.FormatInt(int64(u.LeaseTime/time.Second), 10))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func lookup(m map[string]any, path string) any {
	var cur any = m
	for _, k := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func show(v any) string {
	if v == nil {
		return "(none)"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// networkConf reads the controller's full record of the network whose
// Integration API id is id.
func (p *Provider) networkConf(ctx context.Context, id string) (map[string]any, error) {
	ref, err := p.siteReference(ctx)
	if err != nil {
		return nil, err
	}
	var res struct {
		Data []map[string]any `json:"data"`
	}
	if err := p.doURL(ctx, http.MethodGet, p.legacy, "/api/s/"+url.PathEscape(ref)+"/rest/networkconf", nil, &res); err != nil {
		return nil, err
	}
	var match []map[string]any
	for _, n := range res.Data {
		if n["external_id"] == id {
			match = append(match, n)
		}
	}
	if len(match) != 1 {
		return nil, fmt.Errorf("%d networkconf records have external_id %s", len(match), id)
	}
	return match[0], nil
}

func (p *Provider) site(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.siteID != "" {
		return p.siteID, nil
	}
	sites, err := list[site](ctx, p, "/v1/sites")
	if err != nil {
		return "", fmt.Errorf("unifi: list sites: %w", err)
	}
	var match []site
	for _, s := range sites {
		if s.ID == p.cfg.Site || s.InternalReference == p.cfg.Site || s.Name == p.cfg.Site {
			match = append(match, s)
		}
	}
	if len(match) != 1 {
		return "", fmt.Errorf("unifi: site %q matches %d sites", p.cfg.Site, len(match))
	}
	p.siteID = match[0].ID
	p.siteRef = match[0].InternalReference
	return p.siteID, nil
}

func (p *Provider) siteReference(ctx context.Context) (string, error) {
	if _, err := p.site(ctx); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.siteRef == "" {
		return "", errors.New("unifi: the site has no internal reference")
	}
	return p.siteRef, nil
}

func (p *Provider) networkPath(siteID, id string) string {
	return "/v1/sites/" + url.PathEscape(siteID) + "/networks/" + url.PathEscape(id)
}

const pageLimit = 200

func list[T any](ctx context.Context, p *Provider, path string) ([]T, error) {
	var out []T
	for offset := 0; ; {
		var pg page[T]
		q := fmt.Sprintf("%s?offset=%d&limit=%d", path, offset, pageLimit)
		if err := p.getJSON(ctx, q, &pg); err != nil {
			return nil, err
		}
		out = append(out, pg.Data...)
		offset += len(pg.Data)
		if int64(offset) >= pg.TotalCount || len(pg.Data) == 0 {
			if int64(offset) < pg.TotalCount {
				return nil, fmt.Errorf("incomplete listing: %d of %d", offset, pg.TotalCount)
			}
			return out, nil
		}
	}
}

func (p *Provider) getJSON(ctx context.Context, path string, v any) error {
	return p.do(ctx, http.MethodGet, path, nil, v)
}

func (p *Provider) do(ctx context.Context, method, path string, body any, out any) error {
	return p.doURL(ctx, method, p.base, path, body, out)
}

func (p *Provider) doURL(ctx context.Context, method string, base *url.URL, path string, body any, out any) error {
	ref, err := url.Parse(path)
	if err != nil {
		return err
	}
	u := *base
	u.Path = base.Path + ref.Path
	u.RawQuery = ref.RawQuery

	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-KEY", p.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := p.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}
	if res.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return fmt.Errorf("%s %s: %s: %s", method, ref.Path, res.Status, msg)
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, ref.Path, err)
	}
	return nil
}

func shallowWithout(m map[string]any, keys []string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if !slices.Contains(keys, k) {
			out[k] = v
		}
	}
	return out
}

// diffPaths lists the JSON paths where a and b differ.
func diffPaths(a, b any, at string) []string {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		var out []string
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		slices.Sort(sorted)
		for _, k := range sorted {
			out = append(out, diffPaths(am[k], bm[k], at+"."+k)...)
		}
		return out
	}
	if reflect.DeepEqual(a, b) {
		return nil
	}
	if at == "" {
		at = "."
	}
	return []string{strings.TrimPrefix(at, ".")}
}
