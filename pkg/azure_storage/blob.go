package azure_storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

type ProgressFunc func(bytesDone, bytesTotal int64)

type BlobInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

type BlobEntry struct {
	Name         string
	Key          string
	Prefix       string
	IsDir        bool
	Size         int64
	LastModified time.Time
}

type ContainerInfo struct {
	Name string
}

func NewBlobServiceClient(cred azcore.TokenCredential, cfg Config) (*azblob.Client, error) {
	cfg = cfg.Normalize()
	if err := cfg.ValidateBase(); err != nil {
		return nil, err
	}
	return azblob.NewClient(fmt.Sprintf("https://%s.blob.core.windows.net/", cfg.AccountName), cred, nil)
}

func ListContainers(ctx context.Context, c *azblob.Client) ([]ContainerInfo, error) {
	pager := c.NewListContainersPager(nil)
	var containers []ContainerInfo
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range page.ContainerItems {
			if item == nil || item.Name == nil {
				continue
			}
			containers = append(containers, ContainerInfo{Name: *item.Name})
		}
	}
	sort.Slice(containers, func(i, j int) bool {
		return containers[i].Name < containers[j].Name
	})
	return containers, nil
}

func ListBlobs(ctx context.Context, c *azblob.Client, container, prefix string) ([]BlobInfo, error) {
	if err := ValidateContainerName(container); err != nil {
		return nil, err
	}
	if err := ValidateBlobPrefix(prefix); err != nil {
		return nil, err
	}
	opts := &azblob.ListBlobsFlatOptions{}
	if prefix != "" {
		opts.Prefix = &prefix
	}
	pager := c.NewListBlobsFlatPager(container, opts)
	var blobs []BlobInfo
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		if page.Segment == nil {
			continue
		}
		for _, item := range page.Segment.BlobItems {
			if item == nil || item.Name == nil {
				continue
			}
			info := BlobInfo{Key: *item.Name}
			if item.Properties != nil {
				if item.Properties.ContentLength != nil {
					info.Size = *item.Properties.ContentLength
				}
				if item.Properties.LastModified != nil {
					info.LastModified = *item.Properties.LastModified
				}
			}
			blobs = append(blobs, info)
		}
	}
	return blobs, nil
}

func ListBlobEntries(ctx context.Context, c *azblob.Client, container, prefix string) ([]BlobEntry, error) {
	blobs, err := ListBlobs(ctx, c, container, prefix)
	if err != nil {
		return nil, err
	}
	dirs := make(map[string]BlobEntry)
	var entries []BlobEntry
	for _, blob := range blobs {
		rest := strings.TrimPrefix(blob.Key, prefix)
		if rest == "" {
			continue
		}
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			name := rest[:slash+1]
			dirPrefix := JoinBlobPrefix(prefix, strings.TrimSuffix(name, "/")) + "/"
			dirs[dirPrefix] = BlobEntry{Name: name, Prefix: dirPrefix, IsDir: true}
			continue
		}
		entries = append(entries, BlobEntry{
			Name:         rest,
			Key:          blob.Key,
			IsDir:        false,
			Size:         blob.Size,
			LastModified: blob.LastModified,
		})
	}
	for _, dir := range dirs {
		entries = append(entries, dir)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

func WriteBlobList(out io.Writer, blobs []BlobInfo) error {
	keyWidth := len("KEY")
	sizeWidth := len("BYTES")
	humanWidth := len("HUMAN")
	for _, blob := range blobs {
		keyWidth = max(keyWidth, len(blob.Key))
		sizeWidth = max(sizeWidth, len(fmt.Sprintf("%d", blob.Size)))
		humanWidth = max(humanWidth, len(humanSize(blob.Size)))
	}
	if _, err := fmt.Fprintf(out, "%-*s  %*s  %*s  %s\n", keyWidth, "KEY", sizeWidth, "BYTES", humanWidth, "HUMAN", "LAST_MODIFIED"); err != nil {
		return err
	}
	for _, blob := range blobs {
		lastModified := ""
		if !blob.LastModified.IsZero() {
			lastModified = blob.LastModified.UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(out, "%-*s  %*d  %*s  %s\n", keyWidth, blob.Key, sizeWidth, blob.Size, humanWidth, humanSize(blob.Size), lastModified); err != nil {
			return err
		}
	}
	return nil
}

func JoinBlobPrefix(prefix, name string) string {
	prefix = strings.TrimPrefix(prefix, "/")
	name = strings.Trim(name, "/")
	if prefix == "" {
		return name
	}
	if name == "" {
		return strings.TrimSuffix(prefix, "/")
	}
	return strings.TrimSuffix(prefix, "/") + "/" + name
}

func BlobBaseName(key string) string {
	return pathBase(strings.TrimSuffix(key, "/"))
}

func pathBase(path string) string {
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		return path[slash+1:]
	}
	return path
}

func humanSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", value/unit)
}

