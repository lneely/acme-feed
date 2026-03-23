package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// nsDecl strips xmlns and xmlns:prefix declarations (both quote styles).
// Go's encoding/xml only matches unqualified tags against elements with no
// namespace; Atom feeds put every element in the Atom namespace, so without
// stripping the declaration none of our struct field tags would match.
var nsDecl = regexp.MustCompile(`\s+xmlns(?::\w+)?=(?:"[^"]*"|'[^']*')`)

// nsPrefix strips namespace prefixes from element start/end tags
// (e.g. <dc:creator> → <creator>, </rdf:RDF> → </RDF>).
// After stripping xmlns declarations the prefixes become undeclared; Go's
// xml decoder may reject them even in non-strict mode.
var nsPrefix = regexp.MustCompile(`(</?)[A-Za-z][\w-]*:`)

func normalizeXML(data []byte) []byte {
	data = nsDecl.ReplaceAll(data, nil)
	data = nsPrefix.ReplaceAll(data, []byte("$1"))
	return data
}

// ---------------------------------------------------------------------------
// RSS / Atom XML structures
// ---------------------------------------------------------------------------

type rssFeed struct {
	XMLName xml.Name    `xml:"rss"`
	Channel rssChannel  `xml:"channel"`
}

type rssChannel struct {
	Title string    `xml:"title"`
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
}

type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Title   atomText    `xml:"title"`
	Entries []atomEntry `xml:"entry"`
}

type atomText struct {
	Value string `xml:",chardata"`
}

type atomEntry struct {
	ID      string   `xml:"id"`
	Title   atomText `xml:"title"`
	Updated string   `xml:"updated"`
	Links   []struct {
		Href string `xml:"href,attr"`
		Rel  string `xml:"rel,attr"`
	} `xml:"link"`
	Summary atomText `xml:"summary"`
	Content atomText `xml:"content"`
}

// ---------------------------------------------------------------------------
// Fetch result
// ---------------------------------------------------------------------------

type FetchResult struct {
	FeedTitle    string
	Entries      []*Entry
	ETag         string
	LastModified string
	NotModified  bool
}

// ---------------------------------------------------------------------------
// Fetch and parse
// ---------------------------------------------------------------------------

func fetchFeed(url, etag, lastModified string) (*FetchResult, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}
	req.Header.Set("User-Agent", "Feeds/1.0 (acme feed reader)")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return &FetchResult{NotModified: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB limit
	if err != nil {
		return nil, err
	}

	result, err := parseFeed(body)
	if err != nil {
		return nil, err
	}
	result.ETag = resp.Header.Get("ETag")
	result.LastModified = resp.Header.Get("Last-Modified")
	return result, nil
}

func parseFeed(data []byte) (*FetchResult, error) {
	// Normalise: strip xmlns declarations and namespace prefixes from element
	// names so encoding/xml plain-name tags match regardless of feed format.
	clean := normalizeXML(data)

	// Peek at root element local name to decide RSS vs Atom.
	dec := xml.NewDecoder(bytes.NewReader(clean))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity

	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("XML parse: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "rss":
			return parseRSS(clean)
		case "feed":
			return parseAtom(clean)
		case "RDF":
			return parseRDF1(clean)
		default:
			return nil, fmt.Errorf("unrecognized feed root element: %s", se.Name.Local)
		}
	}
}

func parseRSS(data []byte) (*FetchResult, error) {
	var feed rssFeed
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	if err := dec.Decode(&feed); err != nil {
		return nil, fmt.Errorf("RSS decode: %w", err)
	}

	fetchedAt := time.Now().UTC().Format(time.RFC3339)
	result := &FetchResult{FeedTitle: strings.TrimSpace(feed.Channel.Title)}

	for _, item := range feed.Channel.Items {
		ts := parseRSSDate(item.PubDate)
		if ts == "" {
			ts = fetchedAt
		}
		guid := strings.TrimSpace(item.GUID)
		if guid == "" {
			guid = synthGUID(item.Link, item.Title)
		}
		summary := strings.TrimSpace(item.Description)
		if len(summary) > 4096 {
			summary = truncateUTF8(summary, 4096)
		}
		result.Entries = append(result.Entries, &Entry{
			GUID:      guid,
			Timestamp: ts,
			Title:     strings.TrimSpace(item.Title),
			URL:       strings.TrimSpace(item.Link),
			Summary:   summary,
			FetchedAt: fetchedAt,
		})
	}
	return result, nil
}

