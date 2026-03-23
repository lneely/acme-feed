package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"

	"9fans.net/go/plan9"
)

// ---------------------------------------------------------------------------
// Qid constants
// ---------------------------------------------------------------------------

const (
	qidRoot     uint64 = 0
	qidCtl      uint64 = 1
	qidUnread   uint64 = 2
	qidAll      uint64 = 3
	qidFeedsDir uint64 = 4
	qidPinned   uint64 = 5
	// Dynamic qids are computed via pathQid() from the path string.
	// Static ones above are reserved in the low range.
)

// pathQid computes a stable Qid path from an arbitrary string.
// Uses FNV-1a to avoid import of crypto packages.
func pathQid(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	// Keep top bit clear (reserved for QTDIR in some plan9 implementations).
	return (h & 0x7FFFFFFFFFFFFFFF) | 0x1000
}

// ---------------------------------------------------------------------------
// FID state
// ---------------------------------------------------------------------------

type fidState struct {
	qid  plan9.Qid
	path string
	open bool
	// write buffer — accumulated across Twrite calls, dispatched on Tclunk
	writeBuf []byte
}

// ---------------------------------------------------------------------------
// P9Server
// ---------------------------------------------------------------------------

type P9Server struct {
	listener net.Listener
	store    *FeedStore
	sockPath string
}

func newP9Server(sockPath string, store *FeedStore) (*P9Server, error) {
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}
	return &P9Server{listener: ln, store: store, sockPath: sockPath}, nil
}

func (srv *P9Server) serve() {
	for {
		conn, err := srv.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("9P accept: %v", err)
			}
			return
		}
		go srv.handleConn(conn)
	}
}

func (srv *P9Server) close() {
	srv.listener.Close()
}

// ---------------------------------------------------------------------------
// Connection handler
// ---------------------------------------------------------------------------

func (srv *P9Server) handleConn(conn net.Conn) {
	defer conn.Close()

	fids := make(map[uint32]*fidState)
	var mu sync.Mutex

	get := func(fid uint32) *fidState {
		mu.Lock()
		defer mu.Unlock()
		return fids[fid]
	}
	set := func(fid uint32, f *fidState) {
		mu.Lock()
		defer mu.Unlock()
		fids[fid] = f
	}
	del := func(fid uint32) {
		mu.Lock()
		defer mu.Unlock()
		delete(fids, fid)
	}

	for {
		fcall, err := plan9.ReadFcall(conn)
		if err != nil {
			if err != io.EOF {
				log.Printf("9P read: %v", err)
			}
			return
		}

		resp := plan9.Fcall{Tag: fcall.Tag}

		switch fcall.Type {

		case plan9.Tversion:
			resp.Type = plan9.Rversion
			resp.Msize = fcall.Msize
			resp.Version = "9P2000"

		case plan9.Tattach:
			resp.Type = plan9.Rattach
			resp.Qid = plan9.Qid{Type: plan9.QTDIR, Path: qidRoot}
			set(fcall.Fid, &fidState{qid: resp.Qid, path: "/"})

		case plan9.Twalk:
			f := get(fcall.Fid)
			if f == nil {
				resp.Type = plan9.Rerror
				resp.Ename = "bad fid"
				break
			}
			if len(fcall.Wname) == 0 {
				// Clone.
				nf := &fidState{qid: f.qid, path: f.path}
				set(fcall.Newfid, nf)
				resp.Type = plan9.Rwalk
				break
			}
			cur := f.path
			var wqids []plan9.Qid
			for _, name := range fcall.Wname {
				qid, next, err := srv.walk(cur, name)
				if err != nil {
					break
				}
				wqids = append(wqids, qid)
				cur = next
			}
			if len(wqids) == 0 {
				resp.Type = plan9.Rerror
				resp.Ename = "file not found"
				break
			}
			resp.Type = plan9.Rwalk
			resp.Wqid = wqids
			if len(wqids) == len(fcall.Wname) {
				set(fcall.Newfid, &fidState{
					qid:  wqids[len(wqids)-1],
					path: cur,
				})
			}

		case plan9.Topen:
			f := get(fcall.Fid)
			if f == nil {
				resp.Type = plan9.Rerror
				resp.Ename = "bad fid"
				break
			}
			f.open = true
			resp.Type = plan9.Ropen
			resp.Qid = f.qid
			resp.Iounit = 0

		case plan9.Tread:
			f := get(fcall.Fid)
			if f == nil {
				resp.Type = plan9.Rerror
				resp.Ename = "bad fid"
				break
			}
			data, err := srv.read(f, int64(fcall.Offset), fcall.Count)
			if err != nil {
				resp.Type = plan9.Rerror
				resp.Ename = err.Error()
				break
			}
			resp.Type = plan9.Rread
			resp.Data = data

		case plan9.Twrite:
			f := get(fcall.Fid)
			if f == nil {
				resp.Type = plan9.Rerror
				resp.Ename = "bad fid"
				break
			}
			// Buffer writes; dispatch on Tclunk.
			f.writeBuf = append(f.writeBuf, fcall.Data...)
			resp.Type = plan9.Rwrite
			resp.Count = uint32(len(fcall.Data))

		case plan9.Tclunk:
			f := get(fcall.Fid)
			if f != nil && len(f.writeBuf) > 0 {
				if err := srv.dispatch(f.path, f.writeBuf); err != nil {
					// Log but don't fail the clunk.
					log.Printf("dispatch %s: %v", f.path, err)
				}
			}
			del(fcall.Fid)
			resp.Type = plan9.Rclunk

		case plan9.Tstat:
			f := get(fcall.Fid)
			if f == nil {
				resp.Type = plan9.Rerror
				resp.Ename = "bad fid"
				break
			}
			dir, err := srv.stat(f.path)
			if err != nil {
				resp.Type = plan9.Rerror
				resp.Ename = err.Error()
				break
			}
			resp.Type = plan9.Rstat
			resp.Stat, _ = dir.Bytes()

		default:
			resp.Type = plan9.Rerror
			resp.Ename = "not implemented"
		}

		if err := plan9.WriteFcall(conn, &resp); err != nil {
			log.Printf("9P write: %v", err)
			return
		}
	}
}

