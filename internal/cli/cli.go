// Package cli is the lansync command line.
package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lesomnus/lansync/plan"
	"github.com/lesomnus/lansync/provider"
	"github.com/lesomnus/lansync/providers/unifi"
	"github.com/lesomnus/lansync/spec"
)

const usage = `lansync keeps network controllers in line with a declared LAN address plan.

Usage:
  lansync check  [-f FILE]                 validate the declaration only
  lansync plan   [-f FILE] PROVIDER-FLAGS  show what differs; change nothing
  lansync apply  [-f FILE] -network NAMES PROVIDER-FLAGS
                                           change the named networks ("all" for every
                                           declared network); report the rest as plan does

Run "lansync COMMAND -h" for the flags of a command.
`

// Exit codes.
const (
	ExitOK    = 0
	ExitError = 1
	// ExitDrift is returned by plan and apply with -detailed-exitcode when a
	// network differs and was left as is.
	ExitDrift = 2
)

// Main runs the command line and returns the exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return ExitError
	}
	switch args[0] {
	case "check":
		return runCheck(args[1:], stdout, stderr)
	case "plan":
		return runSync(ctx, false, args[1:], stdout, stderr)
	case "apply":
		return runSync(ctx, true, args[1:], stdout, stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "lansync: unknown command %q\n\n%s", args[0], usage)
		return ExitError
	}
}

func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("f", "lansync.yaml", "declaration file")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	c, err := spec.LoadFile(*file)
	if err != nil {
		printLoadError(stderr, *file, err)
		return ExitError
	}
	fmt.Fprintf(stdout, "%s: %d networks, %d reservations\n", *file, len(c.Networks), len(c.Reservations))
	return ExitOK
}

type syncFlags struct {
	file         string
	providerName string
	output       string
	detailed     bool
	networks     string

	unifiURL        string
	unifiLegacyURL  string
	unifiSite       string
	unifiKeyFile    string
	unifiCA         string
	unifiServerName string
	timeout         time.Duration
}

func runSync(ctx context.Context, apply bool, args []string, stdout, stderr io.Writer) int {
	name := "plan"
	if apply {
		name = "apply"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f syncFlags
	fs.StringVar(&f.file, "f", "lansync.yaml", "declaration file")
	fs.StringVar(&f.providerName, "provider", "", `provider (required): "unifi"`)
	fs.StringVar(&f.output, "o", "text", `output: "text" or "json" (one object per network)`)
	fs.BoolVar(&f.detailed, "detailed-exitcode", false, "exit 2 when a network differs and was left as is")
	if apply {
		fs.StringVar(&f.networks, "network", "", `comma-separated names of the declared networks to change, or "all" (required)`)
	}
	fs.DurationVar(&f.timeout, "timeout", 2*time.Minute, "overall time limit")
	fs.StringVar(&f.unifiURL, "unifi-url", "", "UniFi Integration API root, e.g. https://192.0.2.1/proxy/network/integration")
	fs.StringVar(&f.unifiLegacyURL, "unifi-legacy-url", "", "root of the controller's own API, read to verify updates (default: -unifi-url without /integration)")
	fs.StringVar(&f.unifiSite, "unifi-site", "default", "UniFi site: internal reference, name or ID")
	fs.StringVar(&f.unifiKeyFile, "unifi-api-key-file", "", "file holding the UniFi API key (default: $LANSYNC_UNIFI_API_KEY)")
	fs.StringVar(&f.unifiCA, "unifi-ca", "", "PEM file of the certificate(s) to trust for the controller (default: system roots)")
	fs.StringVar(&f.unifiServerName, "unifi-tls-server-name", "", "name to verify the controller certificate against, when the URL host is an IP")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if f.output != "text" && f.output != "json" {
		fmt.Fprintf(stderr, "lansync: -o must be text or json\n")
		return ExitError
	}
	var targets plan.Targets
	if apply {
		var err error
		targets, err = parseTargets(f.networks)
		if err != nil {
			fmt.Fprintf(stderr, "lansync: %v\n", err)
			if f.output == "json" {
				writeFailure(stdout, f.providerName, err.Error())
			}
			return ExitError
		}
	}

	c, err := spec.LoadFile(f.file)
	if err != nil {
		printLoadError(stderr, f.file, err)
		if f.output == "json" {
			writeFailure(stdout, f.providerName, fmt.Sprintf("%s is not valid: %v", f.file, err))
		}
		return ExitError
	}

	p, err := newProvider(f)
	if err != nil {
		fmt.Fprintf(stderr, "lansync: %v\n", err)
		if f.output == "json" {
			writeFailure(stdout, f.providerName, err.Error())
		}
		return ExitError
	}

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	pl := plan.Make(ctx, c, p, targets)
	if apply {
		pl.Apply(ctx, p)
	}

	if f.output == "json" {
		writeJSON(stdout, pl)
	} else {
		writeText(stdout, pl, apply)
	}

	switch {
	case pl.Failed():
		return ExitError
	case f.detailed && pl.HasDrift():
		return ExitDrift
	default:
		return ExitOK
	}
}

func newProvider(f syncFlags) (provider.Provider, error) {
	switch f.providerName {
	case "":
		return nil, errors.New(`-provider is required ("unifi")`)
	case "unifi":
	default:
		return nil, fmt.Errorf("unknown provider %q", f.providerName)
	}

	key := os.Getenv("LANSYNC_UNIFI_API_KEY")
	if f.unifiKeyFile != "" {
		b, err := os.ReadFile(f.unifiKeyFile)
		if err != nil {
			return nil, fmt.Errorf("unifi API key: %w", err)
		}
		key = strings.TrimSpace(string(b))
	}
	if key == "" {
		return nil, errors.New("unifi API key: set -unifi-api-key-file or $LANSYNC_UNIFI_API_KEY")
	}

	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: f.unifiServerName}
	if f.unifiCA != "" {
		pem, err := os.ReadFile(f.unifiCA)
		if err != nil {
			return nil, fmt.Errorf("unifi CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("unifi CA: no certificate in %s", f.unifiCA)
		}
		tlsConf.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsConf
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	return unifi.New(unifi.Config{
		BaseURL:       f.unifiURL,
		LegacyBaseURL: f.unifiLegacyURL,
		Site:          f.unifiSite,
		APIKey:        key,
		Client:        client,
	})
}

func parseTargets(s string) (plan.Targets, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "":
		return plan.Targets{}, errors.New(`apply: -network is required: name the networks to change, or "all"`)
	case "all":
		return plan.Targets{All: true}, nil
	}
	var t plan.Targets
	for _, n := range strings.Split(s, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			return plan.Targets{}, fmt.Errorf("apply: -network %q has an empty name", s)
		}
		t.Names = append(t.Names, n)
	}
	return t, nil
}

