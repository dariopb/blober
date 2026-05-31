package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/dariopb/blober/cmd/blober/tui"
	azure_storage "github.com/dariopb/blober/pkg/azure_storage"
	"github.com/urfave/cli/v3"
)

func main() {
	if err := newCommand().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name:  "blober",
		Usage: "authenticate to Azure Storage and list, upload, download, or browse blobs",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "subscription", Usage: "Azure subscription ID", Sources: cli.EnvVars("AZURE_SUBSCRIPTION_ID"), Required: true},
			&cli.StringFlag{Name: "account", Usage: "Azure Storage account name", Sources: cli.EnvVars("AZURE_STORAGE_ACCOUNT"), Required: true},
			&cli.StringFlag{Name: "tenant", Usage: "Microsoft Entra tenant ID", Sources: cli.EnvVars("AZURE_TENANT_ID")},
			&cli.StringFlag{Name: "client-id", Usage: "OAuth client ID", Sources: cli.EnvVars("AZURE_CLIENT_ID")},
			&cli.StringFlag{Name: "token-file", Usage: "token cache path", Sources: cli.EnvVars("AZURE_STORAGE_TOKEN_FILE")},
			&cli.BoolFlag{Name: "user-flow", Usage: "authenticate with browser-based OAuth user flow instead of device code when login is required"},
			&cli.BoolFlag{Name: "verbose", Usage: "enable verbose logging"},
		},
		Commands: []*cli.Command{
			blobCommand(),
			tuiCommand(),
		},
	}
}

func blobCommand() *cli.Command {
	return &cli.Command{
		Name:  "blob",
		Usage: "list, upload, or download blobs",
		Commands: []*cli.Command{
			blobListCommand(),
			blobUploadCommand(),
			blobDownloadCommand(),
		},
	}
}

func blobListCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list blobs in a container",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "container", Usage: "container name", Required: true},
			&cli.StringFlag{Name: "prefix", Usage: "blob key prefix"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			container := cmd.String("container")
			prefix := cmd.String("prefix")
			if err := azure_storage.ValidateContainerName(container); err != nil {
				return err
			}
			if err := azure_storage.ValidateBlobPrefix(prefix); err != nil {
				return err
			}
			cfg := buildConfig(cmd)
			cred, err := azure_storage.Login(ctx, cfg)
			if err != nil {
				return err
			}
			client, err := azure_storage.NewBlobServiceClient(cred, cfg)
			if err != nil {
				return err
			}
			blobs, err := azure_storage.ListBlobs(ctx, client, container, prefix)
			if err != nil {
				return err
			}
			return azure_storage.WriteBlobList(cmd.Writer, blobs)
		},
	}
}

func blobUploadCommand() *cli.Command {
	return &cli.Command{
		Name:  "upload",
		Usage: "upload a local file to a blob key",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "container", Usage: "container name", Required: true},
			&cli.StringFlag{Name: "key", Usage: "blob key", Required: true},
			&cli.StringFlag{Name: "file", Usage: "local source file", TakesFile: true, Required: true},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			container := cmd.String("container")
			key := cmd.String("key")
			src := cmd.String("file")
			if err := azure_storage.ValidateContainerName(container); err != nil {
				return err
			}
			if err := azure_storage.ValidateBlobKey(key); err != nil {
				return err
			}
			if _, err := os.Stat(src); err != nil {
				return err
			}
			cfg := buildConfig(cmd)
			cred, err := azure_storage.Login(ctx, cfg)
			if err != nil {
				return err
			}
			client, err := azure_storage.NewBlobServiceClient(cred, cfg)
			if err != nil {
				return err
			}
			progress := newTransferProgress(cmd.ErrWriter, "upload")
			err = azure_storage.UploadBlobWithProgress(ctx, client, container, key, src, progress.Callback)
			progress.Finish()
			return err
		},
	}
}

func blobDownloadCommand() *cli.Command {
	return &cli.Command{
		Name:  "download",
		Usage: "download a blob key to a local file",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "container", Usage: "container name", Required: true},
			&cli.StringFlag{Name: "key", Usage: "blob key", Required: true},
			&cli.StringFlag{Name: "file", Usage: "local destination file", TakesFile: true, Required: true},
			&cli.BoolFlag{Name: "force", Usage: "overwrite destination if it exists"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			container := cmd.String("container")
			key := cmd.String("key")
			dst := cmd.String("file")
			if err := azure_storage.ValidateContainerName(container); err != nil {
				return err
			}
			if err := azure_storage.ValidateBlobKey(key); err != nil {
				return err
			}
			if !cmd.Bool("force") {
				if _, err := os.Stat(dst); err == nil {
					return fmt.Errorf("destination exists: %s (use --force to overwrite)", dst)
				} else if !os.IsNotExist(err) {
					return err
				}
			}
			cfg := buildConfig(cmd)
			cred, err := azure_storage.Login(ctx, cfg)
			if err != nil {
				return err
			}
			client, err := azure_storage.NewBlobServiceClient(cred, cfg)
			if err != nil {
				return err
			}
			progress := newTransferProgress(cmd.ErrWriter, "download")
			err = azure_storage.DownloadBlobFileWithProgress(ctx, client, container, key, dst, cmd.Bool("force"), progress.Callback)
			progress.Finish()
			return err
		},
	}
}

