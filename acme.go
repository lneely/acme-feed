package main

import (
	"fmt"
	"strings"

	"9fans.net/go/acme"
)

// ---------------------------------------------------------------------------
// Main window
// ---------------------------------------------------------------------------

var mainDone = make(chan struct{})

// openMainWindow opens the /+Feeds window and starts its event loop.
func openMainWindow(store *FeedStore) error {
	w, err := acme.New()
	if err != nil {
		return err
	}
	w.Name("/+Feeds")
	w.Write("tag", []byte("Get Put Pins Refresh Unsub Sub "))
	refreshMainWindow(w, store)
	go handleMainWindow(w, store)
	return nil
}

func handleMainWindow(w *acme.Win, store *FeedStore) {
	defer w.CloseFiles()
	defer close(mainDone)

	// snapshot of entries for middle-click resolution
	var snapshot []mainRow
	snapshot = buildSnapshot(store)

	for e := range w.EventChan() {
		switch e.C2 {
		case 'x', 'X':
			cmd := strings.TrimSpace(string(e.Text))
			arg := strings.TrimSpace(string(e.Arg))
			// Accept "Cmd argument" typed inline in the tag bar as an
			// alternative to chording, since chord e.Arg is unreliable
			// across acme builds and mouse configurations.
			if fields := strings.Fields(cmd); len(fields) > 1 {
				cmd = fields[0]
				if arg == "" {
					arg = strings.Join(fields[1:], " ")
				}
			}
			switch cmd {
			case "Get":
				snapshot = buildSnapshot(store)
				refreshMainWindow(w, store)

			case "Put":
				body, _ := w.ReadAll("body")
				applyReadMarks(w, store, &snapshot, string(body))

			case "Pins":
				openPinsWindow(store)

			case "Refresh":
				if err := doRefreshAll(store); err != nil {
					w.Err(err.Error())
				}
				snapshot = buildSnapshot(store)
				refreshMainWindow(w, store)

			case "Sub":
				if arg == "" {
					w.Err("Sub: chord a URL first")
					continue
				}
				if err := doSubscribe(store, arg, ""); err != nil {
					w.Err(fmt.Sprintf("Sub: %v", err))
					continue
				}
				snapshot = buildSnapshot(store)
				refreshMainWindow(w, store)

			case "Unsub":
				if arg != "" {
					if err := store.unsubscribe(arg); err != nil {
						w.Err(err.Error())
						continue
					}
					snapshot = buildSnapshot(store)
					refreshMainWindow(w, store)
				} else {
					openUnsubWindow(store, func() {
						snapshot = buildSnapshot(store)
						refreshMainWindow(w, store)
					})
				}

			default:
				w.WriteEvent(e)
			}

		case 'l', 'L':
			text := strings.TrimSpace(string(e.Text))
			// Middle-click on a line number opens the entry.
			if idx := parseLineIndex(text); idx >= 1 && idx <= len(snapshot) {
				row := snapshot[idx-1]
				openEntryWindow(store, row.slug, row.ef)
				store.markRead(row.slug, row.ef.entry.GUID)
				// Annotate the line in place rather than refreshing the
				// whole window, so the user doesn't lose their place.
				line := fmt.Sprintf("%4d  %-20s  %-20s  %s [read]\n",
					idx,
					formatTS(row.ef.entry.Timestamp),
					row.slug,
					row.ef.entry.Title,
				)
				w.Addr("%d", idx)
				w.Write("data", []byte(line))
				w.Ctl("clean")
			} else {
				w.WriteEvent(e)
			}

		default:
			w.WriteEvent(e)
		}
	}
}

func refreshMainWindow(w *acme.Win, store *FeedStore) {
	rows := store.globalIndex(true) // unread only

	var sb strings.Builder
	for i, row := range rows {
		e := row.ef.entry
		fmt.Fprintf(&sb, "%4d  %-20s  %-20s  %s\n",
			i+1,
			formatTS(e.Timestamp),
			row.slug,
			e.Title,
		)
	}
	if len(rows) == 0 {
		sb.WriteString("(no unread entries)\n")
	}

	w.Addr(",")
	w.Write("data", []byte(sb.String()))
	w.Ctl("clean")
	w.Addr("0")
	w.Ctl("dot=addr")
	w.Ctl("show")
}

// mainRow maps display index → entry.
type mainRow struct {
	slug string
	ef   entryFile
}

func buildSnapshot(store *FeedStore) []mainRow {
	rows := store.globalIndex(true)
	snap := make([]mainRow, len(rows))
	for i, r := range rows {
		snap[i] = mainRow{slug: r.slug, ef: r.ef}
	}
	return snap
}

// ---------------------------------------------------------------------------
// Pins window
// ---------------------------------------------------------------------------

