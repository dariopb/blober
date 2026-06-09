# blober

`blober` is a Go library plus command-line tool for authenticating to
Azure with OAuth 2.0 device flow and operating on Azure Blob Storage.

The reusable library lives under `pkg/azure_storage`; the CLI entry point
lives under `cmd/blober`.

## Goals

- Authenticate interactively with Microsoft Entra ID using device-code
  flow.
- Cache access/refresh tokens in the current working directory and reuse
  or refresh them on later runs.
- Construct authenticated Azure Blob Storage clients.
- Provide a `blober` CLI for listing, uploading, and downloading blobs.
- Provide an interactive two-pane terminal UI for browsing and copying
  between local files and Azure Blob Storage.
- Show upload/download progress with bytes transferred, percent complete,
  current MB/s, and final overall MB/s.

## Non-goals

- Management-plane operations such as creating storage accounts or
  containers.
- General-purpose Azure credential discovery. Authentication is explicit:
  cached token, refresh token, then device flow.

## Authentication

- Default tenant: `common`.
- Default client ID: Azure CLI public client ID
  `04b07795-8ddb-461a-bbee-02f9e1bf7b46`.
- Scope: `https://storage.azure.com/.default offline_access`.
- Token cache path: `./azure_storage_token.json` by default, overridable
  with `--token-file` or `AZURE_STORAGE_TOKEN_FILE`.
- `--user-flow` uses browser-based authorization-code + PKCE login instead of
  device-code login when cached/refresh-token authentication is not possible.
  If `BROWSER` is set, that command is invoked with the authorization URL;
  otherwise the platform default browser opener is used. The flow starts a
  localhost HTTP server for the OAuth redirect, exchanges the returned code for
  tokens, and stores them in the same cache.
- The token cache file is JSON, written with mode `0600`, and contains:
  `access_token`, `refresh_token`, `expires_on`, `tenant_id`,
  `client_id`, and `scope`.

On each run:

1. Load the cache if it exists and has safe permissions.
2. Use the cached access token if it is valid with a 5-minute safety
   margin.
3. Refresh with `grant_type=refresh_token` if possible.
4. If `--user-flow` is set, fall back to browser user flow:
   - start a localhost callback server;
   - generate authorization-code + PKCE parameters;
   - open the authorization URL using `BROWSER` or the platform browser opener;
     the redirect URI should be the loopback root URL
     `http://localhost:<port>` so it matches public-client loopback redirect
     registrations such as the default Azure CLI client ID; include
     `prompt=select_account` so the browser asks which signed-in account to use;
     print the localhost redirect URL and authorization URL for debugging. If
     `BROWSER` is set, print the command being invoked and stream that
     command's stdout/stderr to the CLI output;
   - exchange the callback `code` for tokens;
   - cache the successful token atomically.
5. Otherwise fall back to device-code flow:
   - request a device code from
     `/{tenant}/oauth2/v2.0/devicecode`;
   - print the browser login instructions to stderr, plus a terminal QR code.
     If the identity endpoint returns `verification_uri_complete`, the QR code
     should use that complete URL, including its device-code query parameter.
     Otherwise the QR code should use `verification_uri` and continue showing
     the user code separately;
   - poll `/{tenant}/oauth2/v2.0/token` respecting the server-provided
     interval;
   - handle `authorization_pending`, `slow_down`, `expired_token`, and
     `access_denied`;
   - cache the successful token atomically.

The credential returned after login must keep the current access token
refreshable for long-lived processes such as the TUI. If a refresh token is
available, it should start an in-process background refresh loop that renews
the token before the 5-minute safety margin and updates the cache atomically.
Token lookup should also refresh on demand if a request observes a token that
is no longer usable.

## Library API

Package path: `github.com/dariopb/blober/pkg/azure_storage`

