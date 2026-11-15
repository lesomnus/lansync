// Package unifi is the lansync provider for UniFi Network.
//
// It uses the official Integration API (UniFi Network 10.x):
//
//	GET /v1/sites
//	GET /v1/sites/{siteId}/networks
//	GET /v1/sites/{siteId}/networks/{networkId}
//	PUT /v1/sites/{siteId}/networks/{networkId}
//
// The base URL is the part before /v1, e.g.
// https://192.0.2.1/proxy/network/integration on a UniFi OS console.
//
// The PUT body is the whole network: a field that is left out is reset (the
// API documents that a missing dhcpConfiguration means "no DHCP"). So an update
// reads the network, changes only the DHCP fields, writes it back, reads it
// again and fails if anything other than those fields changed.
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
	// Client is the HTTP client. It decides TLS trust; nil uses
	// http.DefaultClient.
	Client *http.Client
}

type Provider struct {
	cfg  Config
	base *url.URL

	mu     sync.Mutex
	siteID string
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
	return &Provider{cfg: cfg, base: u}, nil
}

func (p *Provider) Name() string { return "unifi" }

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		SelectorKeys: []string{"id", "name", "vlan"},
		DHCPRanges:   1,
		LeaseTime:    true,
	}
}

// readOnly are top-level fields of a network that GET returns and PUT does
// not take.
var readOnly = []string{"id", "default", "metadata"}

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

	siteID, err := p.site(ctx)
	if err != nil {
		return err
	}
	path := p.networkPath(siteID, u.ID)

	before, err := p.getRaw(ctx, path)
	if err != nil {
		return fmt.Errorf("unifi: read before update: %w", err)
	}

	want, err := deepCopy(before)
	if err != nil {
		return err
	}
	if err := setDHCP(want, u); err != nil {
		return fmt.Errorf("unifi: %w", err)
	}
	body := shallowWithout(want, readOnly)

	if err := p.do(ctx, http.MethodPut, path, body, nil); err != nil {
		return fmt.Errorf("unifi: update: %w", err)
	}

	after, err := p.getRaw(ctx, path)
	if err != nil {
		return fmt.Errorf("unifi: read after update (the update was sent): %w", err)
	}
	if diff := diffPaths(shallowWithout(want, readOnly), shallowWithout(after, readOnly), ""); len(diff) > 0 {
		return fmt.Errorf("unifi: after the update the network differs from what was sent at %s; check it on the controller", strings.Join(diff, ", "))
	}
	return nil
}

func setDHCP(m map[string]any, u provider.NetworkUpdate) error {
	v4, ok := m["ipv4Configuration"].(map[string]any)
	if !ok {
		return errors.New("network has no ipv4Configuration")
	}
	dc, ok := v4["dhcpConfiguration"].(map[string]any)
	if !ok {
		return errors.New("network has no DHCP configuration; lansync does not enable DHCP")
	}
	if dc["mode"] != "SERVER" {
		return fmt.Errorf("DHCP mode is %v, not SERVER", dc["mode"])
	}
	if len(u.DHCPRanges) == 1 {
		dc["ipAddressRange"] = map[string]any{
			"start": u.DHCPRanges[0].First.String(),
			"stop":  u.DHCPRanges[0].Last.String(),
		}
	}
	if u.LeaseTime != 0 {
		dc["leaseTimeSeconds"] = json.Number(strconv.FormatInt(int64(u.LeaseTime/time.Second), 10))
	}
	return nil
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
	return p.siteID, nil
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

func (p *Provider) getRaw(ctx context.Context, path string) (map[string]any, error) {
	var m map[string]any
	if err := p.do(ctx, http.MethodGet, path, nil, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (p *Provider) do(ctx context.Context, method, path string, body any, out any) error {
	ref, err := url.Parse(path)
	if err != nil {
		return err
	}
	u := *p.base
	u.Path = p.base.Path + ref.Path
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

func deepCopy(m map[string]any) (map[string]any, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
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