func openPinsWindow(store *FeedStore) {
	w, err := acme.New()
	if err != nil {
		return
	}
	w.Name("/+Pins")
	w.Write("tag", []byte("Get Put "))
	var snapshot []mainRow
	snapshot = buildPinsSnapshot(store)
	refreshPinsWindow(w, store, snapshot)

	go func() {
		defer w.CloseFiles()
		for e := range w.EventChan() {
			switch e.C2 {
			case 'x', 'X':
				cmd := strings.TrimSpace(string(e.Text))
				switch cmd {
				case "Get":
					snapshot = buildPinsSnapshot(store)
					refreshPinsWindow(w, store, snapshot)
				case "Put":
					body, _ := w.ReadAll("body")
					applyPinsEdits(store, &snapshot, string(body))
					refreshPinsWindow(w, store, snapshot)
				default:
					w.WriteEvent(e)
				}
			case 'l', 'L':
				text := strings.TrimSpace(string(e.Text))
				if idx := parseLineIndex(text); idx >= 1 && idx <= len(snapshot) {
					row := snapshot[idx-1]
					openEntryWindow(store, row.slug, row.ef)
				} else {
					w.WriteEvent(e)
				}
			default:
				w.WriteEvent(e)
			}
		}
	}()
}

func buildPinsSnapshot(store *FeedStore) []mainRow {
	rows := store.globalPinned()
	snap := make([]mainRow, len(rows))
	for i, r := range rows {
		snap[i] = mainRow{slug: r.slug, ef: r.ef}
	}
	return snap
}

func refreshPinsWindow(w *acme.Win, store *FeedStore, snapshot []mainRow) {
	var sb strings.Builder
	for i, row := range snapshot {
		e := row.ef.entry
		fmt.Fprintf(&sb, "%4d  %-20s  %-20s  %s\n",
			i+1,
			formatTS(e.Timestamp),
			row.slug,
			e.Title,
		)
	}
	if len(snapshot) == 0 {
		sb.WriteString("(no pinned entries)\n")
	}
	w.Addr(",")
	w.Write("data", []byte(sb.String()))
	w.Ctl("clean")
	w.Addr("0")
	w.Ctl("dot=addr")
	w.Ctl("show")
}

// ---------------------------------------------------------------------------
// Unsub window
// ---------------------------------------------------------------------------

func openUnsubWindow(store *FeedStore, onChanged func()) {
	w, err := acme.New()
	if err != nil {
		return
	}
	w.Name("/+Unsub")
	w.Write("tag", []byte("Get "))
	refreshUnsubWindow(w, store)

	go func() {
		defer w.CloseFiles()
		var lines []string
		lines = buildUnsubLines(store)

		for e := range w.EventChan() {
			switch e.C2 {
			case 'x', 'X':
				if strings.TrimSpace(string(e.Text)) == "Get" {
					lines = buildUnsubLines(store)
					refreshUnsubWindow(w, store)
				} else {
					w.WriteEvent(e)
				}

			case 'l', 'L':
				text := strings.TrimSpace(string(e.Text))
				fields := strings.Fields(text)
				if len(fields) == 0 {
					w.WriteEvent(e)
					continue
				}
				// Middle-click: text is the slug (first field of the line).
				slug := fields[0]
				// Verify it's a known slug.
				found := false
				for _, l := range lines {
					if strings.Fields(l)[0] == slug {
						found = true
						break
					}
				}
				if !found {
					w.WriteEvent(e)
					continue
				}
				if err := store.unsubscribe(slug); err != nil {
					w.Err(err.Error())
					continue
				}
				lines = buildUnsubLines(store)
				refreshUnsubWindow(w, store)
				if onChanged != nil {
					onChanged()
				}

			default:
				w.WriteEvent(e)
			}
		}
	}()
}

func buildUnsubLines(store *FeedStore) []string {
	store.mu.RLock()
	defer store.mu.RUnlock()
	lines := make([]string, len(store.subs))
	for i, sub := range store.subs {
		lines[i] = fmt.Sprintf("%-30s  %s", sub.Slug, sub.URL)
	}
	return lines
}

func refreshUnsubWindow(w *acme.Win, store *FeedStore) {
	lines := buildUnsubLines(store)
	var sb strings.Builder
	if len(lines) == 0 {
		sb.WriteString("(no subscriptions)\n")
	} else {
		for _, l := range lines {
			sb.WriteString(l + "\n")
		}
	}
	w.Addr(",")
	w.Write("data", []byte(sb.String()))
	w.Ctl("clean")
}

// ---------------------------------------------------------------------------
// Entry window
// ---------------------------------------------------------------------------

