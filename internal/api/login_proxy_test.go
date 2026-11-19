package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func proxyServer(t *testing.T, entries ...string) *Server {
	t.Helper()
	prefixes, err := parseTrustedProxies(entries)
	if err != nil {
		t.Fatalf("parseTrustedProxies: %v", err)
	}
	return &Server{trustedProxies: prefixes}
}

func loginFrom(server *Server, remoteAddr string, forwarded ...string) string {
	req := httptest.NewRequest(http.MethodPost, loginPath, nil)
	req.RemoteAddr = remoteAddr
	for _, value := range forwarded {
		req.Header.Add("X-Forwarded-For", value)
	}
	return server.loginAddress(req)
}

// Without a configured proxy, a forwarding header is attacker-controlled: it
// must not choose the limiter bucket (it could dodge the block or pin an
// innocent address).
func TestLoginAddressIgnoresUntrustedForwardingHeaders(t *testing.T) {
	server := proxyServer(t)
	if got := loginFrom(server, "198.51.100.7:5000", "203.0.113.9"); got != "198.51.100.7" {
		t.Fatalf("login address = %q, want the peer address", got)
	}
}

func TestLoginAddressUsesTheLastUntrustedHop(t *testing.T) {
	server := proxyServer(t, "10.0.0.0/8", "fd00::/8")
	for _, tt := range []struct {
		name       string
		remoteAddr string
		forwarded  []string
		want       string
	}{
		{name: "single hop", remoteAddr: "10.0.0.5:4000", forwarded: []string{"203.0.113.9"}, want: "203.0.113.9"},
		{name: "skips inner proxies", remoteAddr: "10.0.0.5:4000", forwarded: []string{"203.0.113.9, 10.0.0.6"}, want: "203.0.113.9"},
		{name: "multiple headers", remoteAddr: "10.0.0.5:4000", forwarded: []string{"203.0.113.9", "10.0.0.6"}, want: "203.0.113.9"},
		{name: "ipv6 proxy", remoteAddr: "[fd00::5]:4000", forwarded: []string{"2001:db8::7"}, want: "2001:db8::7"},
		{name: "all hops trusted", remoteAddr: "10.0.0.5:4000", forwarded: []string{"10.0.0.6"}, want: "10.0.0.5"},
		{name: "malformed chain", remoteAddr: "10.0.0.5:4000", forwarded: []string{"203.0.113.9, not-an-address"}, want: "10.0.0.5"},
		{name: "no header", remoteAddr: "10.0.0.5:4000", want: "10.0.0.5"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := loginFrom(server, tt.remoteAddr, tt.forwarded...); got != tt.want {
				t.Fatalf("login address = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseTrustedProxiesAcceptsBareAddresses(t *testing.T) {
	prefixes, err := parseTrustedProxies([]string{"192.0.2.10", "10.0.0.0/8", "2001:db8::/32"})
	if err != nil {
		t.Fatalf("parseTrustedProxies: %v", err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("192.0.2.10/32"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	if len(prefixes) != len(want) {
		t.Fatalf("prefixes = %v, want %v", prefixes, want)
	}
	for i := range want {
		if prefixes[i] != want[i] {
			t.Fatalf("prefix %d = %v, want %v", i, prefixes[i], want[i])
		}
	}
}
