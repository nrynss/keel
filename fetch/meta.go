package fetch

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// Meta reads link preview metadata out of one fetched page. It returns the
// open graph title, and the open graph image falling back to the twitter
// card image when the page declares no open graph image. The tags match on
// the property or the name attribute, whichever the page carries.
//
// A relative image address resolves against the final URL of the response,
// so the caller can fetch the image back through Get and it meets the same
// address policy. An image address that does not resolve is skipped, and a
// page that declares nothing returns empty strings.
func Meta(resp Response) (title, image string) {
	base, err := url.Parse(resp.URL)
	if err != nil {
		return "", ""
	}
	doc, err := html.Parse(bytes.NewReader(resp.Body))
	if err != nil {
		return "", ""
	}
	var ogTitle, ogImage, twImage string
	for node := doc; node != nil; node = nextNode(node) {
		if node.Type != html.ElementNode || node.Data != "meta" {
			continue
		}
		key := attr(node, "property")
		if key == "" {
			key = attr(node, "name")
		}
		content := strings.TrimSpace(attr(node, "content"))
		if content == "" {
			continue
		}
		switch key {
		case "og:title":
			if ogTitle == "" {
				ogTitle = content
			}
		case "og:image":
			if ogImage == "" {
				ogImage = content
			}
		case "twitter:image":
			if twImage == "" {
				twImage = content
			}
		}
	}
	image = resolve(base, ogImage)
	if image == "" {
		image = resolve(base, twImage)
	}
	return ogTitle, image
}

// nextNode walks a tree of nodes depth first without recursion, so a deeply
// nested page cannot grow the stack.
func nextNode(n *html.Node) *html.Node {
	if n.FirstChild != nil {
		return n.FirstChild
	}
	for c := n; c != nil; c = c.Parent {
		if c.NextSibling != nil {
			return c.NextSibling
		}
	}
	return nil
}

// attr returns one attribute value of an element, or the empty string.
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// resolve resolves one reference against a base address. An empty or
// unresolvable reference returns the empty string.
func resolve(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || base == nil {
		return ""
	}
	parsed, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return base.ResolveReference(parsed).String()
}
