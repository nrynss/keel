package fetch_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/nrynss/keel/fetch"
)

// serverConfig allows the loopback address and the port one test server
// listens on, and nothing else. The default judgement still runs on every
// dialled address outside loopback.
func serverConfig(t *testing.T, rawURL string, mutate func(*fetch.Config)) fetch.Config {
	cfg := fetch.Config{
		AllowHTTP:  true,
		Classifier: func(ip net.IP) bool { return ip.IsLoopback() },
		Ports:      []int{portOf(t, rawURL)},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

// codeOfFetch asserts that err is an Error carrying want and returns it for
// further assertions.
func codeOfFetch(t *testing.T, err error, want string) *fetch.Error {
	t.Helper()
	if err == nil {
		t.Fatal("the fetch was expected to fail")
	}
	var fetchErr *fetch.Error
	if !errors.As(err, &fetchErr) {
		t.Fatalf("the error is not a fetch error: %v", err)
	}
	if fetchErr.Code != want {
		t.Fatalf("code = %q, want %q (message %q)", fetchErr.Code, want, fetchErr.Message)
	}
	return fetchErr
}

func TestDefaultClassifierRefusesInternalRanges(t *testing.T) {
	cases := []struct {
		name string
		ip   string
	}{
		{"loopback", "127.0.0.1"},
		{"loopback last", "127.255.255.254"},
		{"loopback mapped", "::ffff:127.0.0.1"},
		{"loopback v6", "::1"},
		{"private ten", "10.1.2.3"},
		{"private 172", "172.16.0.9"},
		{"private 192", "192.168.1.1"},
		{"unique local", "fd00::1"},
		{"unique local fdef", "fdef:1234::1"},
		{"link local v4", "169.254.0.7"},
		{"link local v6", "fe80::1"},
		{"metadata v4", "169.254.169.254"},
		{"metadata v6", "fd00:ec2::254"},
		{"carrier nat low", "100.64.0.1"},
		{"carrier nat high", "100.127.255.254"},
		{"multicast v4", "224.0.0.1"},
		{"multicast v6", "ff02::1"},
		{"unspecified v4", "0.0.0.0"},
		{"unspecified v6", "::"},
		{"broadcast", "255.255.255.255"},
		{"six to four hides loopback", "2002:7f00:1::"},
		{"six to four hides metadata", "2002:a9fe:a9fe::"},
		{"ipv4 compatible hides loopback", "::127.0.0.1"},
		{"nat64 hides loopback", "64:ff9b::7f00:1"},
		{"nat64 hides metadata", "64:ff9b::a9fe:a9fe"},
		{"nat64 hides private ten", "64:ff9b::a00:1"},
		{"nat64 hides private 192", "64:ff9b::c0a8:101"},
		{"local use nat64 hides loopback", "64:ff9b:1::7f00:1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if fetch.DefaultClassifier(ip) {
				t.Fatalf("%s was allowed", tc.ip)
			}
		})
	}
}

func TestDefaultClassifierAllowsPublicAddresses(t *testing.T) {
	cases := []struct {
		name string
		ip   string
	}{
		{"public v4", "93.184.216.34"},
		{"public resolver", "8.8.8.8"},
		{"public v6", "2606:2800:220:1:248:1893:25c8:1946"},
		{"above carrier nat", "100.128.0.1"},
		{"below carrier nat", "100.63.255.255"},
		{"nat64 public origin", "64:ff9b::5db8:d822"},
		{"local use nat64 public origin", "64:ff9b:1::5db8:d822"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if !fetch.DefaultClassifier(ip) {
				t.Fatalf("%s was refused", tc.ip)
			}
		})
	}
	if fetch.DefaultClassifier(nil) {
		t.Fatal("a missing address was allowed")
	}
}

// fakeResolver answers every A question with ipv4 and every AAAA question
// with ipv6, whatever name is asked. It serves UDP and TCP, because a
// machine whose resolver configuration forces TCP still gets answers. The
// serving goroutines exit when the test closes the listeners.
func fakeResolver(t *testing.T, ipv4, ipv6 net.IP) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close(); ln.Close() })
	go serveDNSUDP(pc, ipv4, ipv6) // exits when the test closes pc
	go serveDNSTCP(ln, ipv4, ipv6) // exits when the test closes ln
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			address := ln.Addr().String()
			if len(network) > 0 && network[:3] == "udp" {
				address = pc.LocalAddr().String()
			}
			var d net.Dialer
			return d.DialContext(ctx, network, address)
		},
	}
}

