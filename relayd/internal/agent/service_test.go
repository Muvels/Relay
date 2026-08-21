package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The auth proxy is the security boundary for public services: correct key
// passes through, while everything else receives 401. The tunnel URL alone gives nothing.
func TestAuthProxyGatesUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "hello from service")
		}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	port := u.Port()

	ap, err := newAuthProxy(true, "sk_test_key", false)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	ap.SetTarget(port)
	base := "http://" + ap.Addr()

	get := func(headers map[string]string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if code, _ := get(nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous request: want 401, got %d", code)
	}
	if code, _ := get(map[string]string{"Authorization": "Bearer wrong"}); code != http.StatusUnauthorized {
		t.Fatalf("wrong key: want 401, got %d", code)
	}
	if code, body := get(map[string]string{"Authorization": "Bearer sk_test_key"}); code != http.StatusOK || !strings.Contains(body, "hello from service") {
		t.Fatalf("valid bearer: got %d %q", code, body)
	}
	if code, _ := get(map[string]string{"X-Relay-Key": "sk_test_key"}); code != http.StatusOK {
		t.Fatalf("valid X-Relay-Key: got %d", code)
	}
}

// The Relay credential must never reach the upstream service, and an app's
// own Authorization survives when the key came via X-Relay-Key.
func TestAuthProxyStripsRelayCredential(t *testing.T) {
	var gotAuth, gotKey string
	upstream := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotKey = r.Header.Get("X-Relay-Key")
			io.WriteString(w, "ok")
		}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)

	ap, err := newAuthProxy(true, "sk_k", false)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	ap.SetTarget(u.Port())

	req, _ := http.NewRequest(http.MethodGet, "http://"+ap.Addr()+"/", nil)
	req.Header.Set("X-Relay-Key", "sk_k")
	req.Header.Set("Authorization", "Bearer app-level-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotKey != "" {
		t.Fatal("X-Relay-Key leaked upstream")
	}
	if gotAuth != "Bearer app-level-token" {
		t.Fatalf("app Authorization should survive, got %q", gotAuth)
	}

	req2, _ := http.NewRequest(http.MethodGet, "http://"+ap.Addr()+"/", nil)
	req2.Header.Set("Authorization", "Bearer sk_k")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if gotAuth != "" {
		t.Fatal("relay key sent via Authorization must be stripped upstream")
	}
}

func TestAuthProxyOpenModeOnlyByExplicitOptOut(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "open") }))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)

	ap, err := newAuthProxy(true, "", true) // auth="none"
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	ap.SetTarget(u.Port())
	resp, err := http.Get("http://" + ap.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth=none should pass anonymous: %d", resp.StatusCode)
	}
}

func TestTunnelURLRegex(t *testing.T) {
	line := "2026-08-16T12:00:00Z INF |  https://random-words-1234.trycloudflare.com  |"
	if got := tunnelURLRe.FindString(line); got != "https://random-words-1234.trycloudflare.com" {
		t.Fatalf("got %q", got)
	}
}
