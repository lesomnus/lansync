package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/lansync/internal/cli"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Main(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCheckExample(t *testing.T) {
	code, out, errOut := run(t, "check", "-f", "../../examples/office.yaml")
	require.Equal(t, cli.ExitOK, code, errOut)
	require.Contains(t, out, "3 networks")
}

func TestCheckInvalid(t *testing.T) {
	f := filepath.Join(t.TempDir(), "bad.yaml")
	require.NoError(t, os.WriteFile(f, []byte("networks:\n  - { name: a, subnet: 192.0.2.1/24 }\n"), 0o600))
	code, _, errOut := run(t, "check", "-f", f)
	require.Equal(t, cli.ExitError, code)
	require.Contains(t, errOut, "match is required")
	require.Contains(t, errOut, "host bits set")
}

// A controller with one network whose pool differs from the declaration.
func controller(t *testing.T) *httptest.Server {
	const site = "s1"
	detail := map[string]any{
		"id": "n2", "name": "F2", "management": "GATEWAY", "enabled": true, "vlanId": 2,
		"ipv4Configuration": map[string]any{
			"hostIpAddress": "192.0.2.1", "prefixLength": 24,
			"dhcpConfiguration": map[string]any{
				"mode":             "SERVER",
				"ipAddressRange":   map[string]any{"start": "192.0.2.6", "stop": "192.0.2.254"},
				"leaseTimeSeconds": 86400,
			},
		},
	}
	conf := map[string]any{"external_id": "n2", "name": "F2", "mdns_enabled": true,
		"dhcpd_start": "192.0.2.6", "dhcpd_stop": "192.0.2.254", "dhcpd_leasetime": 86400}
	puts := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/s/default/rest/networkconf":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{conf}})
		case r.URL.Path == "/integration/v1/sites":
			_ = json.NewEncoder(w).Encode(map[string]any{"offset": 0, "limit": 200, "count": 1, "totalCount": 1,
				"data": []any{map[string]any{"id": site, "internalReference": "default", "name": "Default"}}})
		case r.URL.Path == "/integration/v1/sites/"+site+"/networks":
			_ = json.NewEncoder(w).Encode(map[string]any{"offset": 0, "limit": 200, "count": 1, "totalCount": 1,
				"data": []any{map[string]any{"id": "n2", "name": "F2", "management": "GATEWAY", "vlanId": 2}}})
		case r.URL.Path == "/integration/v1/sites/"+site+"/networks/n2" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(detail)
		case r.URL.Path == "/integration/v1/sites/"+site+"/networks/n2" && r.Method == http.MethodPut:
			puts++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			body["id"] = "n2"
			detail = body
			dc := body["ipv4Configuration"].(map[string]any)["dhcpConfiguration"].(map[string]any)
			rg := dc["ipAddressRange"].(map[string]any)
			conf["dhcpd_start"], conf["dhcpd_stop"], conf["dhcpd_leasetime"] = rg["start"], rg["stop"], dc["leaseTimeSeconds"]
			_ = json.NewEncoder(w).Encode(detail)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeFiles(t *testing.T, srv *httptest.Server) (decl, ca string) {
	dir := t.TempDir()
	decl = filepath.Join(dir, "lansync.yaml")
	require.NoError(t, os.WriteFile(decl, []byte(`
networks:
  - name: F2
    match: { vlan: 2 }
    subnet: 192.0.2.0/24
    blocks: [{ name: dhcp, ranges: [.100-.199] }]
    dhcp: { block: dhcp, leaseTime: 8h }
`), 0o600))
	ca = filepath.Join(dir, "ca.pem")
	cert := srv.Certificate()
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600))
	return decl, ca
}

func TestPlanAndApply(t *testing.T) {
	srv := controller(t)
	t.Setenv("LANSYNC_UNIFI_API_KEY", "k")

	decl, ca := writeFiles(t, srv)
	args := []string{"-f", decl, "-provider", "unifi", "-unifi-url", srv.URL + "/integration", "-unifi-ca", ca}

	code, out, errOut := run(t, append([]string{"plan", "-detailed-exitcode"}, args...)...)
	require.Equal(t, cli.ExitDrift, code, errOut)
	require.Contains(t, out, "dry-run")
	require.Contains(t, out, "dhcp.ranges: 192.0.2.6-192.0.2.254 -> 192.0.2.100-192.0.2.199")

	code, out, errOut = run(t, append([]string{"apply", "-network", "F2", "-o", "json"}, args...)...)
	require.Equal(t, cli.ExitOK, code, errOut)
	var line map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &line))
	require.Equal(t, "applied", line["status"])

	code, out, _ = run(t, append([]string{"plan", "-detailed-exitcode"}, args...)...)
	require.Equal(t, cli.ExitOK, code)
	require.Contains(t, out, "in-sync")
}

func TestApplyNeedsTargets(t *testing.T) {
	srv := controller(t)
	t.Setenv("LANSYNC_UNIFI_API_KEY", "k")
	decl, ca := writeFiles(t, srv)
	args := []string{"-f", decl, "-provider", "unifi", "-unifi-url", srv.URL + "/integration", "-unifi-ca", ca}

	code, _, errOut := run(t, append([]string{"apply"}, args...)...)
	require.Equal(t, cli.ExitError, code)
	require.Contains(t, errOut, "-network is required")

	code, out, _ := run(t, append([]string{"apply", "-network", "nope"}, args...)...)
	require.Equal(t, cli.ExitError, code)
	require.Contains(t, out, `network "nope" is not declared`)
}

func TestUntrustedCertificate(t *testing.T) {
	srv := controller(t)
	t.Setenv("LANSYNC_UNIFI_API_KEY", "k")
	decl, _ := writeFiles(t, srv)
	code, out, _ := run(t, "plan", "-f", decl, "-provider", "unifi", "-unifi-url", srv.URL+"/integration")
	require.Equal(t, cli.ExitError, code)
	require.Contains(t, out, "certificate")
}

func TestJSONReportsInvalidDeclaration(t *testing.T) {
	f := filepath.Join(t.TempDir(), "bad.yaml")
	require.NoError(t, os.WriteFile(f, []byte("networks:\n  - { name: a, subnet: 192.0.2.0/24 }\n"), 0o600))
	code, out, _ := run(t, "plan", "-o", "json", "-f", f, "-provider", "unifi")
	require.Equal(t, cli.ExitError, code)
	var line map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &line))
	require.Equal(t, "failed", line["status"])
	require.Contains(t, line["error"], "match is required")
}