// ---------------------------------------------------------------------------
// walk
// ---------------------------------------------------------------------------

func (srv *P9Server) walk(cur, name string) (plan9.Qid, string, error) {
	parts := splitPath(cur)
	depth := len(parts)

	switch {
	// Root → top-level names.
	case depth == 0:
		switch name {
		case "ctl":
			return plan9.Qid{Type: plan9.QTFILE, Path: qidCtl}, "/ctl", nil
		case "unread":
			return plan9.Qid{Type: plan9.QTFILE, Path: qidUnread}, "/unread", nil
		case "all":
			return plan9.Qid{Type: plan9.QTFILE, Path: qidAll}, "/all", nil
		case "pinned":
			return plan9.Qid{Type: plan9.QTFILE, Path: qidPinned}, "/pinned", nil
		case "feeds":
			return plan9.Qid{Type: plan9.QTDIR, Path: qidFeedsDir}, "/feeds", nil
		}

	// /feeds → slug directory.
	case depth == 1 && parts[0] == "feeds":
		srv.store.mu.RLock()
		exists := srv.store.slugExists(name)
		srv.store.mu.RUnlock()
		if exists {
			p := "/feeds/" + name
			return plan9.Qid{Type: plan9.QTDIR, Path: pathQid(p)}, p, nil
		}

	// /feeds/{slug} → per-feed files and subdirs.
	case depth == 2 && parts[0] == "feeds":
		slug := parts[1]
		p := cur + "/" + name
		switch name {
		case "meta", "ctl", "unread", "all":
			return plan9.Qid{Type: plan9.QTFILE, Path: pathQid(p)}, p, nil
		case "new", "read":
			return plan9.Qid{Type: plan9.QTDIR, Path: pathQid(p)}, p, nil
		}
		_ = slug

	// /feeds/{slug}/new or /feeds/{slug}/read → entry files.
	case depth == 3 && parts[0] == "feeds":
		slug := parts[1]
		bucket := parts[2] // "new" or "read"
		if bucket != "new" && bucket != "read" {
			break
		}
		srv.store.mu.RLock()
		files := srv.store.files[slug]
		srv.store.mu.RUnlock()
		for _, ef := range files {
			if ef.filename == name {
				wantRead := bucket == "read"
				if ef.read == wantRead {
					p := cur + "/" + name
					return plan9.Qid{Type: plan9.QTFILE, Path: pathQid(p)}, p, nil
				}
			}
		}
	}

	return plan9.Qid{}, "", errors.New("not found")
}

