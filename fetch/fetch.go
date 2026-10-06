// Package fetch gets a document from a link a user supplied, under an
// address policy enforced where the packet leaves.
//
// A link that reaches a server is a request forgery waiting to happen. The
// policy here refuses the addresses a link must never reach: loopback,
// private, link-local and unique-local ranges, the carrier NAT range,
// multicast, and the cloud metadata addresses. The refusal runs in the
// dialler's Control hook, on each address the dialler is about to connect
// to, after the name is resolved and before any packet is sent. A link whose
// name looks public is judged on the address the name really resolved to,
// which is what a rebinding attack cannot talk its way past.
//
// The scheme is https, or http beside it when Config.AllowHTTP says so. The
// port is 80 or 443 unless Config.Ports says otherwise. Redirects are capped
// by Config.MaxRedirects, and every hop runs the same address, scheme and
// port checks, because every hop dials again. The body is capped by
// Config.MaxBytes and refused whole rather than truncated. Config.Timeout
// and Config.HeaderTimeout cap the total time and one hop's wait for
// headers. The content type is checked against Config.ContentTypes and
// against the sniffed bytes.
//
// Every refusal and failure carries a stable code on Error, ready to be
// copied into an application's error envelope. Meta reads the open graph and
// twitter card tags out of one fetched page. The image address it hands back
// is fetched through Get, so it meets the same policy.
//
// The package renders no JavaScript, scrapes no particular site, keeps no
// cookies, and retries nothing. A caller who wants a wait between attempts
// wraps Get in throttle.
package fetch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// sniffLen is how much of the body the content type is sniffed from, the
// same window http.DetectContentType looks at.
const sniffLen = 512

// DefaultMaxBytes is the body cap when Config.MaxBytes is unset.
const DefaultMaxBytes = 10 << 20

// DefaultTimeout is the total time for one fetch, including every hop and
// the body, when Config.Timeout is unset.
const DefaultTimeout = 30 * time.Second

// DefaultHeaderTimeout is the time allowed for one hop to connect and start
// its response when Config.HeaderTimeout is unset.
const DefaultHeaderTimeout = 10 * time.Second

// DefaultMaxRedirects is the redirect cap when Config.MaxRedirects is unset.
const DefaultMaxRedirects = 5

// Config polices one fetch. The zero value is usable and means https only,
// ports 80 and 443, the default classifier, DefaultMaxRedirects redirects,
// DefaultTimeout and DefaultHeaderTimeout of waiting, and DefaultMaxBytes of
// body. A zero or negative limit means its default, so a config cannot ask
// for an unbounded fetch by forgetting a field.
type Config struct {
	// AllowHTTP lets http links through beside https. Off by default.
	AllowHTTP bool
	// Ports is the allowlist of destination ports. An unset or empty list
	// means 80 and 443. The port a link leaves out is judged like a
	// written one, so a list without 443 also refuses plain https links.
	Ports []int
	// MaxRedirects is how many redirects one fetch may follow. Zero or
	// negative means DefaultMaxRedirects.
	MaxRedirects int
	// Timeout is the total time for one fetch, including every hop, the
	// wait for headers, and the body. Zero or negative means
	// DefaultTimeout.
	Timeout time.Duration
	// HeaderTimeout is the time one hop may spend connecting and producing
	// its response headers. Zero or negative means DefaultHeaderTimeout.
	HeaderTimeout time.Duration
	// MaxBytes is the body cap. A body past the cap refuses the whole
	// fetch, and nothing over the cap is kept. Zero or negative means
	// DefaultMaxBytes.
	MaxBytes int64
	// ContentTypes is the allowlist the response must satisfy in its
	// Content-Type header and in its sniffed bytes, each an exact
	// type and subtype. An unset or empty list allows every type. With a
	// list set, a missing or unparseable header refuses the fetch. A body
	// with no bytes answers on its header alone.
	ContentTypes []string
	// Classifier decides which dialled addresses are allowed. Nil means
	// DefaultClassifier. Replacing it relaxes the guard, so a caller does
	// that only to admit addresses it has judged itself, such as a test
	// server on loopback.
	Classifier Classifier
	// Resolver resolves link names for the dialler. Nil means the system
	// resolver.
	Resolver *net.Resolver
	// Log receives one debug line per refused link. Nil means
	// slog.Default.
	Log *slog.Logger
}

// Response is one fetched document. The body is whole, never truncated, and
// capped by Config.MaxBytes.
type Response struct {
	// URL is the address the body finally came from, after every redirect.
	URL string
	// StatusCode is the HTTP status of the final hop.
	StatusCode int
	// Header is the header set of the final hop.
	Header http.Header
	// Body is the response body.
	Body []byte
}

