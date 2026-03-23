package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ---------------------------------------------------------------------------
// Data types
// ---------------------------------------------------------------------------

type Subscription struct {
	URL          string `json:"url"`
	Slug         string `json:"slug"`
	Title        string `json:"title"`
	SubscribedAt string `json:"subscribed_at"`
	LastFetched  string `json:"last_fetched,omitempty"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

type Entry struct {
	GUID      string `json:"guid"`
	Timestamp string `json:"timestamp"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Summary   string `json:"summary"`
	FetchedAt string `json:"fetched_at"`
}

// entryFile pairs an entry with its generated filename and state flags.
type entryFile struct {
	filename string
	entry    *Entry
	read     bool
	pinned   bool
}

// ---------------------------------------------------------------------------
// FeedStore — in-memory state + disk persistence
// ---------------------------------------------------------------------------

type FeedStore struct {
	mu   sync.RWMutex
	subs []*Subscription        // ordered by SubscribedAt
	// slug → entries sorted by timestamp descending
	entries map[string][]*Entry
	// slug → set of read GUIDs
	readSets map[string]map[string]bool
	// slug → set of pinned GUIDs
	pinnedSets map[string]map[string]bool
	// slug → computed entryFile list (rebuilt after any mutation)
	files map[string][]entryFile

	dataDir string // ~/.local/share/acme-feed
}

func newFeedStore() (*FeedStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dataDir := filepath.Join(home, ".local", "share", "acme-feed")
	for _, d := range []string{
		dataDir,
		filepath.Join(dataDir, "feeds"),
		filepath.Join(dataDir, "read"),
		filepath.Join(dataDir, "pinned"),
	} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return nil, err
		}
	}

	fs := &FeedStore{
		entries:    make(map[string][]*Entry),
		readSets:   make(map[string]map[string]bool),
		pinnedSets: make(map[string]map[string]bool),
		files:      make(map[string][]entryFile),
		dataDir:    dataDir,
	}
	if err := fs.load(); err != nil {
		return nil, err
	}
	return fs, nil
}

// ---------------------------------------------------------------------------
// Load from disk
// ---------------------------------------------------------------------------

func (s *FeedStore) load() error {
	subs, err := s.loadSubscriptions()
	if err != nil {
		return err
	}
	s.subs = subs

	for _, sub := range subs {
		entries, err := s.loadEntries(sub.Slug)
		if err != nil {
			return fmt.Errorf("load entries %s: %w", sub.Slug, err)
		}
		s.entries[sub.Slug] = entries

		readSet, err := s.loadReadSet(sub.Slug)
		if err != nil {
			return fmt.Errorf("load read set %s: %w", sub.Slug, err)
		}
		s.readSets[sub.Slug] = readSet

		pinnedSet, err := s.loadGUIDFile(filepath.Join(s.dataDir, "pinned", sub.Slug))
		if err != nil {
			return fmt.Errorf("load pinned set %s: %w", sub.Slug, err)
		}
		s.pinnedSets[sub.Slug] = pinnedSet
		s.rebuildFiles(sub.Slug)
	}
	return nil
}

func (s *FeedStore) loadSubscriptions() ([]*Subscription, error) {
	path := filepath.Join(s.dataDir, "subscriptions.jsonl")
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var subs []*Subscription
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var sub Subscription
		if err := json.Unmarshal([]byte(line), &sub); err != nil {
			continue
		}
		subs = append(subs, &sub)
	}
	return subs, sc.Err()
}

func (s *FeedStore) loadEntries(slug string) ([]*Entry, error) {
	path := filepath.Join(s.dataDir, "feeds", slug+".jsonl")
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	seen := make(map[string]bool)
	var entries []*Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if seen[e.GUID] {
			continue
		}
		seen[e.GUID] = true
		entries = append(entries, &e)
	}
	// Sort descending by timestamp.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp > entries[j].Timestamp
	})
	return entries, sc.Err()
}