// ---------------------------------------------------------------------------
// read
// ---------------------------------------------------------------------------

func (srv *P9Server) read(f *fidState, offset int64, count uint32) ([]byte, error) {
	if f.qid.Type&plan9.QTDIR != 0 {
		return srv.readDir(f.path, offset, count)
	}
	data, err := srv.readFile(f.path)
	if err != nil {
		return nil, err
	}
	return sliceAt(data, offset, count), nil
}

func (srv *P9Server) readDir(path string, offset int64, count uint32) ([]byte, error) {
	parts := splitPath(path)
	depth := len(parts)

	var entries []plan9.Dir

	switch {
	case depth == 0: // root
		entries = []plan9.Dir{
			fileDir("ctl", 0644, qidCtl),
			fileDir("unread", 0444, qidUnread),
			fileDir("all", 0444, qidAll),
			fileDir("pinned", 0444, qidPinned),
			dirEntry("feeds", qidFeedsDir),
		}

	case depth == 1 && parts[0] == "feeds": // /feeds
		srv.store.mu.RLock()
		for _, sub := range srv.store.subs {
			p := "/feeds/" + sub.Slug
			entries = append(entries, dirEntry(sub.Slug, pathQid(p)))
		}
		srv.store.mu.RUnlock()

	case depth == 2 && parts[0] == "feeds": // /feeds/{slug}
		slug := parts[1]
		base := path
		entries = []plan9.Dir{
			fileDir("meta", 0444, pathQid(base+"/meta")),
			fileDir("ctl", 0644, pathQid(base+"/ctl")),
			fileDir("unread", 0444, pathQid(base+"/unread")),
			fileDir("all", 0444, pathQid(base+"/all")),
			dirEntry("new", pathQid(base+"/new")),
			dirEntry("read", pathQid(base+"/read")),
		}
		_ = slug

	case depth == 3 && parts[0] == "feeds": // /feeds/{slug}/new or /feeds/{slug}/read
		slug := parts[1]
		bucket := parts[2]
		wantRead := bucket == "read"
		srv.store.mu.RLock()
		files := srv.store.files[slug]
		srv.store.mu.RUnlock()
		for _, ef := range files {
			if ef.read == wantRead {
				p := path + "/" + ef.filename
				entries = append(entries, fileDir(ef.filename, 0444, pathQid(p)))
			}
		}
	}

	return serializeDirs(entries, offset, count), nil
}

func (srv *P9Server) readFile(path string) ([]byte, error) {
	parts := splitPath(path)

	switch {
	case path == "/ctl":
		return []byte("sub <url> [slug]\nunsub <slug>\nrefresh [slug]\n"), nil

	case path == "/unread":
		return []byte(srv.globalIndexText(true)), nil

	case path == "/all":
		return []byte(srv.globalIndexText(false)), nil

	case path == "/pinned":
		return []byte(srv.pinnedIndexText()), nil

	case len(parts) == 3 && parts[0] == "feeds" && parts[2] == "meta":
		slug := parts[1]
		srv.store.mu.RLock()
		sub := srv.store.subscriptionBySlug(slug)
		srv.store.mu.RUnlock()
		if sub == nil {
			return nil, fmt.Errorf("no feed %q", slug)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "url: %s\n", sub.URL)
		fmt.Fprintf(&sb, "slug: %s\n", sub.Slug)
		fmt.Fprintf(&sb, "title: %s\n", sub.Title)
		fmt.Fprintf(&sb, "subscribed: %s\n", sub.SubscribedAt)
		fmt.Fprintf(&sb, "last_fetched: %s\n", sub.LastFetched)
		return []byte(sb.String()), nil

	case len(parts) == 3 && parts[0] == "feeds" && parts[2] == "ctl":
		return []byte("refresh\nunsub\nmark-all-read\nread <guid>\n"), nil

	case len(parts) == 3 && parts[0] == "feeds" && parts[2] == "unread":
		return []byte(srv.feedIndexText(parts[1], true)), nil

	case len(parts) == 3 && parts[0] == "feeds" && parts[2] == "all":
		return []byte(srv.feedIndexText(parts[1], false)), nil

	case len(parts) == 4 && parts[0] == "feeds":
		return srv.readEntryFile(parts[1], parts[2], parts[3])
	}

	return nil, fmt.Errorf("not found: %s", path)
}

