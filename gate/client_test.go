package gate

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

// TestClientKeyTrustBoundary is the table that settles what the gate
// believes about a forwarded address, and where. The peer column is the
// one that matters: from an untrusted peer no header is read at all.
func TestClientKeyTrustBoundary(t *testing.T) {
	g := mustGate(t, Config{})
	cases := []struct {
		name    string
		peer    string
		headers map[string]string
		want    string
	}{
		{
			name: "untrusted peer ignores every forwarding header",
			peer: "203.0.113.9:4444",
			headers: map[string]string{
				cloudflareClientHeader: "198.51.100.1",
				forwardedForHeader:     "198.51.100.2",
			},
			want: "203.0.113.9",
		},
		{
			name:    "trusted peer prefers CF-Connecting-IP",
			peer:    "172.18.0.4:33000",
			headers: map[string]string{cloudflareClientHeader: "198.51.100.1", forwardedForHeader: "198.51.100.2"},
			want:    "198.51.100.1",
		},
		{
			name:    "trusted peer falls back to the rightmost X-Forwarded-For",
			peer:    "172.18.0.4:33000",
			headers: map[string]string{forwardedForHeader: "198.51.100.66, 203.0.113.7"},
			want:    "203.0.113.7",
		},
		{
			name:    "a client-forged leftmost X-Forwarded-For is not the key",
			peer:    "10.1.2.3:5",
			headers: map[string]string{forwardedForHeader: "1.1.1.1, 2.2.2.2, 203.0.113.8"},
			want:    "203.0.113.8",
		},
		{
			name:    "a garbage rightmost entry falls back to the next one along",
			peer:    "10.1.2.3:5",
			headers: map[string]string{forwardedForHeader: "203.0.113.8, unknown"},
			want:    "203.0.113.8",
		},
		{
			name:    "an entirely unparseable chain falls back to the peer",
			peer:    "10.1.2.3:5",
			headers: map[string]string{forwardedForHeader: "unknown, _hidden"},
			want:    "10.1.2.3",
		},
		{
			name:    "an unparseable CF header falls through to X-Forwarded-For",
			peer:    "10.1.2.3:5",
			headers: map[string]string{cloudflareClientHeader: "not-an-ip", forwardedForHeader: "203.0.113.8"},
			want:    "203.0.113.8",
		},
		{
			name:    "no headers at all keys on the trusted peer itself",
			peer:    "127.0.0.1:8080",
			headers: nil,
			want:    "127.0.0.1",
		},
		{
			name:    "an X-Forwarded-For entry with a port is still an address",
			peer:    "10.1.2.3:5",
			headers: map[string]string{forwardedForHeader: "203.0.113.8:19000"},
			want:    "203.0.113.8",
		},
		{
			name:    "a v4-in-v6 client is the same bucket as its v4 form",
			peer:    "10.1.2.3:5",
			headers: map[string]string{cloudflareClientHeader: "::ffff:203.0.113.8"},
			want:    "203.0.113.8",
		},
		{
			name:    "an IPv6 client is keyed on its /64",
			peer:    "10.1.2.3:5",
			headers: map[string]string{cloudflareClientHeader: "2001:db8:1:2:3:4:5:6"},
			want:    "2001:db8:1:2::/64",
		},
		{
			name:    "two addresses in one /64 are one client",
			peer:    "10.1.2.3:5",
			headers: map[string]string{cloudflareClientHeader: "2001:db8:1:2:ffff:ffff:ffff:ffff"},
			want:    "2001:db8:1:2::/64",
		},
		{
			name:    "an fc00::/7 peer is a trusted proxy",
			peer:    "[fd00::1]:9000",
			headers: map[string]string{cloudflareClientHeader: "198.51.100.5"},
			want:    "198.51.100.5",
		},
		{
			name:    "a public IPv6 peer is not a trusted proxy",
			peer:    "[2001:db8:9:9::1]:9000",
			headers: map[string]string{cloudflareClientHeader: "198.51.100.5"},
			want:    "2001:db8:9:9::/64",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.ClientKey(request(tc.peer, tc.headers)); got != tc.want {
				t.Fatalf("ClientKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientKeyUnparseableRemoteAddr covers the branch that cannot
// happen over TCP but can over a unix socket or a synthetic request: an
// address we cannot parse shares one bucket, which refuses more rather
// than less.
func TestClientKeyUnparseableRemoteAddr(t *testing.T) {
	g := mustGate(t, Config{})
	r := httptest.NewRequest(http.MethodPost, "/interviews", nil)
	r.RemoteAddr = "@"
	if got := g.ClientKey(r); got != "peer:@" {
		t.Fatalf("ClientKey = %q, want %q", got, "peer:@")
	}
	r.RemoteAddr = ""
	if got := g.ClientKey(r); got != "peer:" {
		t.Fatalf("ClientKey = %q, want %q", got, "peer:")
	}
}

// TestClientKeyCustomTrustedProxies pins that the trust set is
// configuration and not a hard-coded assumption about the deployment.
func TestClientKeyCustomTrustedProxies(t *testing.T) {
	g := mustGate(t, Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}})
	headers := map[string]string{cloudflareClientHeader: "198.51.100.1"}
	if got := g.ClientKey(request("203.0.113.9:1", headers)); got != "198.51.100.1" {
		t.Fatalf("ClientKey from the configured proxy = %q, want the forwarded address", got)
	}
	// The default private ranges are NOT trusted once the caller
	// supplies its own set.
	if got := g.ClientKey(request("10.0.0.2:1", headers)); got != "10.0.0.2" {
		t.Fatalf("ClientKey from an unlisted peer = %q, want the peer address", got)
	}
}

// TestSpoofedClientKeyCannotOutrunTheGlobalCap is the doc's central
// claim, stated as a test: the per-client key is fairness, the Global
// bucket is the budget.
func TestSpoofedClientKeyCannotOutrunTheGlobalCap(t *testing.T) {
	c := newClock()
	next := &counted{}
	g := mustGate(t, Config{Now: c.now})
	rule := Rule{
		Name:      "generate",
		PerClient: Limit{Burst: 3, Every: 20 * time.Minute},
		Global:    Limit{Burst: 6, Every: 10 * time.Minute},
	}
	h := mustProtect(t, g, rule, next)
	for i := range 500 {
		h.ServeHTTP(httptest.NewRecorder(), request("172.18.0.4:1", map[string]string{
			// A fresh forged client every single request.
			cloudflareClientHeader: netip.AddrFrom4([4]byte{198, 51, byte(i / 256), byte(i % 256)}).String(),
		}))
	}
	if next.count() != 6 {
		t.Fatalf("500 forged client keys bought %d generations, want exactly the global burst of 6", next.count())
	}
}
