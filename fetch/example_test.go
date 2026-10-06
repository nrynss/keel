package fetch_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"

	"github.com/nrynss/keel/fetch"
)

// ExampleGet fetches a page from a test server and reads its preview
// metadata. The classifier allows loopback because the test server listens
// there. A config that fetches links users supplied keeps the default
// classifier, which refuses loopback and every other internal range.
func ExampleGet() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!DOCTYPE html><html><head>")
		fmt.Fprint(w, `<meta property="og:title" content="A shared page">`)
		fmt.Fprint(w, `<meta property="og:image" content="/cover.png">`)
		fmt.Fprint(w, "</head><body></body></html>")
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		fmt.Println("parse failed:", err)
		return
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		fmt.Println("port failed:", err)
		return
	}
	cfg := fetch.Config{
		AllowHTTP:    true,
		Classifier:   func(ip net.IP) bool { return ip.IsLoopback() },
		Ports:        []int{port},
		ContentTypes: []string{"text/html"},
	}
	resp, err := fetch.Get(context.Background(), srv.URL, cfg)
	if err != nil {
		fmt.Println("fetch failed:", err)
		return
	}
	title, image := fetch.Meta(resp)
	fmt.Println(resp.StatusCode)
	fmt.Println(title)
	fmt.Println(strings.HasSuffix(image, "/cover.png"))
	// Output:
	// 200
	// A shared page
	// true
}