// serveDNSUDP answers datagrams on pc until the connection closes.
func serveDNSUDP(pc net.PacketConn, ipv4, ipv6 net.IP) {
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return // the test closed the connection, so the loop exits
		}
		reply, ok := dnsReply(buf[:n], ipv4, ipv6)
		if !ok {
			continue
		}
		_, _ = pc.WriteTo(reply, from) // a client that stopped waiting cannot be told
	}
}

// serveDNSTCP accepts one connection at a time on ln until the listener
// closes.
func serveDNSTCP(ln net.Listener, ipv4, ipv6 net.IP) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // the test closed the listener, so the loop exits
		}
		go serveDNSConn(conn, ipv4, ipv6) // exits when the client hangs up
	}
}

// serveDNSConn answers length-prefixed DNS messages on one TCP connection
// until the client hangs up.
func serveDNSConn(conn net.Conn, ipv4, ipv6 net.IP) {
	defer conn.Close()
	var size [2]byte
	for {
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return // the client hung up, so the loop exits
		}
		msg := make([]byte, int(size[0])<<8|int(size[1]))
		if _, err := io.ReadFull(conn, msg); err != nil {
			return // the client hung up, so the loop exits
		}
		reply, ok := dnsReply(msg, ipv4, ipv6)
		if !ok {
			continue
		}
		framed := make([]byte, 2+len(reply))
		framed[0] = byte(len(reply) >> 8)
		framed[1] = byte(len(reply))
		copy(framed[2:], reply)
		_, _ = conn.Write(framed) // a client that stopped waiting cannot be told
	}
}

// dnsReply parses one DNS query and packs the answer, an A record for ipv4
// or nothing, and an AAAA record for ipv6 or nothing.
func dnsReply(query []byte, ipv4, ipv6 net.IP) ([]byte, bool) {
	var parsed dnsmessage.Message
	if err := parsed.Unpack(query); err != nil {
		return nil, false
	}
	reply := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 parsed.ID,
			Response:           true,
			OpCode:             parsed.OpCode,
			RecursionAvailable: true,
			RCode:              dnsmessage.RCodeSuccess,
		},
		Questions: parsed.Questions,
	}
	for _, question := range parsed.Questions {
		header := dnsmessage.ResourceHeader{
			Name:  question.Name,
			Type:  question.Type,
			Class: question.Class,
		}
		switch question.Type {
		case dnsmessage.TypeA:
			if ipv4 != nil {
				var a [4]byte
				copy(a[:], ipv4.To4())
				reply.Answers = append(reply.Answers, dnsmessage.Resource{
					Header: header,
					Body:   &dnsmessage.AResource{A: a},
				})
			}
		case dnsmessage.TypeAAAA:
			if ipv6 != nil {
				var a [16]byte
				copy(a[:], ipv6.To16())
				reply.Answers = append(reply.Answers, dnsmessage.Resource{
					Header: header,
					Body:   &dnsmessage.AAAAResource{AAAA: a},
				})
			}
		}
	}
	packed, err := reply.Pack()
	if err != nil {
		return nil, false
	}
	return packed, true
}

// portOf extracts the numeric port of a test server address.
func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("test server carries no port: %v", err)
	}
	return port
}

// TestGetJudgesTheDialledAddressNotTheName is the rebinding proof. The name
// looks public and resolves, through the injected resolver, to loopback. A
// policy that judged the name would pass the link, and only a policy that
// judges the address resolution produced can refuse it.
func TestGetJudgesTheDialledAddressNotTheName(t *testing.T) {
	var reached atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	port := portOf(t, srv.URL)
	target := fmt.Sprintf("http://public.example.test:%d/page", port)
	cfg := fetch.Config{
		AllowHTTP: true,
		Ports:     []int{port},
		Resolver:  fakeResolver(t, net.IPv4(127, 0, 0, 1), nil),
	}
	_, err := fetch.Get(context.Background(), target, cfg)
	codeOfFetch(t, err, fetch.CodeBlockedAddress)
	if reached.Load() != 0 {
		t.Fatal("the server was reached past a refused address")
	}
}

