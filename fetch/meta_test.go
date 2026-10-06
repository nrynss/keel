package fetch_test

import (
	"testing"

	"github.com/nrynss/keel/fetch"
)

func TestMetaReadsOpenGraphAndResolvesTheImage(t *testing.T) {
	page := `<!DOCTYPE html><html><head>
	<meta property="og:title" content="A gate opens">
	<meta property="og:image" content="/img/cover.png">
	<meta name="twitter:image" content="https://img.example.test/fallback.png">
	</head><body></body></html>`
	resp := fetch.Response{URL: "https://page.example.test/products/1", Body: []byte(page)}

	title, image := fetch.Meta(resp)
	if title != "A gate opens" {
		t.Fatalf("title = %q", title)
	}
	if image != "https://page.example.test/img/cover.png" {
		t.Fatalf("image = %q", image)
	}
}

func TestMetaFallsBackToTheTwitterImage(t *testing.T) {
	page := `<!DOCTYPE html><html><head>
	<meta property="og:title" content="A gate opens">
	<meta name="twitter:image" content="https://img.example.test/fallback.png">
	</head><body></body></html>`
	resp := fetch.Response{URL: "https://page.example.test/products/1", Body: []byte(page)}

	_, image := fetch.Meta(resp)
	if image != "https://img.example.test/fallback.png" {
		t.Fatalf("image = %q", image)
	}
}

func TestMetaPrefersTheOpenGraphImageWhenBothExist(t *testing.T) {
	page := `<!DOCTYPE html><html><head>
	<meta property="og:image" content="/og.png">
	<meta name="twitter:image" content="/twitter.png">
	</head><body></body></html>`
	resp := fetch.Response{URL: "https://page.example.test/", Body: []byte(page)}

	_, image := fetch.Meta(resp)
	if image != "https://page.example.test/og.png" {
		t.Fatalf("image = %q", image)
	}
}

func TestMetaReadsTheNameAttributeWhenPropertyIsMissing(t *testing.T) {
	page := `<!DOCTYPE html><html><head>
	<meta name="og:title" content="By name">
	<meta name="og:image" content="cover.png">
	</head><body></body></html>`
	resp := fetch.Response{URL: "https://page.example.test/products/1", Body: []byte(page)}

	title, image := fetch.Meta(resp)
	if title != "By name" {
		t.Fatalf("title = %q", title)
	}
	if image != "https://page.example.test/products/cover.png" {
		t.Fatalf("image = %q", image)
	}
}

func TestMetaTakesTheFirstImageWhenOneRepeats(t *testing.T) {
	page := `<!DOCTYPE html><html><head>
	<meta property="og:image" content="/first.png">
	<meta property="og:image" content="/second.png">
	</head><body></body></html>`
	resp := fetch.Response{URL: "https://page.example.test/", Body: []byte(page)}

	_, image := fetch.Meta(resp)
	if image != "https://page.example.test/first.png" {
		t.Fatalf("image = %q", image)
	}
}

func TestMetaSkipsAnUnresolvableImageForATweetableOne(t *testing.T) {
	page := `<!DOCTYPE html><html><head>
	<meta property="og:image" content="http://exa mple.test/broken.png">
	<meta name="twitter:image" content="/fallback.png">
	</head><body></body></html>`
	resp := fetch.Response{URL: "https://page.example.test/", Body: []byte(page)}

	_, image := fetch.Meta(resp)
	if image != "https://page.example.test/fallback.png" {
		t.Fatalf("image = %q", image)
	}
}

func TestMetaOnAPageThatDeclaresNothing(t *testing.T) {
	page := "<!DOCTYPE html><html><body>plain</body></html>"
	resp := fetch.Response{URL: "https://page.example.test/", Body: []byte(page)}

	title, image := fetch.Meta(resp)
	if title != "" || image != "" {
		t.Fatalf("title = %q, image = %q, want both empty", title, image)
	}
}

func TestMetaOnAResponseWithoutAnAddress(t *testing.T) {
	resp := fetch.Response{Body: []byte("<meta property=\"og:title\" content=\"x\">")}

	title, image := fetch.Meta(resp)
	if title != "x" {
		t.Fatalf("title = %q", title)
	}
	if image != "" {
		t.Fatalf("image = %q, want empty", image)
	}
}