func (s *FeedStore) loadReadSet(slug string) (map[string]bool, error) {
	return s.loadGUIDFile(filepath.Join(s.dataDir, "read", slug))
}

// loadGUIDFile reads a newline-separated file of GUIDs into a set.
func (s *FeedStore) loadGUIDFile(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return make(map[string]bool), nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	set := make(map[string]bool)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if g := strings.TrimSpace(sc.Text()); g != "" {
			set[g] = true
		}
	}
	return set, sc.Err()
}

// rewriteGUIDFile atomically rewrites a GUID set file.
func rewriteGUIDFile(path string, set map[string]bool) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for guid := range set {
		fmt.Fprintln(f, guid)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Persist to disk
// ---------------------------------------------------------------------------

func (s *FeedStore) saveSubscriptions() error {
	path := filepath.Join(s.dataDir, "subscriptions.jsonl")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, sub := range s.subs {
		if err := enc.Encode(sub); err != nil {
			return err
		}
	}
	return nil
}

func (s *FeedStore) appendEntries(slug string, entries []*Entry) error {
	if len(entries) == 0 {
		return nil
	}
	path := filepath.Join(s.dataDir, "feeds", slug+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

func (s *FeedStore) appendGUID(path, guid string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, guid)
	return err
}

func (s *FeedStore) removeSubscriptionFiles(slug string) {
	os.Remove(filepath.Join(s.dataDir, "feeds", slug+".jsonl"))
	os.Remove(filepath.Join(s.dataDir, "read", slug))
	os.Remove(filepath.Join(s.dataDir, "pinned", slug))
}

// ---------------------------------------------------------------------------
// Mutations (all hold mu.Lock)
// ---------------------------------------------------------------------------

// Subscribe adds a new feed. Returns an error if the slug already exists.
func (s *FeedStore) subscribe(sub *Subscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.subs {
		if existing.Slug == sub.Slug {
			// Make slug unique by appending -2, -3, etc.
			for i := 2; ; i++ {
				candidate := fmt.Sprintf("%s-%d", sub.Slug, i)
				found := false
				for _, e := range s.subs {
					if e.Slug == candidate {
						found = true
						break
					}
				}
				if !found {
					sub.Slug = candidate
					break
				}
			}
		}
		if existing.URL == sub.URL {
			return fmt.Errorf("already subscribed: %s", existing.Slug)
		}
	}
	s.subs = append(s.subs, sub)
	s.entries[sub.Slug] = nil
	s.readSets[sub.Slug] = make(map[string]bool)
	s.pinnedSets[sub.Slug] = make(map[string]bool)
	s.files[sub.Slug] = nil
	return s.saveSubscriptions()
}

// Unsubscribe removes a feed by slug.
func (s *FeedStore) unsubscribe(slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	var remaining []*Subscription
	for _, sub := range s.subs {
		if sub.Slug == slug {
			found = true
		} else {
			remaining = append(remaining, sub)
		}
	}
	if !found {
		return fmt.Errorf("no feed with slug %q", slug)
	}
	s.subs = remaining
	delete(s.entries, slug)
	delete(s.readSets, slug)
	delete(s.pinnedSets, slug)
	delete(s.files, slug)
	s.removeSubscriptionFiles(slug)
	return s.saveSubscriptions()
}

// addEntries deduplicates and appends new entries for a feed.
func (s *FeedStore) addEntries(slug string, newEntries []*Entry) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.entries[slug]
	seen := make(map[string]bool, len(existing))
	for _, e := range existing {
		seen[e.GUID] = true
	}

	var toAdd []*Entry
	for _, e := range newEntries {
		if !seen[e.GUID] {
			seen[e.GUID] = true
			toAdd = append(toAdd, e)
		}
	}
	if len(toAdd) == 0 {
		return 0, nil
	}

	if err := s.appendEntries(slug, toAdd); err != nil {
		return 0, err
	}

	s.entries[slug] = append(s.entries[slug], toAdd...)
	sort.Slice(s.entries[slug], func(i, j int) bool {
		return s.entries[slug][i].Timestamp > s.entries[slug][j].Timestamp
	})
	s.rebuildFiles(slug)
	return len(toAdd), nil
}

// updateSubscriptionHTTPMeta persists ETag/LastModified after a fetch.
func (s *FeedStore) updateSubscriptionHTTPMeta(slug, etag, lastModified string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sub := range s.subs {
		if sub.Slug == slug {
			sub.ETag = etag
			sub.LastModified = lastModified
			sub.LastFetched = time.Now().UTC().Format(time.RFC3339)
			break
		}
	}
	s.saveSubscriptions()
}

// markRead moves a GUID from unread → read for the given slug.
func (s *FeedStore) markRead(slug, guid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.readSets[slug]
	if rs == nil {
		return fmt.Errorf("no feed %q", slug)
	}
	if rs[guid] {
		return nil // already read
	}
	rs[guid] = true
	if err := s.appendGUID(filepath.Join(s.dataDir, "read", slug), guid); err != nil {
		return err
	}
	s.rebuildFiles(slug)
	return nil
}

// markAllRead marks every entry in a feed as read.
func (s *FeedStore) markAllRead(slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.readSets[slug]
	if rs == nil {
		return fmt.Errorf("no feed %q", slug)
	}
	path := filepath.Join(s.dataDir, "read", slug)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, e := range s.entries[slug] {
		rs[e.GUID] = true
		fmt.Fprintln(f, e.GUID)
	}
	s.rebuildFiles(slug)
	return nil
}

// unmarkRead removes a GUID from the read set (marks the entry unread).
func (s *FeedStore) unmarkRead(slug, guid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.readSets[slug]
	if rs == nil {
		return fmt.Errorf("no feed %q", slug)
	}
	if !rs[guid] {
		return nil // already unread
	}
	delete(rs, guid)
	if err := rewriteGUIDFile(filepath.Join(s.dataDir, "read", slug), rs); err != nil {
		return err
	}
	s.rebuildFiles(slug)
	return nil
}

// pin adds a GUID to the pinned set for a feed.
func (s *FeedStore) pin(slug, guid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.pinnedSets[slug]
	if ps == nil {
		return fmt.Errorf("no feed %q", slug)
	}
	if ps[guid] {
		return nil // already pinned
	}
	ps[guid] = true
	if err := s.appendGUID(filepath.Join(s.dataDir, "pinned", slug), guid); err != nil {
		return err
	}
	s.rebuildFiles(slug)
	return nil
}

// unpin removes a GUID from the pinned set.
func (s *FeedStore) unpin(slug, guid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.pinnedSets[slug]
	if ps == nil {
		return fmt.Errorf("no feed %q", slug)
	}
	if !ps[guid] {
		return nil
	}
	delete(ps, guid)
	if err := rewriteGUIDFile(filepath.Join(s.dataDir, "pinned", slug), ps); err != nil {
		return err
	}
	s.rebuildFiles(slug)
	return nil
}

// ---------------------------------------------------------------------------
// Read-only accessors (caller must hold mu.RLock or mu.Lock)
// ---------------------------------------------------------------------------

func (s *FeedStore) slugExists(slug string) bool {
	for _, sub := range s.subs {
		if sub.Slug == slug {
			return true
		}
	}
	return false
}

func (s *FeedStore) subscriptionBySlug(slug string) *Subscription {
	for _, sub := range s.subs {
		if sub.Slug == slug {
			return sub
		}
	}
	return nil
}

// IndexRow is one entry in the global or per-feed index.
type IndexRow struct {
	slug string
	ef   entryFile
}

// globalIndex returns all entryFiles across all feeds, sorted by timestamp desc.
func (s *FeedStore) globalIndex(unreadOnly bool) []IndexRow {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var rows []IndexRow
	for slug, files := range s.files {
		for _, ef := range files {
			if unreadOnly && ef.read {
				continue
			}
			rows = append(rows, IndexRow{slug: slug, ef: ef})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].ef.entry.Timestamp > rows[j].ef.entry.Timestamp
	})
	return rows
}