// TestGetReturnsTheHappyPath proves the whole pipeline against a server the
// policy allows: dial, headers, sniff, body.
func TestGetReturnsTheHappyPath(t *testing.T) {
	page := "<!DOCTYPE html><html><body>hello</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	cfg := serverConfig(t, srv.URL, func(c *fetch.Config) {
		c.ContentTypes = []string{"text/html"}
	})
	resp, err := fetch.Get(context.Background(), srv.URL+"/page", cfg)
	if err != nil {
		t.Fatalf("the allowed path failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.URL != srv.URL+"/page" {
		t.Fatalf("final url = %q, want %q", resp.URL, srv.URL+"/page")
	}
	if string(resp.Body) != page {
		t.Fatalf("body = %q, want the served page", resp.Body)
	}
	if resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("content type = %q", resp.Header.Get("Content-Type"))
	}
}

func TestGetRefusesSchemeOutsideThePolicy(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   string
	}{
		{"http without the switch", "http://example.test/page", fetch.CodeBadScheme},
		{"ftp", "ftp://example.test/file", fetch.CodeBadScheme},
		{"file", "file:///etc/passwd", fetch.CodeBadScheme},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fetch.Get(context.Background(), tc.target, fetch.Config{})
			codeOfFetch(t, err, tc.want)
		})
	}
}

func TestGetRefusesLinksThatNameNoDestination(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   string
	}{
		{"empty", "", fetch.CodeBadURL},
		{"no host", "https:///path", fetch.CodeBadURL},
		{"port not a number", "https://example.test:notaport/", fetch.CodeBadURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fetch.Get(context.Background(), tc.target, fetch.Config{})
			codeOfFetch(t, err, tc.want)
		})
	}
}

func TestGetRefusesPortsOutsideThePolicy(t *testing.T) {
	_, err := fetch.Get(context.Background(), "https://example.test:8443/", fetch.Config{})
	codeOfFetch(t, err, fetch.CodeBadPort)
}

func TestGetAllowsPortsTheConfigNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!DOCTYPE html><html></html>")
	}))
	defer srv.Close()

	cfg := serverConfig(t, srv.URL, nil)
	resp, err := fetch.Get(context.Background(), srv.URL, cfg)
	if err != nil {
		t.Fatalf("a configured port was refused: %v", err)
	}
	if len(resp.Body) == 0 {
		t.Fatal("the body came back empty")
	}
}

func TestGetRechecksSchemeOnEveryRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	}))
	defer srv.Close()

	_, err := fetch.Get(context.Background(), srv.URL, serverConfig(t, srv.URL, nil))
	codeOfFetch(t, err, fetch.CodeBadScheme)
}

func TestGetRechecksPortOnEveryRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:9001/", http.StatusFound)
	}))
	defer srv.Close()

	_, err := fetch.Get(context.Background(), srv.URL, serverConfig(t, srv.URL, nil))
	codeOfFetch(t, err, fetch.CodeBadPort)
}

// TestGetRechecksTheAddressOnEveryRedirect sends a link at a server the
// policy allows, which redirects to a metadata address. The refusal must
// come from the dialler hook, on the address the hop resolved to.
func TestGetRechecksTheAddressOnEveryRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	_, err := fetch.Get(context.Background(), srv.URL, serverConfig(t, srv.URL, func(c *fetch.Config) {
		c.Ports = append(c.Ports, 80)
	}))
	codeOfFetch(t, err, fetch.CodeBlockedAddress)
}

func TestGetRefusesRedirectsPastTheCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()

	cfg := serverConfig(t, srv.URL, func(c *fetch.Config) { c.MaxRedirects = 2 })
	_, err := fetch.Get(context.Background(), srv.URL, cfg)
	codeOfFetch(t, err, fetch.CodeTooManyRedirects)
}

func TestGetRefusesBodiesPastTheCapWithDeclaredLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "0123456789012345678901234567890123456789")
	}))
	defer srv.Close()

	cfg := serverConfig(t, srv.URL, func(c *fetch.Config) { c.MaxBytes = 10 })
	_, err := fetch.Get(context.Background(), srv.URL, cfg)
	codeOfFetch(t, err, fetch.CodeTooLarge)
}

