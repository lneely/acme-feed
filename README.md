# acme-feed

RSS/Atom feed reader for the [acme](https://en.wikipedia.org/wiki/Acme_(text_editor)) text editor. Embeds a 9P file server and mounts it at `~/mnt/feeds` via `9pfuse`.

## Installation

```sh
mk install
```

## Usage

```sh
Feeds
```

Middle-click `Feeds` in Acme to launch, or run from a shell. The `/+Feeds` window opens automatically.

## Main Window (`/+Feeds`)

| Tag command | Action |
|---|---|
| `Get` | Refresh the window from disk |
| `Put` | Apply edits (read/unread/pin/unpin marks) |
| `Refresh` | Re-fetch all subscribed feeds |
| `Sub <url>` | Subscribe to a feed |
| `Unsub <slug>` | Unsubscribe from a feed (opens picker if no arg) |
| `Pins` | Open the pinned entries window |

Each line shows an entry index, timestamp, feed slug, and title. Middle-click a line number to open the entry.

**Bulk editing with `Put`:** prefix lines in the body and click `Put`:

| Prefix | Action |
|---|---|
| `r N` | Mark entry N read |
| `u N` | Mark entry N unread |
| `+ N` | Pin entry N |
| `- N` | Unpin entry N |

## Entry Windows (`/+Feeds/{slug}/{entry}`)

Tag commands: `Read`, `Unread`, `Pin`, `Unpin`.

Right-clicking a URL in an entry window fetches the page as plain Markdown rather than plumbing it to a browser. A `Get` tag in the content window re-fetches.

## Pins Window (`/+Pins`)

Shows all pinned entries. Middle-click a line to open the entry. Use `Put` with `- N` prefixes to unpin entries.

## 9P File Server (`~/mnt/feeds`)

The server is also accessible as a filesystem:

```
~/mnt/feeds/
  ctl                  — write commands: sub, unsub, refresh [slug]
  unread               — index of all unread entries
  all                  — index of all entries
  pinned               — index of pinned entries
  feeds/{slug}/
    meta               — feed title and URL
    ctl                — per-feed commands: refresh, unsub, mark-all-read, read <guid>
    unread/            — unread entry files
    all/               — all entry files
```

## Storage

Data is stored in `~/.local/share/acme-feed/`:

```
subscriptions.jsonl    — feed list
feeds/{slug}.jsonl     — entry cache
read/{slug}            — read GUIDs
pinned/{slug}          — pinned GUIDs
```

## Dependencies

- Acme editor (Plan 9 from User Space)
- `9pfuse` (for filesystem mount; optional)

## License

GPLv3 — see [LICENSE](LICENSE).