func UploadBlob(ctx context.Context, c *azblob.Client, container, key, srcPath string) error {
	return UploadBlobWithProgress(ctx, c, container, key, srcPath, nil)
}

func UploadBlobWithProgress(ctx context.Context, c *azblob.Client, container, key, srcPath string, progress ProgressFunc) error {
	if err := ValidateContainerName(container); err != nil {
		return err
	}
	if err := ValidateBlobKey(key); err != nil {
		return err
	}
	if srcPath == "" {
		return errors.New("file path is required")
	}
	file, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	total := stat.Size()
	emitProgress(progress, 0, total)
	_, err = c.UploadFile(ctx, container, key, file, &azblob.UploadFileOptions{
		Progress: progressAdapter(progress, total),
	})
	if err == nil {
		emitProgress(progress, total, total)
	}
	return err
}

func DownloadBlob(ctx context.Context, c *azblob.Client, container, key, dstPath string) error {
	return DownloadBlobFile(ctx, c, container, key, dstPath, false)
}

func DownloadBlobFile(ctx context.Context, c *azblob.Client, container, key, dstPath string, force bool) error {
	return DownloadBlobFileWithProgress(ctx, c, container, key, dstPath, force, nil)
}

func DownloadBlobFileWithProgress(ctx context.Context, c *azblob.Client, container, key, dstPath string, force bool, progress ProgressFunc) error {
	if err := ValidateContainerName(container); err != nil {
		return err
	}
	if err := ValidateBlobKey(key); err != nil {
		return err
	}
	if dstPath == "" {
		return errors.New("file path is required")
	}
	if !force {
		if _, err := os.Stat(dstPath); err == nil {
			return fmt.Errorf("destination exists: %s (use --force to overwrite)", dstPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	props, err := c.ServiceClient().NewContainerClient(container).NewBlobClient(key).GetProperties(ctx, nil)
	if err != nil {
		return err
	}
	var total int64
	if props.ContentLength != nil {
		total = *props.ContentLength
	}
	emitProgress(progress, 0, total)

	dir := filepath.Dir(dstPath)
	tmp, err := os.CreateTemp(dir, ".azure_storage_download_*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if _, err := c.DownloadFile(ctx, container, key, tmp, &azblob.DownloadFileOptions{
		Progress: progressAdapter(progress, total),
	}); err != nil {
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
	emitProgress(progress, total, total)
	return os.Rename(tmpName, dstPath)
}

func progressAdapter(progress ProgressFunc, total int64) func(int64) {
	if progress == nil {
		return nil
	}
	var maxDone int64
	return func(done int64) {
		if done > maxDone {
			maxDone = done
		}
		emitProgress(progress, maxDone, total)
	}
}

func emitProgress(progress ProgressFunc, done, total int64) {
	if progress == nil {
		return
	}
	if total >= 0 && done > total {
		done = total
	}
	if done < 0 {
		done = 0
	}
	progress(done, total)
}