```go
type Config struct {
    TenantID       string // default: "common"
    ClientID       string // default: Azure CLI client ID
    SubscriptionID string // required for user context/logging
    AccountName    string // required
    TokenFile      string // default: "./azure_storage_token.json"
    Scope          string // default: "https://storage.azure.com/.default offline_access"
}

type CachedToken struct {
    AccessToken  string `json:"access_token"`
    RefreshToken string `json:"refresh_token,omitempty"`
    ExpiresOn    int64  `json:"expires_on"`
    TenantID     string `json:"tenant_id"`
    ClientID     string `json:"client_id"`
    Scope        string `json:"scope"`
}

func Login(ctx context.Context, cfg Config) (azcore.TokenCredential, error)
func NewBlobServiceClient(cred azcore.TokenCredential, cfg Config) (*azblob.Client, error)

type BlobInfo struct {
    Key          string
    Size         int64
    LastModified time.Time
}

type ProgressFunc func(bytesDone, bytesTotal int64)

func ListBlobs(ctx context.Context, c *azblob.Client, container, prefix string) ([]BlobInfo, error)
func WriteBlobList(out io.Writer, blobs []BlobInfo) error
func UploadBlob(ctx context.Context, c *azblob.Client, container, key, srcPath string) error
func UploadBlobWithProgress(ctx context.Context, c *azblob.Client, container, key, srcPath string, progress ProgressFunc) error
func DownloadBlob(ctx context.Context, c *azblob.Client, container, key, dstPath string) error
func DownloadBlobFile(ctx context.Context, c *azblob.Client, container, key, dstPath string, force bool) error
func DownloadBlobFileWithProgress(ctx context.Context, c *azblob.Client, container, key, dstPath string, force bool, progress ProgressFunc) error
```

Additional library helpers should be added as needed for the TUI so the UI
does not duplicate transfer or path logic:

```go
type BlobEntry struct {
    Name         string
    Key          string
    Prefix       string
    IsDir        bool
    Size         int64
    LastModified time.Time
}

func ListBlobEntries(ctx context.Context, c *azblob.Client, container, prefix string) ([]BlobEntry, error)
func JoinBlobPrefix(prefix, name string) string
func BlobBaseName(key string) string
```

## Blob naming

Azure Blob Storage has a flat namespace. A blob key is the full blob name;
slashes (`/`) are just characters, although this tool treats them as
virtual folder separators by convention.

Rules used by `blober`:

- `--key` is the exact blob name, passed to the SDK without user-side URL
  encoding.
- Keys may contain `/`, e.g. `reports/2026/05/may.csv`.
- Keys must not start or end with `/` and must not contain `//`.
- Keys are case-sensitive.
- `--prefix` is a literal prefix filter for listing. Use a trailing slash
  to target a virtual folder, e.g. `logs/2026-05/`.
- `--file` is a local OS path and is independent from the blob key.

## CLI

Binary name: `blober`

Global flags:

| Flag             | Env var                    | Required | Description |
|------------------|----------------------------|----------|-------------|
| `--subscription` | `AZURE_SUBSCRIPTION_ID`    | blob cmds | Azure subscription ID. Required by `blob` subcommands; for `tui` it only pre-fills the Azure provider modal. |
| `--account`      | `AZURE_STORAGE_ACCOUNT`    | blob cmds | Storage account name. Required by `blob` subcommands; for `tui` it only pre-fills the Azure provider modal. |
| `--tenant`       | `AZURE_TENANT_ID`          | no       | Entra tenant; default `common`. |
| `--client-id`    | `AZURE_CLIENT_ID`          | no       | OAuth client ID. |
| `--token-file`   | `AZURE_STORAGE_TOKEN_FILE` | no       | Token cache path. |
| `--user-flow`    | -                          | no       | Use browser-based authorization-code + PKCE login when interactive auth is required. |
| `--verbose`      | -                          | no       | Enable verbose logging. |

Commands:

```sh
blober blob list --container data --prefix logs/
blober blob upload --container data --key reports/may.csv --file ./may.csv
blober blob download --container data --key reports/may.csv --file ./may.csv --force
blober tui --container data --prefix logs/ --local-path .
```

### `blob list`

Lists blobs in a container, optionally filtered by `--prefix`. Output is
fixed-width aligned columns with both raw byte size and human-readable
size:

```text
KEY            BYTES      HUMAN     LAST_MODIFIED
meru-logo.png  861117     840.9 KB  2026-05-08T01:46:32Z
ubuntu.qcow2   632101376  602.8 MB  2026-05-08T02:46:57Z
```

### `blob upload`

Uploads a local file to `container/key` using block blob upload. Existing
blobs at the same key are overwritten. Progress is printed to stderr:

