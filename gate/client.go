package gate

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// cloudflareClientHeader is the header Cloudflare sets to the address it
// accepted the connection from. Cloudflare overwrites any value the
// client supplied, which is what makes it the best of the forwarded
// headers. See the package doc for what it is worth when Cloudflare is
// bypassed.
const cloudflareClientHeader = "CF-Connecting-IP"

// forwardedForHeader is the conventional proxy chain. Each hop appends,
// so its RIGHTMOST entry is what the nearest proxy actually observed and
// its leftmost is whatever the client chose to claim.
const forwardedForHeader = "X-Forwarded-For"

// ipv6ClientBits is the prefix length one IPv6 client is keyed on. A
// residential or hosted IPv6 allocation is routinely a /64 or shorter,
// so keying the full 128-bit address would give one client as many
// buckets as it cared to enumerate.
const ipv6ClientBits = 64

// defaultTrustedProxies is the peer set whose forwarding headers are
// believed when Config.TrustedProxies is nil: loopback, the RFC1918
// ranges, the IPv4 link-local range, IPv6 loopback, unique-local (fc00::/7)
// and link-local (fe80::/10). A reverse proxy on the same private
// network presents one of these addresses, and nothing else can present
// one to a container-local listener.
func defaultTrustedProxies() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("::1/128"),
		netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("fe80::/10"),
	}
}

// clientKey returns the rate-limit key for r. It names the per-client
// bucket one caller draws from.
//
// Forwarding headers are consulted only when the direct peer is a
// trusted proxy. Otherwise the peer address alone is the key and every
// header is ignored. See the package doc for the full trust argument,
// including why the Global limit and not this key is what actually
// bounds spend.
func (g *Gate) clientKey(r *http.Request) string {
	peer, ok := peerAddr(r.RemoteAddr)
	if !ok {
		// An unparseable RemoteAddr is not a client we can distinguish,
		// so every such request shares one bucket. That is the strict
		// direction: it can only refuse more, never less.
		return "peer:" + r.RemoteAddr
	}
	if !g.trustsPeer(peer) {
		return addrKey(peer)
	}
	if forwarded, ok := parseIP(r.Header.Get(cloudflareClientHeader)); ok {
		return addrKey(forwarded)
	}
	if forwarded, ok := rightmostForwardedFor(r.Header.Get(forwardedForHeader)); ok {
		return addrKey(forwarded)
	}
	return addrKey(peer)
}

// trustsPeer reports whether peer's forwarding headers are believed.
func (g *Gate) trustsPeer(peer netip.Addr) bool {
	for _, prefix := range g.trusted {
		if prefix.Contains(peer) {
			return true
		}
	}
	return false
}

// peerAddr parses an http.Request.RemoteAddr, which is "host:port" for
// a TCP listener but a bare path or address for others.
func peerAddr(remote string) (netip.Addr, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return netip.Addr{}, false
	}
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return parseIP(host)
	}
	return parseIP(remote)
}

// parseIP parses one textual address, unmapping a v4-in-v6 form so
// "::ffff:203.0.113.7" and "203.0.113.7" cannot be two buckets. A zone
// is dropped for the same reason.
func parseIP(raw string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap().WithZone(""), true
}

// rightmostForwardedFor returns the last entry of an X-Forwarded-For
// header. Each hop appends its own view of the peer, so the last entry
// is the one the nearest proxy wrote and the only one no client could
// have chosen. Entries earlier in the list are the client's own claim.
func rightmostForwardedFor(header string) (netip.Addr, bool) {
	if header == "" {
		return netip.Addr{}, false
	}
	parts := strings.Split(header, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		// Some proxies write "host:port" into the list. net.SplitHostPort
		// handles that shape and parseIP handles the bare one.
		if addr, ok := peerAddr(parts[i]); ok {
			return addr, true
		}
	}
	return netip.Addr{}, false
}

// addrKey renders one address as its bucket key: the exact address for
// IPv4, the /64 for IPv6.
func addrKey(addr netip.Addr) string {
	if addr.Is6() {
		if prefix, err := addr.Prefix(ipv6ClientBits); err == nil {
			return prefix.String()
		}
	}
	return addr.String()
}
