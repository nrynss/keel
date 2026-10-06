package fetch

import (
	"mime"
	"net"
	"net/url"
	"strconv"
	"strings"
	"syscall"
)

// Classifier reports whether one address may be dialled. It receives the
// address resolution produced, never the name the link carried, so a name
// that looks public is judged on where it really points.
type Classifier func(net.IP) bool

// metadataIPv4 and metadataIPv6 are the link-scoped metadata addresses cloud
// vendors answer on. Both sit inside ranges DefaultClassifier refuses
// already, and both are named here so the metadata refusal survives on its
// own if a wider rule above them ever relaxes.
var (
	metadataIPv4 = net.IPv4(169, 254, 169, 254).To4()
	metadataIPv6 = net.ParseIP("fd00:ec2::254")
)

// transitionRanges are the deprecated IPv6 transition prefixes whose
// addresses carry an IPv4 destination inside them. The address that leaves
// the box is IPv6, while the place it really lands is an IPv4 this policy
// never judged, so both ranges are refused. The NAT64 prefix is absent on
// purpose, because a resolver on an IPv6-only network synthesizes it for
// ordinary public origins, and refusing it would refuse those origins too.
var transitionRanges = []struct {
	prefix [16]byte
	bytes  int
}{
	{[16]byte{0x20, 0x02}, 2}, // 6to4
	{[16]byte{}, 12},          // IPv4-compatible, deprecated
}

// DefaultClassifier refuses every address that is not clearly public. It
// refuses loopback, private, link-local and unique-local ranges, the carrier
// NAT range, multicast, and the unspecified and broadcast addresses. It
// refuses the cloud metadata addresses and the deprecated transition ranges
// that carry an IPv4 destination inside an IPv6 address. It allows every
// other address. A deployment that knows its own network's map wraps this
// one rather than replacing it.
func DefaultClassifier(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip.Equal(net.IPv4bcast) || ip.Equal(metadataIPv4) || ip.Equal(metadataIPv6) {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64.0.0/10, the carrier NAT range. The mask keeps the check
		// on the top two bits of the second octet, which span 64 to 127.
		if v4[0] == 100 && v4[1]&0xc0 == 64 {
			return false
		}
		return true
	}
	return !inTransition(ip)
}

// inTransition reports whether ip falls in one of the deprecated transition
// prefixes. It is called on IPv6 addresses only, never on a plain or mapped
// IPv4 address, which is judged as itself.
func inTransition(ip net.IP) bool {
	if len(ip) != 16 || ip.To4() != nil {
		return false
	}
	for _, r := range transitionRanges {
		matched := true
		for i := 0; i < r.bytes; i++ {
			if ip[i] != r.prefix[i] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// dialControl builds the Control hook the dialler calls for every address it
// is about to connect to, after resolution and before any packet is sent. A
// hook that returns an error stops that dial, so a link is judged on the
// address it actually resolved to. Anything the hook cannot parse is
// refused.
func dialControl(classify Classifier) func(string, string, syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return refusal(CodeBlockedAddress, "the dialled address could not be read")
		}
		ip := net.ParseIP(host)
		if ip == nil || !classify(ip) {
			return refusal(CodeBlockedAddress, "the address behind the link is refused")
		}
		return nil
	}
}

// checkURL judges one request address on its scheme, host and port. Get runs
// it on the first link and CheckRedirect runs it on every redirect target,
// so no hop leaves the scheme and port policy behind. An implicit port, the
// one a link leaves out, is judged like a written one.
func (c Config) checkURL(u *url.URL) error {
	switch u.Scheme {
	case "https":
	case "http":
		if !c.AllowHTTP {
			return refusal(CodeBadScheme, "the scheme is refused, only https is allowed by default")
		}
	case "":
		return refusal(CodeBadURL, "the link carries no scheme")
	default:
		return refusal(CodeBadScheme, "the scheme is refused, only https is allowed by default")
	}
	if u.Hostname() == "" {
		return refusal(CodeBadURL, "the link names no host")
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return refusal(CodeBadURL, "the link carries a port that is not a number")
	}
	if !c.portAllowed(n) {
		return refusal(CodeBadPort, "the port is not allowed")
	}
	return nil
}

// portAllowed reports whether port is on the allowlist. An unset or empty
// list allows the ordinary web ports, 80 and 443.
func (c Config) portAllowed(port int) bool {
	if len(c.Ports) == 0 {
		return port == 80 || port == 443
	}
	for _, allowed := range c.Ports {
		if port == allowed {
			return true
		}
	}
	return false
}

// typeAllowed reports whether mediaType is on the allowlist. An unset or
// empty allowlist allows every type. Entries compare on the type and subtype
// alone, so a parameter such as a charset never decides the match.
func (c Config) typeAllowed(mediaType string) bool {
	if len(c.ContentTypes) == 0 {
		return true
	}
	for _, allowed := range c.ContentTypes {
		if strings.EqualFold(strings.TrimSpace(allowed), mediaType) {
			return true
		}
	}
	return false
}

// mediaTypeOf reduces one Content-Type value to its lowercase type and
// subtype, dropping parameters. A value that does not parse returns the
// empty string, which no allowlist contains.
func mediaTypeOf(value string) string {
	if value == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return strings.ToLower(mediaType)
}