// ---------------------------------------------------------------------------
// File list builder
// ---------------------------------------------------------------------------

// rebuildFiles rebuilds the entryFile list for a slug.
// Caller must hold mu.Lock.
func (s *FeedStore) rebuildFiles(slug string) {
	entries := s.entries[slug]
	rs := s.readSets[slug]
	if rs == nil {
		rs = make(map[string]bool)
	}
	ps := s.pinnedSets[slug]
	if ps == nil {
		ps = make(map[string]bool)
	}

	seen := make(map[string]int) // base filename → collision count
	files := make([]entryFile, 0, len(entries))
	for _, e := range entries {
		base := makeEntryFilename(e)
		n := seen[base]
		seen[base]++
		name := base
		if n > 0 {
			name = fmt.Sprintf("%s-%d", base, n+1)
		}
		files = append(files, entryFile{
			filename: name,
			entry:    e,
			read:     rs[e.GUID],
			pinned:   ps[e.GUID],
		})
	}
	s.files[slug] = files
}

// globalPinned returns all pinned entryFiles across all feeds, sorted by
// timestamp descending.
func (s *FeedStore) globalPinned() []IndexRow {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var rows []IndexRow
	for slug, files := range s.files {
		for _, ef := range files {
			if ef.pinned {
				rows = append(rows, IndexRow{slug: slug, ef: ef})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].ef.entry.Timestamp > rows[j].ef.entry.Timestamp
	})
	return rows
}

