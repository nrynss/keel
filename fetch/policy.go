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
// never judged, so both ranges are refused outright.
var transitionRanges = []struct {
	prefix [16]byte
	bytes  int
}{
	{[16]byte{0x20, 0x02}, 2}, // 6to4
	{[16]byte{}, 12},          // IPv4-compatible, deprecated
}

// DefaultClassifier refuses every address that is not clearly public. It
// refuses loopback, private, link-local and unique-local ranges, the carrier
// NAT range, the IETF protocol assignments range, and the benchmarking
// range. It refuses multicast, the reserved range with the broadcast address
// in it, and the whole self range of zero addresses. It refuses the cloud
// metadata addresses and the deprecated transition ranges that carry an IPv4
// destination inside an IPv6 address. A NAT64 address carries its IPv4
// destination in its last four bytes, judged by the IPv4 rules, so a
// synthesised public origin still passes. It allows every other address. A
// deployment that knows its own network's map wraps this one rather than
// replacing it.
func DefaultClassifier(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// A plain or IPv4-mapped address is judged as itself.
		return !ipv4Refused(v4)
	}
	return ipv6Allowed(ip)
}

// ipv4Refused reports whether one IPv4 address falls in a range the policy
// refuses. It takes the address in IPv4 form, plain, mapped, or read out of
// an embedding, and judges it by every IPv4 rule. An address that does not
// reduce to IPv4 form is refused.
func ipv4Refused(v4 net.IP) bool {
	v4 = v4.To4()
	if v4 == nil {
		return true
	}
	if v4[0] == 0 {
		// 0.0.0.0/8, the self range. Nothing to dial.
		return true
	}
	if v4.IsLoopback() || v4.IsPrivate() || v4.IsLinkLocalUnicast() || v4.IsMulticast() {
		return true
	}
	if v4.Equal(metadataIPv4) {
		return true
	}
	// 100.64.0.0/10, the carrier NAT range. The mask keeps the check on
	// the top two bits of the second octet, which span 64 to 127.
	if v4[0] == 100 && v4[1]&0xc0 == 64 {
		return true
	}
	// 192.0.0.0/24, the IETF protocol assignments range.
	if v4[0] == 192 && v4[1] == 0 && v4[2] == 0 {
		return true
	}
	// 198.18.0.0/15, the benchmarking range. The mask spans both values
	// the second octet takes in the range, 18 and 19.
	if v4[0] == 198 && v4[1]&0xfe == 18 {
		return true
	}
	// 240.0.0.0/4, the reserved range, the broadcast address at its end
	// included.
	return v4[0]&0xf0 == 240
}

// ipv6Allowed judges a native IPv6 address by the IPv6 rules. An address
// under a transition prefix is refused outright. An address under a NAT64
// prefix is judged on the IPv4 address it carries, because a gateway
// translates that address on the way out.
func ipv6Allowed(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip.Equal(metadataIPv6) {
		return false
	}
	if inTransition(ip) {
		return false
	}
	if nat64(ip) {
		return !ipv4Refused(net.IPv4(ip[12], ip[13], ip[14], ip[15]))
	}
	return true
}

// nat64 reports whether ip carries an IPv4 address in its last four bytes
// under one of the NAT64 prefixes. Both allocated prefixes start with the
// same four bytes, and no global unicast address does, so the marker alone
// decides the question.
func nat64(ip net.IP) bool {
	return len(ip) == 16 && ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b
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
