package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBlobUploadValidatesLocalFileBeforeAuth(t *testing.T) {
	cmd := newCommand()
	var stdout, stderr bytes.Buffer
	cmd.Writer = &stdout
	cmd.ErrWriter = &stderr

	err := cmd.Run(context.Background(), []string{
		"blober",
		"--subscription", "00000000-0000-0000-0000-000000000000",
		"--account", "acct123",
		"blob", "upload",
		"--container", "data",
		"--key", "reports/may.csv",
		"--file", "/definitely/not/here.csv",
	})
	if err == nil {
		t.Fatal("expected missing file error")
	}
	if !strings.Contains(err.Error(), "not/here.csv") {
		t.Fatalf("err = %v", err)
	}
}

func TestBlobDownloadRefusesOverwriteBeforeAuth(t *testing.T) {
	tmp := t.TempDir() + "/out.txt"
	if err := os.WriteFile(tmp, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newCommand()

	err := cmd.Run(context.Background(), []string{
		"blober",
		"--subscription", "00000000-0000-0000-0000-000000000000",
		"--account", "acct123",
		"blob", "download",
		"--container", "data",
		"--key", "reports/may.csv",
		"--file", tmp,
	})
	if err == nil {
		t.Fatal("expected overwrite error")
	}
	if !strings.Contains(err.Error(), "use --force") {
		t.Fatalf("err = %v", err)
	}
}

func TestFormatTransferProgress(t *testing.T) {
	if got, want := formatTransferProgress("upload", 25, 100, time.Second), "\rupload 25/100 bytes (25.0%) 0.00 MB/s"; got != want {
		t.Fatalf("progress output = %q, want %q", got, want)
	}

	if got, want := formatTransferProgress("upload", 1024*1024, 0, time.Second), "\rupload 1048576 bytes 1.00 MB/s"; got != want {
		t.Fatalf("progress output = %q, want %q", got, want)
	}
}

func TestFormatTransferComplete(t *testing.T) {
	got := formatTransferComplete("download", 2*1024*1024, 2*time.Second)
	want := "\ndownload complete: 2097152 bytes in 2s (1.00 MB/s overall)\n"
	if got != want {
		t.Fatalf("complete output = %q, want %q", got, want)
	}
}