// ---------------------------------------------------------------------------
// Slug and filename helpers
// ---------------------------------------------------------------------------

var multiDash = regexp.MustCompile(`-{2,}`)

func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case unicode.IsSpace(r) || r == '-' || r == '_':
			b.WriteByte('-')
		}
	}
	return strings.Trim(multiDash.ReplaceAllString(b.String(), "-"), "-")
}

// feedSlugFromTitle derives a unique slug. Caller must hold mu.Lock.
func (s *FeedStore) feedSlugFromTitle(title, userAlias string) string {
	base := userAlias
	if base == "" {
		base = slugify(title)
	}
	if base == "" {
		base = "feed"
	}
	candidate := base
	for i := 2; ; i++ {
		taken := false
		for _, sub := range s.subs {
			if sub.Slug == candidate {
				taken = true
				break
			}
		}
		if !taken {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
}

func makeEntryFilename(e *Entry) string {
	ts := "00000000T000000Z"
	if t, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
		ts = t.UTC().Format("20060102T150405Z")
	}
	titleSlug := slugify(e.Title)
	if titleSlug == "" {
		// Fall back to hash of GUID.
		h := sha256.Sum256([]byte(e.GUID))
		titleSlug = fmt.Sprintf("%x", h[:8])
	}
	// Max 128 bytes total; ts is 17 bytes + 1 dash = 18.
	titleSlug = truncateUTF8(titleSlug, 128-18)
	return ts + "-" + titleSlug
}

// truncateUTF8 truncates s to at most maxBytes bytes without splitting a rune.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	b := []byte(s)
	// Walk back from maxBytes to find a valid rune boundary.
	for maxBytes > 0 && (b[maxBytes]&0xC0) == 0x80 {
		maxBytes--
	}
	return string(b[:maxBytes])
}

// synthGUID synthesises a stable GUID when the feed provides none.
func synthGUID(url, title string) string {
	h := sha256.Sum256([]byte(url + "\x00" + title))
	return fmt.Sprintf("synth:%x", h[:16])
}
