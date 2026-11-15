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

	"github.com/lesomnus/lansync/lan"
	"github.com/lesomnus/lansync/plan"
	"github.com/lesomnus/lansync/provider"
	"github.com/lesomnus/lansync/providers/unifi"
	"github.com/lesomnus/lansync/spec"
)

const usage = `lansync keeps network controllers in line with a declared LAN address plan.

Usage:
  lansync check  [-f FILE]                 validate the declaration only
  lansync plan   [-f FILE] PROVIDER-FLAGS  show what differs; change nothing
  lansync apply  [-f FILE] PROVIDER-FLAGS  change networks whose mode is apply

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

	unifiURL        string
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
	fs.DurationVar(&f.timeout, "timeout", 2*time.Minute, "overall time limit")
	fs.StringVar(&f.unifiURL, "unifi-url", "", "UniFi Integration API root, e.g. https://192.0.2.1/proxy/network/integration")
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

	c, err := spec.LoadFile(f.file)
	if err != nil {
		printLoadError(stderr, f.file, err)
		return ExitError
	}

	p, err := newProvider(f)
	if err != nil {
		fmt.Fprintf(stderr, "lansync: %v\n", err)
		return ExitError
	}

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	pl := plan.Make(ctx, c, p)
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
		BaseURL: f.unifiURL,
		Site:    f.unifiSite,
		APIKey:  key,
		Client:  client,
	})
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

func writeJSON(w io.Writer, pl *plan.Plan) {
	enc := json.NewEncoder(w)
	for _, e := range pl.Errors {
		_ = enc.Encode(map[string]string{"provider": pl.Provider, "status": string(plan.Failed), "error": e})
	}
	for _, n := range pl.Networks {
		_ = enc.Encode(jsonLine{Provider: pl.Provider, NetworkResult: n})
	}
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
		mode := string(n.Mode)
		if !apply && n.Mode == lan.Apply {
			mode += " (plan)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n.Network, n.Selector, mode, n.Status, detail[0])
		for _, d := range detail[1:] {
			fmt.Fprintf(tw, "\t\t\t\t%s\n", d)
		}
	}
	tw.Flush()
}
