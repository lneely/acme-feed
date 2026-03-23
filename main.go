// Feeds — RSS/Atom feed reader for the acme text editor.
// Embeds a 9P server, mounts it at ~/mnt/feeds via 9pfuse,
// and opens an acme window for browsing and managing feeds.
package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"9fans.net/go/plan9/client"
)

const serviceName = "feeds"

func main() {
	// Locate Plan 9 namespace directory.
	ns := client.Namespace()
	if ns == "" {
		log.Fatal("no Plan 9 namespace (NAMESPACE not set?)")
	}
	sockPath := filepath.Join(ns, serviceName)

	// Load or create the feed store.
	store, err := newFeedStore()
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	// Remove stale socket.
	os.Remove(sockPath)

	// Start 9P server.
	srv, err := newP9Server(sockPath, store)
	if err != nil {
		log.Fatalf("9P listen: %v", err)
	}
	go srv.serve()
	log.Printf("9P server on %s", sockPath)

	// Mount via 9pfuse at ~/mnt/feeds.
	mnt, fuseCmd := mountFeeds(sockPath)
	defer unmount(fuseCmd, mnt, sockPath)

	// Open acme main window.
	if err := openMainWindow(store); err != nil {
		log.Fatalf("acme: %v", err)
	}

	// Wait for window close or signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-mainDone:
	case <-sigCh:
	}
}

// ---------------------------------------------------------------------------
// Mount / unmount
// ---------------------------------------------------------------------------

func mountFeeds(sockPath string) (string, *exec.Cmd) {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("cannot determine home dir: %v", err)
		return "", nil
	}
	mnt := filepath.Join(home, "mnt", "feeds")
	if err := os.MkdirAll(mnt, 0755); err != nil {
		log.Printf("cannot create mount dir %s: %v", mnt, err)
		return mnt, nil
	}
	cmd := exec.Command("9pfuse", sockPath, mnt)
	if err := cmd.Start(); err != nil {
		log.Printf("9pfuse: %v (continuing without mount)", err)
		return mnt, nil
	}
	log.Printf("mounted at %s", mnt)
	return mnt, cmd
}

func unmount(fuseCmd *exec.Cmd, mnt, sockPath string) {
	if fuseCmd != nil {
		exec.Command("fusermount", "-u", mnt).Run()
		fuseCmd.Wait()
	}
	os.Remove(sockPath)
}

// ---------------------------------------------------------------------------
// Refresh helpers (called from acme.go and p9.go)
// ---------------------------------------------------------------------------

// doSubscribe fetches a feed URL, derives a slug, subscribes, and polls entries.
func doSubscribe(store *FeedStore, url, alias string) error {
	result, err := fetchFeed(url, "", "")
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	if result.NotModified || result.FeedTitle == "" {
		result.FeedTitle = url
	}

	store.mu.Lock()
	slug := store.feedSlugFromTitle(result.FeedTitle, alias)
	store.mu.Unlock()

	sub := &Subscription{
		URL:          url,
		Slug:         slug,
		Title:        result.FeedTitle,
		SubscribedAt: time.Now().UTC().Format(time.RFC3339),
		// Do not store ETag/LastModified yet. If parsing yields zero entries
		// the cached validator would cause every subsequent refresh to return
		// 304 and never recover. They are written by doRefresh after a
		// successful fetch that produces at least one new entry.
	}
	if err := store.subscribe(sub); err != nil {
		return err
	}
	log.Printf("fetched %s: %d raw entries", sub.Slug, len(result.Entries))
	// Use sub.Slug, not the local slug variable: subscribe() may have
	// adjusted it for uniqueness.
	n, err := store.addEntries(sub.Slug, result.Entries)
	if err != nil {
		return fmt.Errorf("save entries: %w", err)
	}
	log.Printf("subscribed %s (%s): %d entries added", sub.Slug, url, n)
	return nil
}

// doRefresh re-polls a single feed.
func doRefresh(store *FeedStore, slug string) error {
	store.mu.RLock()
	sub := store.subscriptionBySlug(slug)
	store.mu.RUnlock()
	if sub == nil {
		return fmt.Errorf("no feed %q", slug)
	}

	result, err := fetchFeed(sub.URL, sub.ETag, sub.LastModified)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", slug, err)
	}
	if result.NotModified {
		log.Printf("refresh %s: not modified", slug)
		return nil
	}
	log.Printf("refresh %s: %d raw entries", slug, len(result.Entries))
	n, err := store.addEntries(slug, result.Entries)
	if err != nil {
		return fmt.Errorf("save entries: %w", err)
	}
	// Only advance the cached validators when we actually stored something;
	// if parsing returned zero entries we want the next refresh to re-fetch.
	if n > 0 {
		store.updateSubscriptionHTTPMeta(slug, result.ETag, result.LastModified)
	}
	log.Printf("refresh %s: %d new entries", slug, n)
	return nil
}

// doRefreshAll re-polls every subscribed feed.
func doRefreshAll(store *FeedStore) error {
	store.mu.RLock()
	slugs := make([]string, len(store.subs))
	for i, s := range store.subs {
		slugs[i] = s.Slug
	}
	store.mu.RUnlock()

	var lastErr error
	for _, slug := range slugs {
		if err := doRefresh(store, slug); err != nil {
			log.Printf("refresh %s: %v", slug, err)
			lastErr = err
		}
	}
	return lastErr
}