func printLoadError(w io.Writer, file string, err error) {
	fmt.Fprintf(w, "lansync: %s is not valid:\n", file)
	for _, line := range strings.Split(err.Error(), "\n") {
		fmt.Fprintf(w, "  - %s\n", line)
	}
}

type jsonLine struct {
	Provider string `json:"provider"`
	plan.NetworkResult
}

// writeFailure reports, in the JSON output, a failure that happened before
// any network was looked at, so that log-based monitoring sees it as one.
func writeFailure(w io.Writer, providerName, msg string) {
	_ = json.NewEncoder(w).Encode(map[string]string{"provider": providerName, "status": string(plan.Failed), "error": msg})
}

func writeJSON(w io.Writer, pl *plan.Plan) {
	for _, e := range pl.Errors {
		writeFailure(w, pl.Provider, e)
	}
	enc := json.NewEncoder(w)
	for _, n := range pl.Networks {
		_ = enc.Encode(jsonLine{Provider: pl.Provider, NetworkResult: n})
	}
	for _, r := range pl.Reservations {
		_ = enc.Encode(reservationLine{Provider: pl.Provider, ReservationResult: r})
	}
}

type reservationLine struct {
	Provider string `json:"provider"`
	plan.ReservationResult
}

func writeText(w io.Writer, pl *plan.Plan, apply bool) {
	for _, e := range pl.Errors {
		fmt.Fprintf(w, "error: %s\n", e)
	}
	if len(pl.Errors) > 0 && apply {
		fmt.Fprintln(w, "nothing was applied because of the errors above")
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NETWORK\tMATCH\tMODE\tSTATUS\tDETAIL")
	for _, n := range pl.Networks {
		var detail []string
		for _, c := range n.Changes {
			detail = append(detail, fmt.Sprintf("%s: %s -> %s", c.Field, c.From, c.To))
		}
		if n.Error != "" {
			detail = append(detail, n.Error)
		}
		if len(detail) == 0 {
			detail = []string{""}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n.Network, n.Selector, n.Mode, n.Status, detail[0])
		for _, d := range detail[1:] {
			fmt.Fprintf(tw, "\t\t\t\t%s\n", d)
		}
	}
	tw.Flush()

	if len(pl.Reservations) == 0 {
		return
	}
	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RESERVATION\tNETWORK\tIP\tMODE\tSTATUS\tDETAIL")
	for _, r := range pl.Reservations {
		var detail []string
		for _, c := range r.Changes {
			detail = append(detail, fmt.Sprintf("%s: %s -> %s", c.Field, c.From, c.To))
		}
		if r.Error != "" {
			detail = append(detail, r.Error)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Reservation, r.Network, r.IP, r.Mode, r.Status, strings.Join(detail, "; "))
	}
	tw.Flush()
}
