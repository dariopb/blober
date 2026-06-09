# VFS providers

This document specifies the virtual filesystem (VFS) abstraction used by the
`blober` TUI to make the dual-pane file manager work uniformly across multiple
storage backends, and the `p` shortcut that lets the user switch the provider of
the active pane at runtime.

## Goals

- Decouple panel/model logic from any single storage backend so that listing,
  reading, writing, stat, mkdir and delete all flow through one interface.
- Support multiple backends that are interchangeable per pane:
  - Local filesystem.
  - Azure Blob Storage (virtual prefixes).
  - SCP over SSH (sftp).
  - Plain HTTP (HTML index listing + PUT uploads).
  - WebDAV (PROPFIND/MKCOL/PUT/DELETE).
- Allow the user to change the active pane's provider at runtime via `p`.

A panel holds **only** a `Provider` pointer (plus presentation state); it has no
backend-specific `kind` field. The provider value itself fully determines how a
pane talks to storage.

## The `Provider` interface

Each panel holds a reference to a `Provider` (`cmd/blober/tui/vfs.go`) and
performs **all** of its storage operations through it. The interface is:

```go
type Provider interface {
    Label() string
    List(ctx, location) ([]Entry, error)
    Parent(location) (string, bool)
    Join(location, relSlash) (string, error)
    Mkdir(ctx, location, name) (child string, virtual bool, err error)
    Virtual() bool
    Remove(ctx, entry Entry) error
    Exists(ctx, path) (bool, error)
    Open(ctx, path) (io.ReadCloser, int64, error)
    Create(ctx, path, size, r) error
    Walk(ctx, entry Entry) ([]WalkItem, error)
    Close() error
}
```

A provider may optionally implement `headerDescriber` (`Header(location) string`)
to render a custom panel header; Azure uses it to show account/container/prefix.
Providers that don't implement it fall back to a generic `Label: location`
header.

Semantics:

- `Label` is a short human-readable backend identity used in panel headers.
- `List` returns the entries at `location`, including a `..` parent entry where
  applicable. For Azure, an empty container yields no entries.
- `Parent` returns the parent location and whether one exists (the local root
  and the Azure container root have no parent).
- `Join` converts a **slash-separated, destination-relative** path into a
  provider-native absolute path under `location`. It rejects unsafe relative
  paths (see *Path safety*).
- `Mkdir` creates directory `name` under `location` and returns the new child
  location. For backends where directories are implicit (Azure), it returns
  `virtual = true` and creates nothing physical.
- `Virtual` reports whether directories are implicit. `createDirectory` uses
  this to navigate inline for virtual backends (no placeholder object) versus
  issuing an asynchronous `Mkdir` for real ones.
- `Remove` deletes an entry, recursing into directories.
- `Exists` reports whether a file exists at a provider-native path; used for the
  pre-copy overwrite check.
- `Open` opens a file for reading and reports its size (`-1` if unknown).
- `Create` writes a reader to a provider-native path, creating any parent
  directories. Progress is observed by wrapping the reader, so the same code
  path reports upload and download progress uniformly.