func openEntryWindow(store *FeedStore, slug string, ef entryFile) {
	w, err := acme.New()
	if err != nil {
		return
	}
	e := ef.entry
	w.Name("/+Feeds/%s/%s", slug, ef.filename)
	w.Write("tag", []byte("Read Unread Pin "))

	store.mu.RLock()
	sub := store.subscriptionBySlug(slug)
	store.mu.RUnlock()
	feedTitle := slug
	if sub != nil {
		feedTitle = sub.Title
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n", e.Title)
	fmt.Fprintf(&sb, "%s\n", e.URL)
	fmt.Fprintf(&sb, "%s | %s\n", feedTitle, e.Timestamp)
	if e.Summary != "" {
		fmt.Fprintf(&sb, "\n%s\n", e.Summary)
	}

	w.Write("body", []byte(sb.String()))
	w.Ctl("clean")
	w.Addr("0")
	w.Ctl("dot=addr")
	w.Ctl("show")

	entrySlug := slug
	entryFilename := ef.filename

	go func() {
		defer w.CloseFiles()
		for ev := range w.EventChan() {
			switch ev.C2 {
			case 'x', 'X':
				switch strings.TrimSpace(string(ev.Text)) {
				case "Read":
					store.markRead(entrySlug, ef.entry.GUID)
				case "Unread":
					store.unmarkRead(entrySlug, ef.entry.GUID)
				case "Pin":
					store.pin(entrySlug, ef.entry.GUID)
				default:
					w.WriteEvent(ev)
				}
			case 'l', 'L':
				// Intercept right-click on URLs: fetch as Markdown instead
				// of plumbing to the browser.
				text := strings.TrimSpace(string(ev.Text))
				if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
					go openContentWindow(text, entrySlug, entryFilename)
					continue // consumed — do not plumb
				}
				w.WriteEvent(ev)
			default:
				w.WriteEvent(ev)
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// Content window
// ---------------------------------------------------------------------------

func openContentWindow(url, slug, entryFilename string) {
	w, err := acme.New()
	if err != nil {
		return
	}
	w.Name("/+Feeds/%s/%s/content", slug, entryFilename)
	w.Write("tag", []byte("Get "))
	w.Write("body", []byte("fetching…\n"))
	w.Ctl("clean")

	md, err := fetchMarkdown(url)
	if err != nil {
		w.Addr(",")
		w.Write("data", []byte(fmt.Sprintf("error: %v\n", err)))
		w.Ctl("clean")
	} else {
		w.Addr(",")
		w.Write("data", []byte(md))
		w.Ctl("clean")
		w.Addr("0")
		w.Ctl("dot=addr")
		w.Ctl("show")
	}

	go func() {
		defer w.CloseFiles()
		for ev := range w.EventChan() {
			switch {
			case (ev.C2 == 'x' || ev.C2 == 'X') && strings.TrimSpace(string(ev.Text)) == "Get":
				md2, err2 := fetchMarkdown(url)
				w.Addr(",")
				if err2 != nil {
					w.Write("data", []byte(fmt.Sprintf("error: %v\n", err2)))
				} else {
					w.Write("data", []byte(md2))
					w.Addr("0")
					w.Ctl("dot=addr")
					w.Ctl("show")
				}
				w.Ctl("clean")
			default:
				w.WriteEvent(ev)
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// applyPinsEdits processes the Pins window body after Put.
// Lines prefixed with "- N" unpin entry N.
func applyPinsEdits(store *FeedStore, snapshot *[]mainRow, body string) {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		fields := strings.Fields(trimmed[2:])
		if len(fields) == 0 {
			continue
		}
		idx := parseLineIndex(fields[0])
		if idx < 1 || idx > len(*snapshot) {
			continue
		}
		row := (*snapshot)[idx-1]
		store.unpin(row.slug, row.ef.entry.GUID)
	}
	*snapshot = buildPinsSnapshot(store)
}

func parseLineIndex(s string) int {
	s = strings.TrimSpace(s)
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

// applyEdits processes the window body after Put.
// Recognised line prefixes:
//
//	r N  — mark entry N read
//	u N  — mark entry N unread
//	+ N  — pin entry N
//	- N  — unpin entry N
func applyReadMarks(w *acme.Win, store *FeedStore, snapshot *[]mainRow, body string) {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) < 2 || trimmed[1] != ' ' {
			continue
		}
		verb := trimmed[0]
		if verb != 'r' && verb != 'u' && verb != '+' && verb != '-' {
			continue
		}
		fields := strings.Fields(trimmed[2:])
		if len(fields) == 0 {
			continue
		}
		idx := parseLineIndex(fields[0])
		if idx < 1 || idx > len(*snapshot) {
			continue
		}
		row := (*snapshot)[idx-1]
		switch verb {
		case 'r':
			store.markRead(row.slug, row.ef.entry.GUID)
		case 'u':
			store.unmarkRead(row.slug, row.ef.entry.GUID)
		case '+':
			store.pin(row.slug, row.ef.entry.GUID)
		case '-':
			store.unpin(row.slug, row.ef.entry.GUID)
		}
	}
	*snapshot = buildSnapshot(store)
	refreshMainWindow(w, store)
}
