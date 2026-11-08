# blober

`blober` is a Go library plus command-line tool for authenticating to Azure
with OAuth 2.0 (device-code or browser user flow) and operating on
Azure Blob Storage.

- Reusable library: [`pkg/azure_storage`](./pkg/azure_storage)
- CLI entry point: [`cmd/blober`](./cmd/blober)
- Interactive two-pane TUI for browsing/copying between the local
  filesystem and Azure Blob Storage (built on
  [bubbletea](https://github.com/charmbracelet/bubbletea)).

See [`spec.md`](./spec.md) for the full design.

## Features

- Reuse an existing Azure CLI (`az login`) session, with device-code or browser
  (auth-code + PKCE) login against Microsoft Entra ID as a fallback.
- Local token cache with automatic refresh (kept fresh in the background for
  long-lived processes such as the TUI).
- `list`, `upload`, `download` blob commands with progress (bytes, %, MB/s).
- Two-pane TUI (`blober tui`) for interactive transfers.

## Requirements

- Go **1.25+** (see [`go.mod`](./go.mod))
- An Azure subscription and a Storage account you can access.

## Build

### Linux / macOS

```sh
./build.sh
```

This produces two binaries in the repo root:

- `blober`      — Linux/macOS (matches the host toolchain)
- `blober.exe`  — Windows amd64 (cross-compiled)

You can also build directly with `go`:

```sh
go build -o blober ./cmd/blober
```

### Windows (PowerShell)

```powershell
go build -o blober.exe .\cmd\blober
```

### Run tests

```sh
go test ./...
```

## Configuration

All flags can also be provided via environment variables.

| Flag             | Env var                     | Description                                  |
| ---------------- | --------------------------- | -------------------------------------------- |
| `--subscription` | `AZURE_SUBSCRIPTION_ID`     | Azure subscription ID (required)             |
| `--account`      | `AZURE_STORAGE_ACCOUNT`     | Storage account name (required)              |
| `--tenant`       | `AZURE_TENANT_ID`           | Entra tenant (default: `common`)             |
| `--client-id`    | `AZURE_CLIENT_ID`           | OAuth client ID (default: Azure CLI client)  |
| `--token-file`   | `AZURE_STORAGE_TOKEN_FILE`  | Token cache path (default: `./azure_storage_token.json`) |
| `--user-flow`    | —                           | Use browser auth-code + PKCE instead of device code |
| `--verbose`      | —                           | Verbose logging                              |

Authenticated commands first reuse the app's cached token and refresh it as
needed. If no usable cached token is available, they try the current user's
Azure CLI (`az login`) session for the configured subscription and tenant.
Azure CLI manages renewal of these tokens; they are not saved to the app's
token cache. If Azure CLI is unavailable or cannot obtain a token, you will be
prompted to sign in (device code by default, or browser if `--user-flow` is set).

## Usage

The general shape is:

```
blober --subscription <SUB> --account <ACCOUNT> <command> [flags]
```

### Linux / macOS examples

```sh
export AZURE_SUBSCRIPTION_ID=00000000-0000-0000-0000-000000000000
export AZURE_STORAGE_ACCOUNT=mystorageacct

# List blobs in a container
./blober blob list --container mycontainer

# List blobs under a prefix
./blober blob list --container mycontainer --prefix logs/2026/

# Upload a local file to a blob key
./blober blob upload \
    --container mycontainer \
    --key uploads/report.pdf \
    --file ./report.pdf

# Download a blob to a local file (overwrite if it exists)
./blober blob download \
    --container mycontainer \
    --key uploads/report.pdf \
    --file ./report.pdf \
    --force

# Launch the interactive two-pane TUI
./blober tui --container mycontainer --local-path ./downloads

# Use browser login if cached and Azure CLI credentials are unavailable
./blober --user-flow blob list --container mycontainer
```

### Windows examples (PowerShell)

```powershell
$env:AZURE_SUBSCRIPTION_ID = "00000000-0000-0000-0000-000000000000"
$env:AZURE_STORAGE_ACCOUNT = "mystorageacct"

# List blobs in a container
.\blober.exe blob list --container mycontainer

# Upload a local file
.\blober.exe blob upload `
    --container mycontainer `
    --key uploads\report.pdf `
    --file .\report.pdf

# Download a blob
.\blober.exe blob download `
    --container mycontainer `
    --key uploads\report.pdf `
    --file .\report.pdf `
    --force

# Launch the TUI
.\blober.exe tui --container mycontainer --local-path .\downloads
```

### Windows examples (cmd.exe)

```bat
set AZURE_SUBSCRIPTION_ID=00000000-0000-0000-0000-000000000000
set AZURE_STORAGE_ACCOUNT=mystorageacct

blober.exe blob list --container mycontainer
blober.exe blob upload  --container mycontainer --key uploads/report.pdf --file report.pdf
blober.exe blob download --container mycontainer --key uploads/report.pdf --file report.pdf --force
blober.exe tui --container mycontainer --local-path .\downloads
```

## TUI navigation and saved connections

| Key | Action |
| --- | ------ |
| `F2` | Rename the highlighted file or directory. |
| `m` | Move selected items (or the highlighted item) to the other pane after confirmation. |
| `g` | Enter a directory path or Azure prefix directly. |
| `Ctrl+B` | Save the current pane's connection and directory as a named bookmark. |
| `b` | Open the bookmark picker; Enter opens, Delete removes a bookmark. |
| `1` | Open keyboard help; Up/Down or PageUp/PageDown scroll it. |

Rename and move reject existing destinations, including when `--force` is set.
Local, SFTP and WebDAV use native rename/move when possible. Other moves copy
each complete source item before deleting it. A failed or cancelled transfer
keeps that item's source, but earlier completed items stay moved and copied
destination files may remain. Copy/delete moves do not preserve permissions or
timestamps and reject symlinks and special files rather than changing their
meaning. Empty directories are preserved where supported; moving a tree with
empty directories to Azure or plain HTTP is rejected.

The path prompt supports relative paths for Local/SFTP/HTTP/WebDAV and `~` for
the local home directory. Azure prefixes are container-root-relative (`/` goes
to the root). Web paths must stay within the connection's root URL; use `p` to
connect elsewhere. Navigation failures leave the current directory unchanged.
Text prompts support cursor editing, `Ctrl+U` to clear, and `Esc` to cancel.

Bookmarks are saved explicitly in `blober/bookmarks.json` under the OS user
configuration directory (`$XDG_CONFIG_HOME`, otherwise `~/.config` on Linux).
The file is written atomically with owner-only permissions. Bookmarks contain
connection parameters, an optional SSH key *path*, and the current directory or
Azure container/prefix, never passwords, tokens or private-key contents.
Credential-bearing URLs cannot be saved. Remote bookmarks reopen the provider
form for review and password entry, then reconnect when you press Connect.
Bookmark names must be unique; deleting a bookmark never deletes stored files.

## Library usage

```go
import (
    "context"
    "github.com/dariopb/blober/pkg/azure_storage"
)

func main() {
    cfg := azure_storage.Config{
        SubscriptionID: "00000000-0000-0000-0000-000000000000",
        AccountName:    "mystorageacct",
    }

    ctx := context.Background()
    cred, err := azure_storage.Login(ctx, cfg)
    if err != nil { panic(err) }

    client, err := azure_storage.NewBlobServiceClient(cred, cfg)
    if err != nil { panic(err) }

    blobs, err := azure_storage.ListBlobs(ctx, client, "mycontainer", "")
    if err != nil { panic(err) }

    _ = azure_storage.WriteBlobList(os.Stdout, blobs)
}
```

## Security notes

- The token cache (`azure_storage_token.json` by default) contains access and
  refresh tokens. It is written with mode `0600` and **must never** be
  committed to source control — it's listed in [`.gitignore`](./.gitignore).
- Use `--token-file` / `AZURE_STORAGE_TOKEN_FILE` to relocate the cache if
  needed (for example, to `$XDG_CONFIG_HOME/blober/token.json`).

## License

See repository for license details.