// RSS 1.0 / RDF Site Summary structures.
// After namespace stripping, the root is <RDF>, <channel> holds the feed
// title, and <item> elements are direct children of <RDF> (not of <channel>).
// dc:date is stripped to <date>; dc:description may appear too but we use
// <description> which is in the core RSS 1.0 namespace.
type rdf1Feed struct {
	XMLName xml.Name    `xml:"RDF"`
	Channel rdf1Channel `xml:"channel"`
	Items   []rdf1Item  `xml:"item"`
}

type rdf1Channel struct {
	Title string `xml:"title"`
}

type rdf1Item struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	Date        string `xml:"date"`    // dc:date → <date>
	Identifier  string `xml:"identifier"` // dc:identifier → <identifier>
}

func parseRDF1(data []byte) (*FetchResult, error) {
	var feed rdf1Feed
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	if err := dec.Decode(&feed); err != nil {
		return nil, fmt.Errorf("RSS 1.0 decode: %w", err)
	}

	fetchedAt := time.Now().UTC().Format(time.RFC3339)
	result := &FetchResult{FeedTitle: strings.TrimSpace(feed.Channel.Title)}

	for _, item := range feed.Items {
		ts := parseRSSDate(item.Date)
		if ts == "" {
			ts = fetchedAt
		}
		guid := strings.TrimSpace(item.Identifier)
		if guid == "" {
			guid = synthGUID(item.Link, item.Title)
		}
		summary := strings.TrimSpace(item.Description)
		if len(summary) > 4096 {
			summary = truncateUTF8(summary, 4096)
		}
		result.Entries = append(result.Entries, &Entry{
			GUID:      guid,
			Timestamp: ts,
			Title:     strings.TrimSpace(item.Title),
			URL:       strings.TrimSpace(item.Link),
			Summary:   summary,
			FetchedAt: fetchedAt,
		})
	}
	return result, nil
}

func parseAtom(data []byte) (*FetchResult, error) {
	var feed atomFeed
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	if err := dec.Decode(&feed); err != nil {
		return nil, fmt.Errorf("Atom decode: %w", err)
	}

	fetchedAt := time.Now().UTC().Format(time.RFC3339)
	result := &FetchResult{FeedTitle: strings.TrimSpace(feed.Title.Value)}

	for _, e := range feed.Entries {
		ts := strings.TrimSpace(e.Updated)
		if ts == "" {
			ts = fetchedAt
		} else {
			ts = normalizeTimestamp(ts, fetchedAt)
		}
		link := ""
		for _, l := range e.Links {
			if l.Rel == "alternate" || l.Rel == "" {
				link = l.Href
				break
			}
		}
		guid := strings.TrimSpace(e.ID)
		if guid == "" {
			guid = synthGUID(link, e.Title.Value)
		}
		summary := strings.TrimSpace(e.Summary.Value)
		if summary == "" {
			summary = strings.TrimSpace(e.Content.Value)
		}
		if len(summary) > 4096 {
			summary = truncateUTF8(summary, 4096)
		}
		result.Entries = append(result.Entries, &Entry{
			GUID:      guid,
			Timestamp: ts,
			Title:     strings.TrimSpace(e.Title.Value),
			URL:       link,
			Summary:   summary,
			FetchedAt: fetchedAt,
		})
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Date helpers
// ---------------------------------------------------------------------------

var rssDateFormats = []string{
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05Z",
	"2006-01-02",
}

func parseRSSDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	now := time.Now().UTC()
	for _, f := range rssDateFormats {
		if t, err := time.Parse(f, s); err == nil {
			if t.After(now) {
				t = now
			}
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

func normalizeTimestamp(s, fallback string) string {
	s = strings.TrimSpace(s)
	now := time.Now().UTC()
	for _, f := range rssDateFormats {
		if t, err := time.Parse(f, s); err == nil {
			if t.After(now) {
				t = now
			}
			return t.UTC().Format(time.RFC3339)
		}
	}
	return fallback
}