func (srv *P9Server) readEntryFile(slug, bucket, filename string) ([]byte, error) {
	wantRead := bucket == "read"
	srv.store.mu.RLock()
	files := srv.store.files[slug]
	srv.store.mu.RUnlock()

	for _, ef := range files {
		if ef.filename == filename && ef.read == wantRead {
			e := ef.entry
			var sb strings.Builder
			fmt.Fprintf(&sb, "%s\n", e.Title)
			fmt.Fprintf(&sb, "%s\n", e.URL)

			srv.store.mu.RLock()
			sub := srv.store.subscriptionBySlug(slug)
			srv.store.mu.RUnlock()
			feedTitle := slug
			if sub != nil {
				feedTitle = sub.Title
			}
			fmt.Fprintf(&sb, "%s | %s\n", feedTitle, e.Timestamp)

			if e.Summary != "" {
				fmt.Fprintf(&sb, "\n%s\n", e.Summary)
			}
			return []byte(sb.String()), nil
		}
	}
	return nil, fmt.Errorf("not found")
}

// ---------------------------------------------------------------------------
// dispatch — handle writes to ctl files (called on Tclunk)
// ---------------------------------------------------------------------------

func (srv *P9Server) dispatch(path string, data []byte) error {
	cmd := strings.TrimSpace(string(data))
	parts := splitPath(path)

	switch {
	case path == "/ctl":
		return srv.handleGlobalCtl(cmd)

	case len(parts) == 3 && parts[0] == "feeds" && parts[2] == "ctl":
		return srv.handleFeedCtl(parts[1], cmd)
	}
	return nil
}

func (srv *P9Server) handleGlobalCtl(cmd string) error {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return nil
	}
	switch fields[0] {
	case "sub":
		if len(fields) < 2 {
			return fmt.Errorf("sub: missing URL")
		}
		url := fields[1]
		alias := ""
		if len(fields) >= 3 {
			alias = fields[2]
		}
		return doSubscribe(srv.store, url, alias)

	case "unsub":
		if len(fields) < 2 {
			return fmt.Errorf("unsub: missing slug")
		}
		return srv.store.unsubscribe(fields[1])

	case "refresh":
		if len(fields) >= 2 {
			return doRefresh(srv.store, fields[1])
		}
		return doRefreshAll(srv.store)
	}
	return fmt.Errorf("unknown command: %s", fields[0])
}

func (srv *P9Server) handleFeedCtl(slug, cmd string) error {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return nil
	}
	switch fields[0] {
	case "refresh":
		return doRefresh(srv.store, slug)
	case "unsub":
		return srv.store.unsubscribe(slug)
	case "mark-all-read":
		return srv.store.markAllRead(slug)
	case "read":
		if len(fields) < 2 {
			return fmt.Errorf("read: missing guid")
		}
		return srv.store.markRead(slug, fields[1])
	}
	return fmt.Errorf("unknown command: %s", fields[0])
}

// ---------------------------------------------------------------------------
// stat
// ---------------------------------------------------------------------------

