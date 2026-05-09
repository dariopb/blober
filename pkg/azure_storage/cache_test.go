package azure_storage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCacheRoundTrip(t *testing.T) {
	cfg := testConfig(t)
	path := filepath.Join(t.TempDir(), "token.json")
	token := CachedToken{
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresOn:    time.Now().Add(time.Hour).Unix(),
		TenantID:     cfg.TenantID,
		ClientID:     cfg.ClientID,
		Scope:        cfg.Scope,
	}

	if err := saveCachedToken(path, token); err != nil {
		t.Fatalf("saveCachedToken: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("mode = %o, want 0600", got)
		}
	}

	got, ok, err := loadCachedToken(path, cfg)
	if err != nil {
		t.Fatalf("loadCachedToken: %v", err)
	}
	if !ok {
		t.Fatal("cache not loaded")
	}
	if got.AccessToken != token.AccessToken || got.RefreshToken != token.RefreshToken {
		t.Fatalf("loaded token mismatch: %#v", got)
	}
}

func TestLoadCachedTokenMismatchIsUnusable(t *testing.T) {
	cfg := testConfig(t)
	path := filepath.Join(t.TempDir(), "token.json")
	token := CachedToken{
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresOn:    time.Now().Add(time.Hour).Unix(),
		TenantID:     "other",
		ClientID:     cfg.ClientID,
		Scope:        cfg.Scope,
	}
	if err := saveCachedToken(path, token); err != nil {
		t.Fatalf("saveCachedToken: %v", err)
	}

	_, ok, err := loadCachedToken(path, cfg)
	if err != nil {
		t.Fatalf("loadCachedToken: %v", err)
	}
	if ok {
		t.Fatal("mismatched cache should be unusable")
	}
}

func TestLoadCachedTokenUnsafePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions test")
	}
	cfg := testConfig(t)
	path := filepath.Join(t.TempDir(), "token.json")
	token := CachedToken{
		AccessToken: "access",
		ExpiresOn:   time.Now().Add(time.Hour).Unix(),
		TenantID:    cfg.TenantID,
		ClientID:    cfg.ClientID,
		Scope:       cfg.Scope,
	}
	if err := saveCachedToken(path, token); err != nil {
		t.Fatalf("saveCachedToken: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := loadCachedToken(path, cfg)
	if !errors.Is(err, ErrCachePermission) {
		t.Fatalf("err = %v, want ErrCachePermission", err)
	}
}

func TestIsTokenUsableUsesSkew(t *testing.T) {
	now := time.Now()
	if isTokenUsable(CachedToken{AccessToken: "token", ExpiresOn: now.Add(10 * time.Minute).Unix()}, now) != true {
		t.Fatal("token expiring after skew should be usable")
	}
	if isTokenUsable(CachedToken{AccessToken: "token", ExpiresOn: now.Add(time.Minute).Unix()}, now) != false {
		t.Fatal("token expiring inside skew should be unusable")
	}
}

func TestProgressAdapterIsMonotonicAndClamped(t *testing.T) {
	var got [][2]int64
	progress := progressAdapter(func(done, total int64) {
		got = append(got, [2]int64{done, total})
	}, 10)

	progress(3)
	progress(2)
	progress(12)

	want := [][2]int64{{3, 10}, {3, 10}, {10, 10}}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		SubscriptionID: "00000000-0000-0000-0000-000000000000",
		AccountName:    "acct123",
	}.Normalize()
}
