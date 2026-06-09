package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	azure_storage "github.com/dariopb/blober/pkg/azure_storage"
)

// WalkItem describes a single file discovered while expanding a directory for a
// copy. Rel is always slash-separated and destination-relative (it includes the
// base directory name of the copied entry).
type WalkItem struct {
	Path    string
	Rel     string
	Size    int64
	ModTime time.Time
}

// Provider is the virtual filesystem abstraction backing a panel. Every panel
// holds a Provider and performs all of its storage operations (listing,
// reading, writing, stat, mkdir, delete) through this interface so that local,
// Azure and scp backends are interchangeable.
type Provider interface {
	// Label returns a short human-readable identity for the backend.
	Label() string
	// List returns the entries at location (including ".." when applicable).
	List(ctx context.Context, location string) ([]Entry, error)
	// Parent returns the parent of location and whether one exists.
	Parent(location string) (string, bool)
	// Join converts a slash-separated, destination-relative path into a
	// provider-native absolute path under location.
	Join(location, rel string) (string, error)
	// Mkdir creates directory name under location and returns the new child
	// location. virtual is true for backends (Azure) where directories are
	// implicit and nothing is physically created.
	Mkdir(ctx context.Context, location, name string) (child string, virtual bool, err error)
	// Virtual reports whether directories are implicit (no physical mkdir).
	Virtual() bool
	// Remove deletes entry, recursing into directories.
	Remove(ctx context.Context, entry Entry) error
	// Exists reports whether a file exists at the provider-native path.
	Exists(ctx context.Context, p string) (bool, error)
	// Open opens a file for reading and reports its size (-1 if unknown).
	Open(ctx context.Context, p string) (io.ReadCloser, int64, error)
	// Create writes the contents of r to the provider-native path, creating
	// any parent directories.
	Create(ctx context.Context, p string, size int64, r io.Reader) error
	// Walk expands a directory entry into a flat list of files.
	Walk(ctx context.Context, entry Entry) ([]WalkItem, error)
	// Close releases any resources held by the provider (e.g. an ssh/sftp
	// connection). Stateless backends return nil.
	Close() error
}

// headerDescriber is an optional interface a Provider may implement to render a
// custom header line (used by the Azure backend to show account/container). When
// a provider does not implement it, the header falls back to "Label: location".
type headerDescriber interface {
	Header(location string) string
}

// safeRel rejects relative paths that could escape the destination root.
func safeRel(rel string) error {
	if rel == "" {
		return errors.New("empty relative path")
	}
	if strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) {
		return fmt.Errorf("unsafe relative path: %s", rel)
	}
	// Relative paths are documented as slash-separated; reject backslashes so a
	// path like `a\..\escape` cannot bypass the segment checks on Windows.
	if strings.Contains(rel, "\\") {
		return fmt.Errorf("unsafe relative path: %s", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("unsafe relative path: %s", rel)
		}
	}
	return nil
}

// progressReader wraps a reader to report bytes consumed and to honor context
// cancellation between reads.
type progressReader struct {
	ctx    context.Context
	r      io.Reader
	report func(done int64)
	done   int64
}

func (pr *progressReader) Read(b []byte) (int, error) {
	if err := pr.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := pr.r.Read(b)
	if n > 0 {
		pr.done += int64(n)
		if pr.report != nil {
			pr.report(pr.done)
		}
	}
	return n, err
}

// WriteTo lets io.Copy pick a concurrent fast path when the wrapped reader
// supports one. Without it, io.Copy of an sftp download would fall back to the
// destination's ReadFrom, which calls Read in a sequential 32KB loop (one
// network round-trip per chunk) and runs ~20x slower than the pipelined
// transfer that sftp.File.WriteTo performs. io.Copy consults src.WriteTo before
// dst.ReadFrom, so implementing this here is enough to engage the fast path.
func (pr *progressReader) WriteTo(w io.Writer) (int64, error) {
	if err := pr.ctx.Err(); err != nil {
		return 0, err
	}
	if wt, ok := pr.r.(io.WriterTo); ok {
		n, err := wt.WriteTo(progressWriter{pr: pr, w: w})
		// If the fast path failed before transferring anything (e.g. a server
		// that rejects the stat sftp.File.WriteTo issues), retry sequentially so
		// the copy still succeeds, just slower. A partial transfer (n>0) is not
		// retried to avoid duplicating bytes.
		if err != nil && n == 0 {
			return pr.copyFallback(w)
		}
		return n, err
	}
	return pr.copyFallback(w)
}

// copyFallback performs a plain buffered copy that still reports progress via
// Read. The onlyReader/onlyWriter wrappers hide the WriteTo/ReadFrom methods so
// io.Copy cannot recurse back into the fast path.
func (pr *progressReader) copyFallback(w io.Writer) (int64, error) {
	return io.Copy(onlyWriter{w}, onlyReader{pr})
}

// progressWriter counts bytes as they are written by a delegated WriteTo and
// honors context cancellation. WriteTo serializes writes on a single goroutine,
// so the counter needs no synchronization.
type progressWriter struct {
	pr *progressReader
	w  io.Writer
}

func (pw progressWriter) Write(b []byte) (int, error) {
	if err := pw.pr.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := pw.w.Write(b)
	if n > 0 {
		pw.pr.done += int64(n)
		if pw.pr.report != nil {
			pw.pr.report(pw.pr.done)
		}
	}
	return n, err
}