func tuiCommand() *cli.Command {
	return &cli.Command{
		Name:  "tui",
		Usage: "browse and copy blobs in a two-pane terminal UI",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "container", Usage: "container name"},
			&cli.StringFlag{Name: "prefix", Usage: "initial blob key prefix"},
			&cli.StringFlag{Name: "local-path", Usage: "initial local directory", TakesFile: true, Value: "."},
			&cli.StringFlag{Name: "theme-file", Usage: "JSON theme file"},
			&cli.BoolFlag{Name: "force", Usage: "overwrite destination files during copy"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			container := cmd.String("container")
			prefix := cmd.String("prefix")
			localPath := cmd.String("local-path")
			if container != "" {
				if err := azure_storage.ValidateContainerName(container); err != nil {
					return err
				}
			}
			if container == "" && prefix != "" {
				return fmt.Errorf("--prefix requires --container")
			}
			if err := azure_storage.ValidateBlobPrefix(prefix); err != nil {
				return err
			}
			if stat, err := os.Stat(localPath); err != nil {
				return err
			} else if !stat.IsDir() {
				return fmt.Errorf("local path is not a directory: %s", localPath)
			}
			theme, err := tui.LoadThemeFile(cmd.String("theme-file"))
			if err != nil {
				return err
			}
			cfg := buildConfig(cmd)
			cred, err := azure_storage.Login(ctx, cfg)
			if err != nil {
				return err
			}
			client, err := azure_storage.NewBlobServiceClient(cred, cfg)
			if err != nil {
				return err
			}
			model := tui.New(tui.Config{
				Client:      client,
				AccountName: cfg.AccountName,
				Container:   container,
				Prefix:      prefix,
				LocalPath:   localPath,
				Force:       cmd.Bool("force"),
				Theme:       theme,
			})
			_, err = tea.NewProgram(model, tea.WithAltScreen()).Run()
			return err
		},
	}
}

func buildConfig(cmd *cli.Command) azure_storage.Config {
	return azure_storage.Config{
		SubscriptionID: cmd.String("subscription"),
		AccountName:    cmd.String("account"),
		TenantID:       cmd.String("tenant"),
		ClientID:       cmd.String("client-id"),
		TokenFile:      cmd.String("token-file"),
		UserFlow:       cmd.Bool("user-flow"),
	}.Normalize()
}

type transferProgress struct {
	w         io.Writer
	operation string
	start     time.Time
	lastDone  int64
	lastTotal int64
}

func newTransferProgress(w io.Writer, operation string) *transferProgress {
	return &transferProgress{
		w:         w,
		operation: operation,
		start:     time.Now(),
	}
}

func (p *transferProgress) Callback(done, total int64) {
	p.lastDone = done
	p.lastTotal = total
	fmt.Fprint(p.w, formatTransferProgress(p.operation, done, total, time.Since(p.start)))
}

func (p *transferProgress) Finish() {
	fmt.Fprint(p.w, formatTransferComplete(p.operation, p.lastDone, time.Since(p.start)))
}

func formatTransferProgress(operation string, done, total int64, elapsed time.Duration) string {
	speed := megabytesPerSecond(done, elapsed)
	if total > 0 {
		percent := (float64(done) / float64(total)) * 100
		return fmt.Sprintf("\r%s %d/%d bytes (%.1f%%) %.2f MB/s", operation, done, total, percent, speed)
	}
	return fmt.Sprintf("\r%s %d bytes %.2f MB/s", operation, done, speed)
}

func formatTransferComplete(operation string, done int64, elapsed time.Duration) string {
	return fmt.Sprintf("\n%s complete: %d bytes in %s (%.2f MB/s overall)\n", operation, done, elapsed.Round(time.Millisecond), megabytesPerSecond(done, elapsed))
}

func megabytesPerSecond(bytes int64, elapsed time.Duration) float64 {
	if bytes <= 0 || elapsed <= 0 {
		return 0
	}
	return (float64(bytes) / (1024 * 1024)) / elapsed.Seconds()
}