```text
upload 1048576/4194304 bytes (25.0%) 12.34 MB/s
upload complete: 4194304 bytes in 340ms (11.76 MB/s overall)
```

### `blob download`

Downloads `container/key` to a local file. Existing destination files are
not overwritten unless `--force` is set. Downloads are written to a temp
file in the destination directory, synced, then renamed into place.
Progress is printed to stderr:

```text
download 1048576/4194304 bytes (25.0%) 12.34 MB/s
download complete: 4194304 bytes in 340ms (11.76 MB/s overall)
```

### `tui`

Starts an interactive Midnight Commander-style terminal UI built with
Charm Bubble Tea. The UI has two side-by-side panels. Azure is optional: the
TUI **always starts with both panels on the local filesystem** and performs no
authentication at startup. Any Azure connection (and its OAuth sign-in) happens
on demand through the `p` provider modal, which is pre-filled from the
`--account`/`--subscription`/`--tenant` command-line values when present. Each
panel can independently point at any provider (Local, Azure, SCP, HTTP, WebDAV):

- **Header**: a single top line showing the active panel's context. For an Azure
  panel this includes storage account, container, and current remote prefix.
- **Local panels**: the current working directory by default, or the path
  provided by `--local-path`.
- **Azure panel**: after connecting through the modal, lists Azure Blob Storage
  contents for the chosen container rooted at `--prefix` if provided. When no
  container is selected yet, a modal container picker is shown first.

Flags:

| Flag           | Required | Description |
|----------------|----------|-------------|
| `--container`  | no       | Blob container to browse; if omitted, choose from an accessible-container picker. |
| `--prefix`     | no       | Initial remote virtual directory/prefix; requires `--container`. |
| `--local-path` | no       | Initial local directory; defaults to `.`. |
| `--theme-file` | no       | Explicit JSON theme file overriding named color values and automatic discovery. |
| `--force`      | no       | Allow copy operations to overwrite existing targets after confirmation. |

Behavior:

- The active panel is the source for actions. The inactive panel is the
  target. A visible indicator must distinguish the active panel, such as a
  highlighted border/title and selected-row styling.
- Each panel shows three vertical sections separated by visible `│`
  delimiters: file name, human-readable size (`B`, `KB`, `MB`, etc.), and
  last modified date. Directory entries are sorted before files and display a
  leading `/` before the directory name.
- `Tab` switches the active panel while preserving each panel's current
  directory, cursor, and scroll offset.
- The status bar should keep the persistent shortcut hint short and always
  visible: show `1 -> help`. Pressing `1` opens a centered keyboard-help modal
  that lists all keys and their actions, one per line, with the key and action
  in aligned columns.
- `l` lists accessible containers in a centered modal picker. Selecting a
  container switches the remote panel to that container at the root prefix.
  The same modal opens at startup when `--container` is omitted.
- Arrow keys or `j`/`k` move the selection in the active panel. `PageUp` and
  `PageDown` move by one visible pane page. `Home` moves to the first entry,
  and `End` moves to the last entry.
- `/` edits a live filter for the active panel. Typing filters the visible
  entries by displayed name, `Backspace` removes characters, `Enter` accepts
  the filter, and `Esc` cancels the edit. The active panel filter is shown in
  the status bar.
- `Space` toggles selection for the highlighted file or directory in the active
  panel. Selected items render in yellow, matching Midnight Commander-style
  visual feedback. Pressing `Space` again unselects the item. Selected
  directories are copied recursively after confirmation.
- `Enter` or `RightArrow` opens a directory. For the remote panel, directories
  are virtual prefixes ending in `/`; for the local panel, directories are
  filesystem directories.
- `Backspace`, `h`, or `LeftArrow` moves to the parent directory/prefix when
  one exists. After returning to the parent, the directory/prefix the user just
  left remains selected instead of resetting the cursor to the top.
- `n` opens a fixed-size modal prompt to create a new directory in the active
  panel's current directory. The modal shows selected/deselected GUI-style
  Create and Cancel buttons. Create is selected by default, `Enter` activates
  the selected button, arrow keys move between Create and Cancel, and `Esc`
  cancels. Directory names are single path segments. For Azure Blob Storage,
  the TUI enters the new virtual prefix without creating a trailing slash
  placeholder blob, avoiding empty `<no name>` marker files; the prefix becomes
  visible from the parent after files are copied into it.