// onlyReader and onlyWriter expose just Read/Write so io.Copy does not detect a
// WriterTo/ReaderFrom and recurse into a fast path.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(b []byte) (int, error) { return o.r.Read(b) }

type onlyWriter struct{ w io.Writer }

func (o onlyWriter) Write(b []byte) (int, error) { return o.w.Write(b) }

// localProvider is the local filesystem backend.
type localProvider struct{}

func (localProvider) Label() string { return "Local" }

func (localProvider) List(_ context.Context, location string) ([]Entry, error) {
	return listLocal(location)
}

func (localProvider) Parent(location string) (string, bool) {
	parent := filepath.Dir(location)
	if parent == location {
		return "", false
	}
	return parent, true
}

func (localProvider) Join(location, rel string) (string, error) {
	if err := safeRel(rel); err != nil {
		return "", err
	}
	return filepath.Join(location, filepath.FromSlash(rel)), nil
}

func (localProvider) Mkdir(_ context.Context, location, name string) (string, bool, error) {
	dir := filepath.Join(location, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", false, err
	}
	return dir, false, nil
}

func (localProvider) Virtual() bool { return false }

func (localProvider) Remove(_ context.Context, entry Entry) error {
	return removeLocalEntry(entry)
}

func (localProvider) Exists(_ context.Context, p string) (bool, error) {
	if _, err := os.Stat(p); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}

func (localProvider) Open(_ context.Context, p string) (io.ReadCloser, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

func (localProvider) Create(_ context.Context, p string, _ int64, r io.Reader) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".blober_download_*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, p); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func (localProvider) Walk(_ context.Context, entry Entry) ([]WalkItem, error) {
	base := copyTargetName(entry)
	var items []WalkItem
	err := filepath.WalkDir(entry.Path, func(filePath string, dirEntry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if dirEntry.IsDir() {
			return nil
		}
		info, err := dirEntry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(entry.Path, filePath)
		if err != nil {
			return err
		}
		items = append(items, WalkItem{
			Path:    filePath,
			Rel:     path.Join(base, filepath.ToSlash(rel)),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
		return nil
	})
	return items, err
}

func (localProvider) Close() error { return nil }

// azureProvider is the Azure Blob Storage backend. It is built from the model's
// client and the currently selected container.
type azureProvider struct {
	client      *azblob.Client
	container   string
	accountName string
}

func (azureProvider) Label() string { return "Azure" }

// Close is a no-op: the azblob client is shared, owned by the Model, and must
// not be closed when a pane switches away from Azure.
func (azureProvider) Close() error { return nil }

// Header renders the Azure-specific header line (account/container/prefix).
func (p azureProvider) Header(location string) string {
	account := p.accountName
	if account == "" {
		account = "(unknown account)"
	}
	prefix := location
	if prefix == "" {
		prefix = "(root)"
	}
	container := p.container
	if container == "" {
		container = "(select container)"
	}
	return fmt.Sprintf("Remote: account=%s container=%s prefix=%s", account, container, prefix)
}

func (p azureProvider) List(ctx context.Context, location string) ([]Entry, error) {
	if p.container == "" {
		return nil, nil
	}
	return listRemote(ctx, p.client, p.container, location)
}

func (azureProvider) Parent(location string) (string, bool) {
	if location == "" {
		return "", false
	}
	return parentBlobPrefix(location), true
}

func (azureProvider) Join(location, rel string) (string, error) {
	if err := safeRel(rel); err != nil {
		return "", err
	}
	return azure_storage.JoinBlobPrefix(location, rel), nil
}

func (azureProvider) Mkdir(_ context.Context, location, name string) (string, bool, error) {
	return azure_storage.JoinBlobPrefix(location, name) + "/", true, nil
}

func (azureProvider) Virtual() bool { return true }

func (p azureProvider) Remove(ctx context.Context, entry Entry) error {
	if entry.IsDir {
		return deleteRemotePrefix(ctx, p.client, p.container, entry.Path)
	}
	return cleanupRemoteBlob(ctx, p.client, p.container, entry.Path)
}

func (p azureProvider) Exists(ctx context.Context, key string) (bool, error) {
	return remoteBlobExists(ctx, p.client, p.container, key)
}

func (p azureProvider) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	return azure_storage.OpenBlobStream(ctx, p.client, p.container, key)
}

func (p azureProvider) Create(ctx context.Context, key string, _ int64, r io.Reader) error {
	return azure_storage.UploadBlobStream(ctx, p.client, p.container, key, r)
}

func (p azureProvider) Walk(ctx context.Context, entry Entry) ([]WalkItem, error) {
	blobs, err := azure_storage.ListBlobs(ctx, p.client, p.container, entry.Path)
	if err != nil {
		return nil, err
	}
	base := copyTargetName(entry)
	items := make([]WalkItem, 0, len(blobs))
	for _, blob := range blobs {
		rel := strings.TrimPrefix(blob.Key, entry.Path)
		if rel == "" {
			continue
		}
		items = append(items, WalkItem{
			Path:    blob.Key,
			Rel:     path.Join(base, rel),
			Size:    blob.Size,
			ModTime: blob.LastModified,
		})
	}
	return items, nil
}
