package agent

import "testing"

func TestParseTailscaleStatus(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"running with magicdns",
			`{"BackendState":"Running","CurrentTailnet":{"MagicDNSEnabled":true},"Self":{"DNSName":"spark.tail1234.ts.net.","TailscaleIPs":["100.64.0.7","fd7a::1"]}}`,
			"spark.tail1234.ts.net"},
		{"named tailnet",
			`{"BackendState":"Running","CurrentTailnet":{"MagicDNSEnabled":true},"Self":{"DNSName":"mac.fox-bear.ts.net."}}`,
			"mac.fox-bear.ts.net"},
		{"magicdns disabled falls back to v4 ip",
			`{"BackendState":"Running","CurrentTailnet":{"MagicDNSEnabled":false},"Self":{"DNSName":"spark.tail1234.ts.net.","TailscaleIPs":["fd7a::1","100.64.0.7"]}}`,
			"100.64.0.7"},
		{"no name but has ip",
			`{"BackendState":"Running","CurrentTailnet":{"MagicDNSEnabled":true},"Self":{"DNSName":"","TailscaleIPs":["100.64.0.9"]}}`,
			"100.64.0.9"},
		{"stopped",
			`{"BackendState":"Stopped","Self":{"DNSName":"spark.tail1234.ts.net."}}`, ""},
		{"needs login", `{"BackendState":"NeedsLogin","Self":{"DNSName":""}}`, ""},
		{"garbage", `not json`, ""},
		{"empty", `{}`, ""},
	}
	for _, c := range cases {
		if got := parseTailscaleStatus([]byte(c.json)); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestFunnelURLRegex(t *testing.T) {
	lines := map[string]string{
		"Available on the internet:":                                      "",
		"|-- https://spark.tail1234.ts.net/ proxy http://127.0.0.1:53211": "https://spark.tail1234.ts.net",
		"https://mac.fox-bear.ts.net (Funnel on)":                         "https://mac.fox-bear.ts.net",
		"https://example.com is not a funnel url":                         "",
	}
	for line, want := range lines {
		if got := funnelURLRe.FindString(line); got != want {
			t.Errorf("%q: got %q want %q", line, got, want)
		}
	}
}