func TestGetRefusesBodiesPastTheCapWithUnknownLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		flusher := w.(http.Flusher)
		for i := 0; i < 40; i++ {
			fmt.Fprint(w, "0123456789")
			flusher.Flush()
		}
	}))
	defer srv.Close()

	cfg := serverConfig(t, srv.URL, func(c *fetch.Config) { c.MaxBytes = 10 })
	_, err := fetch.Get(context.Background(), srv.URL, cfg)
	codeOfFetch(t, err, fetch.CodeTooLarge)
}

func TestGetKeepsABodyAtTheCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "0123456789")
	}))
	defer srv.Close()

	cfg := serverConfig(t, srv.URL, func(c *fetch.Config) { c.MaxBytes = 10 })
	resp, err := fetch.Get(context.Background(), srv.URL, cfg)
	if err != nil {
		t.Fatalf("a body at the cap was refused: %v", err)
	}
	if string(resp.Body) != "0123456789" {
		t.Fatalf("body = %q, want the whole page", resp.Body)
	}
}

func TestGetRefusesADeclaredTypeTheSniffDisagreesWith(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		fmt.Fprint(w, "<!DOCTYPE html><html></html>")
	}))
	defer srv.Close()

	cfg := serverConfig(t, srv.URL, func(c *fetch.Config) {
		c.ContentTypes = []string{"image/png"}
	})
	_, err := fetch.Get(context.Background(), srv.URL, cfg)
	codeOfFetch(t, err, fetch.CodeBadType)
}

func TestGetRefusesASniffedTypeOutsideTheAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "\x89PNG\r\n\x1a\n")
	}))
	defer srv.Close()

	cfg := serverConfig(t, srv.URL, func(c *fetch.Config) {
		c.ContentTypes = []string{"text/html"}
	})
	_, err := fetch.Get(context.Background(), srv.URL, cfg)
	codeOfFetch(t, err, fetch.CodeBadType)
}

func TestGetRefusesAnUnparsableDeclaredType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "///junk")
		fmt.Fprint(w, "anything")
	}))
	defer srv.Close()

	cfg := serverConfig(t, srv.URL, func(c *fetch.Config) {
		c.ContentTypes = []string{"text/html"}
	})
	_, err := fetch.Get(context.Background(), srv.URL, cfg)
	codeOfFetch(t, err, fetch.CodeBadType)
}

func TestGetAllowsEveryTypeWithoutAnAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	resp, err := fetch.Get(context.Background(), srv.URL, serverConfig(t, srv.URL, nil))
	if err != nil {
		t.Fatalf("an empty allowlist refused a fetch: %v", err)
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Fatalf("body = %q", resp.Body)
	}
}

func TestGetRefusesAFetchPastItsTotalBudget(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	cfg := serverConfig(t, srv.URL, func(c *fetch.Config) { c.Timeout = 50 * time.Millisecond })
	_, err := fetch.Get(context.Background(), srv.URL, cfg)
	codeOfFetch(t, err, fetch.CodeTimeout)
}

func TestGetRefusesAFetchPastItsHeaderBudget(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer func() { close(release); srv.Close() }()

	cfg := serverConfig(t, srv.URL, func(c *fetch.Config) { c.HeaderTimeout = 50 * time.Millisecond })
	_, err := fetch.Get(context.Background(), srv.URL, cfg)
	codeOfFetch(t, err, fetch.CodeTimeout)
}

func TestGetRefusesAStatusOutside2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := fetch.Get(context.Background(), srv.URL, serverConfig(t, srv.URL, nil))
	codeOfFetch(t, err, fetch.CodeBadStatus)
}

func TestGetMapsADeadConnectionToUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed := srv.URL
	srv.Close()

	_, err := fetch.Get(context.Background(), closed, serverConfig(t, closed, nil))
	fetchErr := codeOfFetch(t, err, fetch.CodeUnreachable)
	if fetchErr.Unwrap() == nil {
		t.Fatal("the transport cause is not wrapped below the error")
	}
	if fetchErr.Message == "" {
		t.Fatal("the error carries no message")
	}
}

func TestGetReturnsTheCallerCancellationUntouched(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fetch.Get(ctx, "https://example.test/", fetch.Config{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context cancellation", err)
	}
	var fetchErr *fetch.Error
	if errors.As(err, &fetchErr) {
		t.Fatal("a caller cancellation was coded as a fetch failure")
	}
}
