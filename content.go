package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// fetchMarkdown fetches a URL and returns its main content as Markdown.
func fetchMarkdown(url string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Feeds/1.0 (acme feed reader)")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB
	if err != nil {
		return "", err
	}

	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("HTML parse: %w", err)
	}

	// Prefer <article> or <main>; fall back to <body>.
	root := findNode(doc, atom.Article)
	if root == nil {
		root = findNode(doc, atom.Main)
	}
	if root == nil {
		root = findNode(doc, atom.Body)
	}
	if root == nil {
		root = doc
	}

	var sb strings.Builder
	renderMarkdown(&sb, root, 0)
	return cleanupMarkdown(sb.String()), nil
}

// ---------------------------------------------------------------------------
// HTML → Markdown walker
// ---------------------------------------------------------------------------

func renderMarkdown(sb *strings.Builder, n *html.Node, depth int) {
	switch n.Type {
	case html.TextNode:
		sb.WriteString(n.Data)
		return
	case html.ElementNode:
		// skip invisible/structural elements
		switch n.DataAtom {
		case atom.Script, atom.Style, atom.Nav, atom.Header,
			atom.Footer, atom.Form, atom.Button, atom.Input,
			atom.Iframe, atom.Noscript, atom.Aside:
			return
		}
	}

	if n.Type != html.ElementNode {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderMarkdown(sb, c, depth)
		}
		return
	}

	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		var level int
		switch n.DataAtom {
		case atom.H1:
			level = 1
		case atom.H2:
			level = 2
		case atom.H3:
			level = 3
		case atom.H4:
			level = 4
		case atom.H5:
			level = 5
		default:
			level = 6
		}
		sb.WriteString("\n" + strings.Repeat("#", level) + " ")
		renderChildren(sb, n, depth)
		sb.WriteString("\n\n")

	case atom.P:
		sb.WriteString("\n")
		renderChildren(sb, n, depth)
		sb.WriteString("\n\n")

	case atom.Br:
		sb.WriteString("\n")

	case atom.Hr:
		sb.WriteString("\n---\n\n")

	case atom.A:
		href := attr(n, "href")
		var inner strings.Builder
		renderChildren(&inner, n, depth)
		text := strings.TrimSpace(inner.String())
		if href == "" || href == text {
			sb.WriteString(text)
		} else {
			fmt.Fprintf(sb, "[%s](%s)", text, href)
		}

	case atom.Strong, atom.B:
		sb.WriteString("**")
		renderChildren(sb, n, depth)
		sb.WriteString("**")

	case atom.Em, atom.I:
		sb.WriteString("*")
		renderChildren(sb, n, depth)
		sb.WriteString("*")

	case atom.Code:
		// Check if parent is <pre> — handled there.
		if n.Parent != nil && n.Parent.DataAtom == atom.Pre {
			renderChildren(sb, n, depth)
			return
		}
		sb.WriteString("`")
		renderChildren(sb, n, depth)
		sb.WriteString("`")

	case atom.Pre:
		sb.WriteString("\n```\n")
		renderChildren(sb, n, depth)
		sb.WriteString("\n```\n\n")

	case atom.Blockquote:
		var inner strings.Builder
		renderChildren(&inner, n, depth+1)
		for _, line := range strings.Split(strings.TrimRight(inner.String(), "\n"), "\n") {
			fmt.Fprintf(sb, "> %s\n", line)
		}
		sb.WriteString("\n")

	case atom.Ul:
		sb.WriteString("\n")
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.DataAtom == atom.Li {
				var inner strings.Builder
				renderChildren(&inner, c, depth)
				text := strings.TrimSpace(inner.String())
				fmt.Fprintf(sb, "%s- %s\n", strings.Repeat("  ", depth), text)
			}
		}
		sb.WriteString("\n")

	case atom.Ol:
		sb.WriteString("\n")
		i := 1
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.DataAtom == atom.Li {
				var inner strings.Builder
				renderChildren(&inner, c, depth)
				text := strings.TrimSpace(inner.String())
				fmt.Fprintf(sb, "%s%d. %s\n", strings.Repeat("  ", depth), i, text)
				i++
			}
		}
		sb.WriteString("\n")

	case atom.Img:
		alt := attr(n, "alt")
		src := attr(n, "src")
		if alt != "" || src != "" {
			fmt.Fprintf(sb, "![%s](%s)", alt, src)
		}

	default:
		renderChildren(sb, n, depth)
	}
}

func renderChildren(sb *strings.Builder, n *html.Node, depth int) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderMarkdown(sb, c, depth)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func findNode(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findNode(c, a); found != nil {
			return found
		}
	}
	return nil
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

var multiNewline = regexp.MustCompile(`\n{3,}`)

func cleanupMarkdown(s string) string {
	s = multiNewline.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s) + "\n"
}
