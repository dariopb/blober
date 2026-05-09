package azure_storage

import (
	"strings"
	"testing"
	"time"
)

func TestHumanSize(t *testing.T) {
	tests := []struct {
		size int64
		want string
	}{
		{861117, "840.9 KB"},
		{632101376, "602.8 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
	}

	for _, tt := range tests {
		if got := humanSize(tt.size); got != tt.want {
			t.Fatalf("humanSize(%d) = %q, want %q", tt.size, got, tt.want)
		}
	}
}

func TestWriteBlobListAligned(t *testing.T) {
	var out strings.Builder
	err := WriteBlobList(&out, []BlobInfo{
		{Key: "meru-logo.png", Size: 861117, LastModified: time.Date(2026, 5, 8, 1, 46, 32, 0, time.UTC)},
		{Key: "ubuntu.qcow2", Size: 632101376, LastModified: time.Date(2026, 5, 8, 2, 46, 57, 0, time.UTC)},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := out.String()
	for _, want := range []string{"KEY", "BYTES", "HUMAN", "LAST_MODIFIED", "840.9 KB", "602.8 MB"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "meru-logo.png     861117  840.9 KB") {
		t.Fatalf("output not aligned as expected:\n%s", got)
	}
}
