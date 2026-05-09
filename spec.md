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
- The token cache file is JSON, written with mode `0600`, and contains:
  `access_token`, `refresh_token`, `expires_on`, `tenant_id`,
  `client_id`, and `scope`.

On each run:

1. Load the cache if it exists and has safe permissions.
2. Use the cached access token if it is valid with a 5-minute safety
   margin.
3. Refresh with `grant_type=refresh_token` if possible.
4. Fall back to device-code flow:
   - request a device code from
     `/{tenant}/oauth2/v2.0/devicecode`;
   - print the browser login instructions to stderr;
   - poll `/{tenant}/oauth2/v2.0/token` respecting the server-provided
     interval;
   - handle `authorization_pending`, `slow_down`, `expired_token`, and
     `access_denied`;
   - cache the successful token atomically.

## Library API

Package path: `github.com/dariopb/azure-storage/pkg/azure_storage`

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
| `--subscription` | `AZURE_SUBSCRIPTION_ID`    | yes      | Azure subscription ID. |
| `--account`      | `AZURE_STORAGE_ACCOUNT`    | yes      | Storage account name. |
| `--tenant`       | `AZURE_TENANT_ID`          | no       | Entra tenant; default `common`. |
| `--client-id`    | `AZURE_CLIENT_ID`          | no       | OAuth client ID. |
| `--token-file`   | `AZURE_STORAGE_TOKEN_FILE` | no       | Token cache path. |
| `--verbose`      | -                          | no       | Enable verbose logging. |

Commands:

```sh
blober blob list --container data --prefix logs/
blober blob upload --container data --key reports/may.csv --file ./may.csv
blober blob download --container data --key reports/may.csv --file ./may.csv --force
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
        ├── main.go
        └── main_test.go
```

## Dependencies

- `github.com/Azure/azure-sdk-for-go/sdk/azcore`
- `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob`
- `github.com/urfave/cli/v3`
- Standard library `net/http` for OAuth device-code and refresh-token
  exchanges.

## Testing

- Config defaulting and validation.
- Token cache behavior and permissions.
- OAuth refresh/device-code behavior via `httptest`.
- Blob local-file safety checks.
- CLI validation and progress formatting.
- Optional integration tests may exercise real blob list/upload/download
  against a storage account.

## Security notes

- The token cache contains bearer credentials and must remain `0600`.
- Token values must never be printed, logged, or included in errors.
- `azure_storage_token.json` should stay ignored by git.