// Get fetches the document at rawURL under the policy in cfg. Every address
// the fetch touches passes the scheme and port checks, on the first link and
// on every redirect hop. Every dial passes the classifier on the address
// resolution produced. The body comes back whole under the byte cap, refused
// instead of truncated past it.
//
// A refused or failed fetch returns a zero Response and an *Error carrying
// one of the Code constants. A fetch the caller cancelled returns the
// context's own error.
func Get(ctx context.Context, rawURL string, cfg Config) (Response, error) {
	base, err := url.Parse(rawURL)
	if err != nil {
		return Response{}, failure(CodeBadURL, "the link does not parse", err)
	}
	if err := cfg.checkURL(base); err != nil {
		cfg.log().Debug("fetch: link refused", "code", codeOf(err), "url", rawURL)
		return Response{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.timeout())
	defer cancel()

	client, closeIdle := cfg.client()
	defer closeIdle()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return Response{}, failure(CodeBadURL, "the link does not build a request", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Response{}, mapClientError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Response{}, refusal(CodeBadStatus, "the link answered outside 2xx")
	}
	if resp.ContentLength > cfg.maxBytes() {
		return Response{}, refusal(CodeTooLarge, "the body is larger than the byte cap")
	}

	body, err := readCapped(resp, cfg)
	if err != nil {
		return Response{}, err
	}
	return Response{
		URL:        resp.Request.URL.String(),
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       body,
	}, nil
}

// readCapped reads the response body under the byte cap. It sniffs the first
// bytes and refuses a type the allowlist does not hold before reading the
// rest, so a refusal stops the download early. A body with no bytes answers
// on its header alone, because an empty body sniffs as plain text and that
// says nothing about the response. A body past the cap refuses whole.
func readCapped(resp *http.Response, cfg Config) ([]byte, error) {
	limited := io.LimitReader(resp.Body, cfg.maxBytes()+1)
	head := make([]byte, sniffLen)
	n, err := io.ReadFull(limited, head)
	switch {
	case err == nil || err == io.EOF || err == io.ErrUnexpectedEOF:
		head = head[:n]
	default:
		return nil, bodyReadError(err)
	}
	declared := mediaTypeOf(resp.Header.Get("Content-Type"))
	if !cfg.typeAllowed(declared) {
		return nil, refusal(CodeBadType, "the content type is not allowed")
	}
	if n > 0 {
		sniffed := mediaTypeOf(http.DetectContentType(head))
		if !cfg.typeAllowed(sniffed) {
			return nil, refusal(CodeBadType, "the content type is not allowed")
		}
	}
	rest, err := io.ReadAll(limited)
	if err != nil {
		return nil, bodyReadError(err)
	}
	body := append(head, rest...)
	if int64(len(body)) > cfg.maxBytes() {
		return nil, refusal(CodeTooLarge, "the body is larger than the byte cap")
	}
	return body, nil
}

// client builds the HTTP client one fetch runs on. The dialler carries the
// Control hook, so the classifier judges every address resolution produced.
// The transport keeps no connection alive, so every hop dials fresh and the
// policy sees each one.
func (c Config) client() (*http.Client, func()) {
	dialer := &net.Dialer{
		Timeout:  c.headerTimeout(),
		Resolver: c.Resolver,
		Control:  dialControl(c.classifier()),
	}
	transport := &http.Transport{
		// No proxy. A proxy would dial its own address past the hook and
		// carry the link's host in a header the policy never sees.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ResponseHeaderTimeout: c.headerTimeout(),
		TLSHandshakeTimeout:   c.headerTimeout(),
		DisableKeepAlives:     true,
	}
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: c.checkRedirect(),
	}
	return client, transport.CloseIdleConnections
}

// checkRedirect builds the redirect rule. It caps the hop count and runs the
// scheme and port policy on every target, while the dialler's Control hook
// rejudges every address the hop resolves to.
func (c Config) checkRedirect() func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > c.maxRedirects() {
			return refusal(CodeTooManyRedirects, "the link redirects past the cap")
		}
		return c.checkURL(req.URL)
	}
}

// mapClientError sorts one client failure into the package's own codes. An
// error the package already coded, a redirect refusal or a dial refusal,
// comes back unchanged. A fetch the caller cancelled returns the context's
// own error. A deadline maps to the timeout code, and everything else below
// the policy maps to the unreachable code.
func mapClientError(err error) error {
	var fetchErr *Error
	if errors.As(err, &fetchErr) {
		return fetchErr
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if isDeadline(err) {
		return failure(CodeTimeout, "the fetch outlived its time limit", err)
	}
	return failure(CodeUnreachable, "the fetch did not complete", err)
}

// bodyReadError maps a failure that happened while the body was arriving. A
// deadline maps to the timeout code, everything else to the unreachable
// code.
func bodyReadError(err error) error {
	if isDeadline(err) {
		return failure(CodeTimeout, "the fetch outlived its time limit", err)
	}
	return failure(CodeUnreachable, "the body did not arrive", err)
}

// isDeadline reports whether err is a deadline running out, either the
// context's or one the net package set.
func isDeadline(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.As(err, &netErr) && netErr.Timeout()
}

// classifier returns the address policy, substituting the default for an
// unset one.
func (c Config) classifier() Classifier {
	if c.Classifier != nil {
		return c.Classifier
	}
	return DefaultClassifier
}

// timeout returns the total time budget, substituting the default for a zero
// or negative one.
func (c Config) timeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultTimeout
	}
	return c.Timeout
}

// headerTimeout returns the per-hop budget, substituting the default for a
// zero or negative one.
func (c Config) headerTimeout() time.Duration {
	if c.HeaderTimeout <= 0 {
		return DefaultHeaderTimeout
	}
	return c.HeaderTimeout
}

// maxBytes returns the body cap, substituting the default for a zero or
// negative one.
func (c Config) maxBytes() int64 {
	if c.MaxBytes <= 0 {
		return DefaultMaxBytes
	}
	return c.MaxBytes
}

// maxRedirects returns the redirect cap, substituting the default for a zero
// or negative one.
func (c Config) maxRedirects() int {
	if c.MaxRedirects <= 0 {
		return DefaultMaxRedirects
	}
	return c.MaxRedirects
}

// log returns the logger, substituting the default for a nil one.
func (c Config) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// codeOf returns the code an error from this package carries, for the debug
// log. An error of another shape returns unknown.
func codeOf(err error) string {
	var fetchErr *Error
	if errors.As(err, &fetchErr) {
		return fetchErr.Code
	}
	return "unknown"
}