func (srv *P9Server) stat(path string) (plan9.Dir, error) {
	parts := splitPath(path)
	name := "/"
	if len(parts) > 0 {
		name = parts[len(parts)-1]
	}
	d := plan9.Dir{Name: name, Uid: "feeds", Gid: "feeds", Muid: "feeds"}

	switch {
	case path == "/":
		d.Mode = plan9.DMDIR | 0755
		d.Qid = plan9.Qid{Type: plan9.QTDIR, Path: qidRoot}

	case path == "/ctl":
		d.Mode = 0644
		d.Qid = plan9.Qid{Type: plan9.QTFILE, Path: qidCtl}

	case path == "/unread":
		d.Mode = 0444
		d.Qid = plan9.Qid{Type: plan9.QTFILE, Path: qidUnread}

	case path == "/all":
		d.Mode = 0444
		d.Qid = plan9.Qid{Type: plan9.QTFILE, Path: qidAll}

	case path == "/pinned":
		d.Mode = 0444
		d.Qid = plan9.Qid{Type: plan9.QTFILE, Path: qidPinned}

	case path == "/feeds":
		d.Mode = plan9.DMDIR | 0755
		d.Qid = plan9.Qid{Type: plan9.QTDIR, Path: qidFeedsDir}

	case len(parts) == 2 && parts[0] == "feeds":
		slug := parts[1]
		srv.store.mu.RLock()
		exists := srv.store.slugExists(slug)
		srv.store.mu.RUnlock()
		if !exists {
			return plan9.Dir{}, fmt.Errorf("not found")
		}
		d.Mode = plan9.DMDIR | 0755
		d.Qid = plan9.Qid{Type: plan9.QTDIR, Path: pathQid(path)}

	case len(parts) == 3 && parts[0] == "feeds":
		switch parts[2] {
		case "new", "read":
			d.Mode = plan9.DMDIR | 0755
			d.Qid = plan9.Qid{Type: plan9.QTDIR, Path: pathQid(path)}
		case "meta", "unread", "all":
			d.Mode = 0444
			d.Qid = plan9.Qid{Type: plan9.QTFILE, Path: pathQid(path)}
		case "ctl":
			d.Mode = 0644
			d.Qid = plan9.Qid{Type: plan9.QTFILE, Path: pathQid(path)}
		default:
			return plan9.Dir{}, fmt.Errorf("not found")
		}

	case len(parts) == 4 && parts[0] == "feeds":
		d.Mode = 0444
		d.Qid = plan9.Qid{Type: plan9.QTFILE, Path: pathQid(path)}

	default:
		return plan9.Dir{}, fmt.Errorf("not found: %s", path)
	}

	return d, nil
}

// ---------------------------------------------------------------------------
// Index text helpers
// ---------------------------------------------------------------------------

func (srv *P9Server) globalIndexText(unreadOnly bool) string {
	rows := srv.store.globalIndex(unreadOnly)
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
	return sb.String()
}

func (srv *P9Server) pinnedIndexText() string {
	rows := srv.store.globalPinned()
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
	return sb.String()
}

func (srv *P9Server) feedIndexText(slug string, unreadOnly bool) string {
	srv.store.mu.RLock()
	files := srv.store.files[slug]
	srv.store.mu.RUnlock()

	var sb strings.Builder
	n := 0
	for _, ef := range files {
		if unreadOnly && ef.read {
			continue
		}
		n++
		fmt.Fprintf(&sb, "%4d  %-20s  %s\n", n, formatTS(ef.entry.Timestamp), ef.entry.Title)
	}
	return sb.String()
}

func formatTS(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// ---------------------------------------------------------------------------
// Utility
// ---------------------------------------------------------------------------

func splitPath(path string) []string {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func sliceAt(data []byte, offset int64, count uint32) []byte {
	if offset >= int64(len(data)) {
		return []byte{}
	}
	end := offset + int64(count)
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return data[offset:end]
}

func serializeDirs(entries []plan9.Dir, offset int64, count uint32) []byte {
	var buf []byte
	for _, d := range entries {
		b, _ := d.Bytes()
		buf = append(buf, b...)
	}
	return sliceAt(buf, offset, count)
}

func fileDir(name string, mode plan9.Perm, qidPath uint64) plan9.Dir {
	return plan9.Dir{
		Name: name,
		Mode: mode,
		Qid:  plan9.Qid{Type: plan9.QTFILE, Path: qidPath},
	}
}

func dirEntry(name string, qidPath uint64) plan9.Dir {
	return plan9.Dir{
		Name: name,
		Mode: plan9.DMDIR | 0755,
		Qid:  plan9.Qid{Type: plan9.QTDIR, Path: qidPath},
	}
}