- `c` copies from the active source panel to the other panel's current
  directory. If the active panel has selected files, all selected files are
  copied as a batch. If no files are selected, the highlighted file or
  directory is copied:
  - local to remote uploads each source file to
    `remoteCurrentPrefix + localBaseName`;
  - remote to local downloads each source blob to
    `localCurrentDir/remoteBaseName`;
  - copying a directory is recursive after a confirmation modal, preserves the
    selected directory name under the target panel's current directory/prefix,
    and copies every file below it. Empty Azure virtual directories do not
    create placeholder blobs.
- Copy operations display a fixed-size centered modal progress dialog, not a
  status-line update or appended bottom panel. The modal should match
  Midnight Commander behavior by showing the current file name, current file
  progress bar, current transfer speed in human-readable units per second,
  total selection progress bar, and a visible cancel button. Progress UI
  updates should be throttled to roughly every 200ms, while final per-file
  completion updates should still be shown. `Esc` or `Ctrl+C` cancels the
  in-flight copy. Cancellation must remove any partial target file/blob created
  for the interrupted transfer. The modal remains visible until the batch
  completes, fails, or is cancelled.
- Before a copy starts, the TUI checks whether any destination file/blob already
  exists. For one existing target, show an overwrite confirmation modal and
  copy with overwrite enabled only if confirmed. For multiple existing targets,
  the modal offers Overwrite, Overwrite All, and Cancel. Choosing Overwrite
  confirms only the current target and shows the same modal again for the next
  existing target; choosing Overwrite All confirms the whole selection.
- Modal dialogs use the theme's dim-white `modal_background`, configurable
  `modal_border`, and a configurable one-cell down/right `modal_shadow` drawn
  behind the bottom and right edges. All modal actions must be rendered as
  selected/deselected GUI-style buttons, with one empty modal-background line
  before the button row.
- `d` deletes from the active source panel. If the active panel has selected
  files, all selected files are deleted. If no files are selected, the
  highlighted file or directory is deleted. Local directory deletion removes the
  directory tree; remote virtual-directory deletion removes every blob under
  that prefix. Delete must show a fixed-size centered confirmation modal before
  removing anything. The modal shows selected/deselected GUI-style Yes and
  No/Cancel buttons. Yes is selected by default, `Enter` activates the selected
  button, arrow keys move between Yes and No/Cancel, `y` confirms, and `n` or
  `Esc` cancels.
- Existing targets are not overwritten unless the user confirms the overwrite in
  the TUI.
