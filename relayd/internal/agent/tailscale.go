package agent

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Tailscale integration. Two independent capabilities:
//
//  1. PRIVATE endpoints prefer the machine's MagicDNS name when a tailnet
//     is up. The auth proxy is then reachable from every device on the
//     tailnet, from anywhere, with zero extra processes. Works with both
//     Tailscale's hosted control plane AND self-hosted Headscale (same
//     client, plain tailnet reachability).
//
//  2. expose="funnel" runs a supervised `tailscale funnel <port>` and provides a
//     STABLE public https://machine.tailnet.ts.net URL with no domain
//     required. Tailscale-hosted control plane only (Headscale has no
//     Funnel infrastructure); must be allowed by the tailnet policy.

// macOS App Store build keeps the CLI inside the app bundle.
const macTailscaleApp = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

func tailscaleBin() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	if _, err := exec.LookPath(macTailscaleApp); err == nil {
		return macTailscaleApp
	}
	return ""
}

// tailscaleSelf returns this machine's best tailnet address: the MagicDNS
// name when MagicDNS is enabled (clients can actually resolve it), else
// the Tailscale IPv4. "" when tailscale is absent, stopped, or logged out.
func tailscaleSelf(ctx context.Context) string {
	bin := tailscaleBin()
	if bin == "" {
		return ""
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(tctx, bin, "status", "--json").Output()
	if err != nil {
		return ""
	}
	return parseTailscaleStatus(out)
}

func parseTailscaleStatus(data []byte) string {
	var status struct {
		BackendState   string `json:"BackendState"`
		CurrentTailnet struct {
			MagicDNSEnabled bool `json:"MagicDNSEnabled"`
		} `json:"CurrentTailnet"`
		Self struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Self"`
	}
	if json.Unmarshal(data, &status) != nil {
		return ""
	}
	if status.BackendState != "Running" {
		return ""
	}
	name := strings.TrimSuffix(status.Self.DNSName, ".")
	if name != "" && status.CurrentTailnet.MagicDNSEnabled {
		return name
	}
	// No resolvable name → fall back to the stable v4 tailnet address.
	for _, ip := range status.Self.TailscaleIPs {
		if strings.Count(ip, ".") == 3 {
			return ip
		}
	}
	return ""
}

// funnelURLRe matches the public URL `tailscale funnel` prints
// ("Available on the internet: ... https://machine.tailnet.ts.net/").
var funnelURLRe = regexp.MustCompile(`https://[a-z0-9-]+\.[a-z0-9-]+\.ts\.net`)