- `Walk` expands a directory entry into a flat list of files. Each `WalkItem`
  carries a `Path`, a slash-separated destination-relative `Rel` (including the
  copied directory's base name), `Size`, and `ModTime`.
- `Close` releases any resources held by the provider (e.g. an ssh/sftp
  connection). Stateless backends (local, Azure, HTTP, WebDAV) return `nil`; it
  is idempotent. The model calls it when a pane switches provider and when a
  stale connection result is discarded.

### Path safety

`safeRel` rejects relative paths that could escape the destination root: empty
paths, absolute paths, and any path containing an empty, `.`, or `..` segment.
`Join` calls it before composing a native path, so a malicious or malformed
listing cannot cause writes outside the target location.

## Providers

### `localProvider`

Backs the local filesystem. `Create` is atomic: it writes to a temporary file in
the destination directory and renames it into place, removing the temp file on
any failure or cancellation so no partial files are left behind. `Walk` uses
`filepath.WalkDir` and emits only regular files (empty directories are not
copied).

### `azureProvider`

Backs Azure Blob Storage. It is constructed from the model's blob client and the
currently selected container. Directories are virtual prefixes ending in `/`, so
`Virtual()` is true and `Mkdir` only computes the new prefix. Streaming is done
via `OpenBlobStream`/`UploadBlobStream` in `pkg/azure_storage`. The Azure
account/container remains global model state (a documented limitation): both
panes share one account, and the `l` container picker affects that global state.

### `scpProvider`

Backs a remote machine over SSH/sftp (`cmd/blober/tui/scp.go`). A live
`scpSession` owns the `*ssh.Client` and `*sftp.Client`; the `scpProvider` holds a
pointer to it so bubbletea's value copies of the model share one connection.
`Close` tears the session down once (guarded by `sync.Once`). `Remove` recurses
over sftp; `Walk` recurses over sftp directories.

Transfers are pipelined so throughput matches the native `scp`/`sftp` clients
rather than being latency-bound. Uploads call `sftp.File.ReadFromWithConcurrency`
(many in-flight writes instead of one round-trip per 32KB packet); downloads rely
on `sftp.File.WriteTo`, which the shared `progressReader.WriteTo` fast path
delegates to (see below). The 32KB packet size is the SFTP-protocol maximum that
all servers are guaranteed to accept; the speed-up comes from issuing up to 64
concurrent packets (~2MB window), not from larger packets.

The generic copy plumbing wraps every source reader in a `progressReader` for the
progress bar. To avoid defeating the providers' concurrent transfer paths,
`progressReader` implements `io.WriterTo`: `io.Copy` consults `src.WriteTo` before
`dst.ReadFrom`, so when the wrapped reader is an `*sftp.File` the concurrent
`WriteTo` is used (bytes counted via a `progressWriter`); otherwise it falls back
to a plain buffered copy that still counts via `Read`.

### `httpProvider`

Backs a plain HTTP server that serves browsable directory index pages
(`cmd/blober/tui/web.go`). `Location` and `Entry.Path` are full URL strings;
directories carry a trailing slash.

- `List` issues a `GET`, parses the returned HTML with `golang.org/x/net/html`,
  and resolves each `<a href>` against the **post-redirect** request URL. Only
  direct children are kept: links are skipped if they are fragments (`#…`),
  queries (`?…`), cross-origin, ascend above the current directory, or point more
  than one path segment deep. A trailing slash marks a directory. File size and
  modification time are recovered from the autoindex text following each link
  (e.g. nginx's `… 03-Jun-2026 00:30   21595`); a `-` size marks a directory.
- `Create` uploads with `PUT`, mirroring `curl --data-binary`: it always sends an
  explicit `Content-Length` (buffering the body when the source size is unknown
  so the request is never chunked, which DAV servers reject) and a
  `Content-Type`. Nested destinations are uploaded to their full relative URL and
  rely on the server to create parent directories (e.g. nginx
  `create_full_put_path`). `Exists` uses `HEAD`. `Remove` uses `DELETE`.
- `Mkdir` is **not supported** over plain HTTP (there is no portable directory
  creation verb); it returns an error. Directory *copies* still work because the
  copy path uploads nested files directly rather than pre-creating directories.

### `webdavProvider`

Backs a standard WebDAV server (`cmd/blober/tui/web.go`), also URL-addressed.

- `List` issues a `PROPFIND` with `Depth: 1` and parses the `DAV:` multistatus
  XML. It skips the collection's own self entry and anything deeper than a direct
  child; a `<resourcetype><collection/>` marks a directory, and
  `getcontentlength`/`getlastmodified` populate size and mod time.
- `Mkdir` issues `MKCOL` (tolerating `405`/`301`, which usually mean the
  collection already exists). `Create` first `MKCOL`s any missing ancestor
  collections (`ensureParents`) and then `PUT`s the file. `Exists` uses
  `PROPFIND` with `Depth: 0`. `Remove` uses `DELETE`.

#### Redirects and Basic auth

Both web providers share an `http.Client` (`newWebClient`) that follows up to ten
redirects. Optional Basic-auth credentials from the modal are sent on the initial
request and **re-applied across redirects only when the target is the same host
and not an `https`→`http` downgrade**; otherwise the `Authorization` header is
stripped so credentials never leak to a redirect target. Streaming `PUT` bodies
cannot be replayed on a redirect, so Go's client surfaces an error rather than
silently succeeding.

## Generic copy

Copy is backend-agnostic. For each item, `copyCmd`/`copyOne`:

1. `Join` the destination location with the slash-relative target path.
2. `Open` the source reader from the source provider.
3. Wrap it in a counting reader that reports bytes read (acting as upload or
   download progress uniformly) and checks context cancellation between reads.
4. `Create` it on the destination provider.

The final per-file completion is reported only after `Create` returns
successfully. On cancellation, the destination's `Remove` is invoked
best-effort to clean up a partially written target. Empty directories are never
copied because `Walk` yields only files.

## SCP connection and host-key verification

`connectSCP` dials SSH and opens an sftp session, then resolves the remote root
via the working directory. Authentication is selected as follows:

- An explicit **Key file** (parsed by `loadSigner`) and/or a **Password** are
  used when provided.
- If **both are empty**, `defaultSSHAuths` falls back to the user's default SSH
  credentials: the running **ssh-agent** (via `SSH_AUTH_SOCK`) and the standard
  private keys under `~/.ssh` (`id_ed25519`, `id_ecdsa`, `id_rsa`, `id_dsa`).
  Passphrase-protected or unparseable keys are skipped. The agent connection is
  closed once the handshake completes.

Host keys are verified against `~/.ssh/known_hosts`:

- Known host with a matching key: accepted.
- Unknown host: trust-on-first-use (TOFU) — the key is appended to
  `known_hosts` (the file/dir are created if missing).
- Known host with a **changed** key: rejected (possible man-in-the-middle).

The connection never uses `InsecureIgnoreHostKey`.

## The `p` provider modal

`p` opens a centered modal for the active pane:

- A Type row cycles Local / Azure / SCP / HTTP / WebDAV with the left/right
  arrows.
- SCP reveals Host, Port (default `22`), User, Password (masked), and Key file
  fields. A host and user are required; leaving both Password and Key file empty
  uses the default `~/.ssh`/agent credentials described above.
- Azure reveals Account, Subscription and Tenant fields, pre-filled from the
  command-line defaults (`--account`/`--subscription`/`--tenant`) when present.
  Account and Subscription are required. Applying either reuses the existing
  authenticated client (when the account and tenant are unchanged) or starts an
  in-TUI sign-in (see "Azure sign-in" below).
- HTTP and WebDAV reveal URL, User and Password (masked) fields. The URL is
  required; a bare `host[:port]/path` is normalized to `http://…` with a trailing
  slash. User/Password, when given, are sent as HTTP Basic auth. The connection
  is validated with an initial directory listing before the pane switches.
- Pressing **Enter on the Key file field** opens a private-key file browser
  (`key_browser.go`) that reuses `listLocal`. It starts in the directory of the
  current key path, otherwise `~/.ssh`, otherwise the home/working directory.
  Up/Down (or `j`/`k`), PgUp/PgDn and Home/End move the cursor; Left/`h`/
  Backspace go to the parent directory; Right/`l`/Enter descend into a directory
  or select a file. Only regular files may be selected (special files are
  rejected). Selecting a key fills the Key file field and returns focus to it;
  Esc cancels back to the modal.
- The active text field shows a block cursor. Left/Right move the cursor within
  the field (the value scrolls horizontally to keep the cursor visible),
  Home/End jump to the bounds, typing inserts at the cursor, Backspace deletes
  the rune before the cursor, and Delete removes the rune at the cursor. Moving
  to a different field places the cursor at the end of that field's value.
- Up/Down or Tab move between fields and the Connect/Cancel buttons.
- Local applies immediately. Azure either reuses the current authenticated
  client or starts an in-TUI sign-in; once connected it opens the container
  picker when no container is selected. SCP, HTTP and WebDAV connect
  asynchronously, showing a connecting state that `Esc` cancels.
- When a connection or Azure sign-in fails, the full error is shown in a
  dedicated, word-wrapped error modal (the status bar would truncate it).
  Dismissing it with `Esc`/`Enter` returns to the provider modal with the
  entered values intact so the connection can be adjusted and retried.

### Azure sign-in

Azure is optional: when the TUI starts with no Azure client both panes open on
the local filesystem and no authentication happens until the user picks Azure in
the `p` modal. All OAuth interaction then happens **inside** the TUI rather than
on stderr (which would corrupt the alternate screen):

- A cancelable context and a buffered message channel are created, and an
  `authGen` counter is bumped so stale prompts/results from a superseded or
  cancelled sign-in are ignored.
- `azure_storage.Login` runs in a `tea.Cmd` goroutine. Its `Config.Prompt`
  callback pushes the device-code or browser instructions (message, user code,
  verification URL and an optional QR code rendered to text) into the channel,
  which the model shows in an "Azure sign-in" modal.
- `Esc` cancels the context (aborting the login and any credential refresh
  loop). On success the model takes ownership of the credential's cancel func,
  cancelling the previous one when it replaces the client.

### Generation tokens

Switching a pane's provider bumps a per-pane generation counter. Asynchronous
list results and connection results carry the generation they were issued under;
the model drops any result whose generation no longer matches the pane, so a
stale in-flight listing or connection cannot overwrite a pane that has since
changed provider. A discarded provider is `Close`d.

## Known limitations

- The Azure account/container is global model state shared by both panes; the
  `l` container picker affects that global state.
- Provider switching is gen-guarded for listing and SCP-connection results. Other
  in-flight async operations (overwrite checks, mkdir, delete) are not
  gen-guarded; switching a pane's provider while one of those is running is not
  prevented. In practice these operations are short and provider switching is a
  deliberate user action.
- Empty directories are not copied: directory expansion yields only files, so an
  empty source directory (local or remote) produces no destination directory.
- Plain HTTP cannot create directories (no portable verb); directory copies
  rely on the server creating parent paths on `PUT`. File sizes are recovered
  from common autoindex listings (e.g. nginx); servers using a different index
  format may report unknown (zero) sizes, in which case transfer progress is
  byte-counted without a known total.

## Testing

- `safeRel` rejects traversal/absolute/empty paths and accepts normal ones.
- `resolveProvider` returns the panel's explicit provider and falls back to a
  local provider when a panel has none.
- `localProvider` `Join`/`Parent`/`Walk`/`Open`/`Create` round-trip, including a
  local-to-local copy through `copyCmd`, and no temp files are left behind.
- `p` opens the modal, selecting Local switches the pane, and the SCP form
  validates required fields and supports field editing.
- `connectWeb` normalizes URLs and rejects unsupported schemes; `httpProvider`
  and `webdavProvider` listing is exercised against an `httptest` server,
  covering HTML index parsing (direct-child filtering of parent/query/fragment/
  off-site/nested links, plus autoindex size/date recovery), PROPFIND
  multistatus parsing (self-skip, collection vs file, content length), `PUT`
  round-trips (explicit `Content-Length`, `Content-Type`, no chunked encoding,
  including the unknown-size buffering path), `HEAD` existence, and parent
  bounding at the root.
