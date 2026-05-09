package azure_storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type CachedToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresOn    int64  `json:"expires_on"`
	TenantID     string `json:"tenant_id"`
	ClientID     string `json:"client_id"`
	Scope        string `json:"scope"`
}

var ErrCachePermission = errors.New("token cache has unsafe permissions")

func loadCachedToken(path string, cfg Config) (CachedToken, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return CachedToken{}, false, nil
	}
	if err != nil {
		return CachedToken{}, false, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return CachedToken{}, false, fmt.Errorf("%w: %s must be readable only by the owner", ErrCachePermission, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return CachedToken{}, false, err
	}

	var token CachedToken
	if err := json.Unmarshal(data, &token); err != nil {
		return CachedToken{}, false, nil
	}
	if token.AccessToken == "" || token.ExpiresOn == 0 {
		return CachedToken{}, false, nil
	}
	if token.TenantID != cfg.TenantID || token.ClientID != cfg.ClientID || token.Scope != cfg.Scope {
		return CachedToken{}, false, nil
	}
	return token, true, nil
}

func isTokenUsable(token CachedToken, now time.Time) bool {
	return token.AccessToken != "" && now.Add(defaultValiditySkew*time.Second).Unix() < token.ExpiresOn
}

func saveCachedToken(path string, token CachedToken) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}

	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".azure_storage_token_*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Chmod(path, 0o600)
	}
	return nil
}
