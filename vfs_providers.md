# VFS providers

This document specifies the virtual filesystem (VFS) abstraction used by the
`blober` TUI to make the dual-pane file manager work uniformly across multiple
storage backends, and the `p` shortcut that lets the user switch the provider of
the active pane at runtime.

## Goals

- Decouple panel/model logic from any single storage backend so that listing,
  reading, writing, stat, mkdir and delete all flow through one interface.
- Support three backends that are interchangeable per pane:
  - Local filesystem.
  - Azure Blob Storage (virtual prefixes).
  - SCP over SSH (sftp).
- Allow the user to change the active pane's provider at runtime via `p`.

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
}
```

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
`scpSession` owns the `*ssh.Client` and `*sftp.Client`; a pointer to it is stored
on the panel so bubbletea's value copies of the model share one connection.
`Remove` recurses over sftp; `Walk` recurses over sftp directories.

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

- A Type row cycles Local / Azure / SCP with the left/right arrows.
- SCP reveals Host, Port (default `22`), User, Password (masked), and Key file
  fields. A host and user are required; leaving both Password and Key file empty
  uses the default `~/.ssh`/agent credentials described above.
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
- Local and Azure apply immediately (Azure opens the container picker if no
  container is selected). SCP dials asynchronously, showing a connecting state
  that `Esc` cancels.

### Generation tokens

Switching a pane's provider bumps a per-pane generation counter. Asynchronous
list results and SCP connection results carry the generation they were issued
under; the model drops any result whose generation no longer matches the pane,
so a stale in-flight listing or connection cannot overwrite a pane that has
since changed provider. A discarded SCP session is closed.

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

## Testing

- `safeRel` rejects traversal/absolute/empty paths and accepts normal ones.
- `resolveProvider` returns the correct provider per panel kind, and an explicit
  panel provider overrides the kind fallback.
- `localProvider` `Join`/`Parent`/`Walk`/`Open`/`Create` round-trip, including a
  local-to-local copy through `copyCmd`, and no temp files are left behind.
- `p` opens the modal, selecting Local switches the pane, and the SCP form
  validates required fields and supports field editing.