- `r` refreshes the active panel.
- `p` opens a fixed-size centered modal to choose the storage provider for the
  active panel: Local filesystem, Azure Blob Storage, SCP (ssh/sftp), plain HTTP,
  or WebDAV. The Type row cycles with the left/right arrows. Selecting SCP reveals
  Host, Port, User, Password, and Key file fields. A host and user are required;
  if both Password and Key file are left empty, the connection falls back to the
  user's default SSH credentials (ssh-agent and the standard `~/.ssh` keys).
  Pressing Enter on the Key file field opens a directory browser (starting in the
  key's directory, otherwise `~/.ssh`) to navigate and pick a private key.
  Selecting HTTP or WebDAV reveals URL, User and Password fields; the URL is
  required and optional credentials are sent as HTTP Basic auth. Local and Azure
  switch the panel immediately; SCP, HTTP and WebDAV connect asynchronously and
  show a connecting state with `Esc` to cancel. The modal has GUI-style Connect
  and Cancel buttons. The text fields are editable with a visible block cursor:
  left/right move the cursor within the field, Home/End jump to the start/end,
  typing inserts at the cursor, Backspace deletes the character before it, and
  Delete removes the character at it. See `vfs_providers.md` for the underlying
  VFS provider abstraction, the HTTP/WebDAV providers, and SCP
  connection/known-hosts behavior.
- `q`, `Esc`, or `Ctrl+C` exits the TUI.
- The bottom status line shows the active panel, currently highlighted file,
  and full byte size, plus errors or informational messages when present.
  Status output must not corrupt the terminal.
- `t` shows a fixed-size centered editable theme modal listing the current
  named color values in `#RRGGBB` format. Arrow keys or `j`/`k` select a color
  row, and the active editable row is visibly marked. Typing a hex digit starts
  replacing the selected value; `Enter` or `e` explicitly enters edit mode for
  the current value. Valid `#RRGGBB` values apply immediately so the user can
  preview changes live. `Backspace` removes a character. The theme modal has
  GUI-style Edit, Save, and Close buttons; moving below the color list selects
  the button row, arrow keys move between buttons, and `Enter` activates the
  selected button. `s` saves the current theme to `.theme.json` in the current
  directory. `t`, `Esc`, or `q` closes the modal.

Theme files are JSON objects. Missing keys use the default theme. Supported
color names and default values. At TUI startup, `--theme-file` is honored when
provided. Otherwise the TUI first tries `.theme.json` in the current directory,
then `$HOME/.theme.json`, and falls back to these defaults if neither file
exists:

```json
{
  "background": "#0011EE",
  "highlight": "#00AAFF",
  "text": "#D0D0D0",
  "header_text": "#000000",
  "selected": "#FFFF00",
  "error": "#FF0000",
  "modal_border": "#111111",
  "modal_background": "#D0D0D0",
  "modal_shadow": "#000000"
}
```

Remote virtual-directory listing should be derived from flat blob names. At
prefix `logs/`, blob `logs/2026/05/app.log` appears as directory `2026/`;
blob `logs/readme.txt` appears as file `readme.txt`. The remote panel should
include a parent entry (`..`) whenever the current prefix is not empty.

The TUI should use Bubble Tea's model/update/view architecture:

1. Add a top-level `tui` command in `cmd/blober` that performs normal config
   validation, login, and client construction before starting the Bubble Tea
   program.
2. Add a small internal TUI package, for example `cmd/blober/tui`, containing
   panel state, key handling, rendering, and async commands for list and copy
   operations.
3. Keep Azure operations in `pkg/azure_storage`; the Bubble Tea model should
   call library functions instead of using the Azure SDK directly.
4. Represent panels with a shared interface for list/open/parent/copy-source
   behavior, with concrete local and remote implementations.
5. Track selected files per panel by stable source path/key so selections
   survive cursor movement and scrolling, and clear selections after a
   successful copy.
6. Run remote listing and transfers through Bubble Tea commands so the UI
   remains responsive.
7. Reuse existing upload/download progress callbacks and convert progress
   updates into Bubble Tea messages that update the modal's current-file and
   total-batch progress.

## Project layout

```text
.
├── go.mod
├── spec.md
├── pkg/
│   └── azure_storage/
│       ├── blob.go
│       ├── cache.go
│       ├── config.go
│       ├── credential.go
│       ├── oauth.go
│       ├── *_test.go
│       └── internal/
│           └── oauth/
│               └── types.go
└── cmd/
    └── blober/
        ├── tui/
        │   ├── model.go
        │   ├── panel.go
        │   └── *_test.go
        ├── main.go
        └── main_test.go
```

## Dependencies

- `github.com/Azure/azure-sdk-for-go/sdk/azcore`
- `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob`
- `github.com/charmbracelet/bubbletea`
- `github.com/charmbracelet/lipgloss` for layout, borders, and active-panel
  styling.
- `github.com/urfave/cli/v3`
- Standard library `net/http` for OAuth device-code and refresh-token
  exchanges.

## Testing

- Config defaulting and validation.
- Token cache behavior and permissions.
- OAuth refresh/device-code behavior via `httptest`.
- Blob local-file safety checks.
- CLI validation and progress formatting.
- TUI command validation for `--container`, `--prefix`, and `--local-path`.
- TUI model tests for active-panel switching, navigation, refresh, and copy
  target selection.
- TUI rendering tests for visible active-panel indication and status/error
  messages.
- Local panel tests using temporary directories.
- Remote panel tests using fake list/copy implementations; optional
  integration tests may exercise real remote listing and transfers.
- Optional integration tests may exercise real blob list/upload/download
  against a storage account.

## Security notes

- The token cache contains bearer credentials and must remain `0600`.
- Token values must never be printed, logged, or included in errors.
- `azure_storage_token.json` should stay ignored by git.
