package azure_storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/dariopb/azure-storage/pkg/azure_storage/internal/oauth"
)

var identityBaseURL = "https://login.microsoftonline.com"

func Login(ctx context.Context, cfg Config) (azcore.TokenCredential, error) {
	cfg = cfg.Normalize()
	if err := cfg.ValidateBase(); err != nil {
		return nil, err
	}

	if cached, ok, err := loadCachedToken(cfg.TokenFile, cfg); err != nil {
		if !errors.Is(err, ErrCachePermission) {
			return nil, err
		}
	} else if ok {
		if isTokenUsable(cached, time.Now()) {
			return credentialFromCachedToken(cached), nil
		}
		if cached.RefreshToken != "" {
			if refreshed, err := refreshCachedToken(ctx, http.DefaultClient, cfg, cached.RefreshToken); err == nil {
				if err := saveCachedToken(cfg.TokenFile, refreshed); err != nil {
					return nil, err
				}
				return credentialFromCachedToken(refreshed), nil
			}
		}
	}

	token, err := runDeviceFlow(ctx, http.DefaultClient, cfg)
	if err != nil {
		return nil, err
	}
	if err := saveCachedToken(cfg.TokenFile, token); err != nil {
		return nil, err
	}
	return credentialFromCachedToken(token), nil
}

func refreshCachedToken(ctx context.Context, client *http.Client, cfg Config, refreshToken string) (CachedToken, error) {
	values := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {cfg.ClientID},
		"refresh_token": {refreshToken},
		"scope":         {cfg.Scope},
	}
	resp, err := postForm(ctx, client, tokenURL(cfg.TenantID), values)
	if err != nil {
		return CachedToken{}, err
	}
	return cachedTokenFromResponse(resp, cfg, refreshToken)
}

func runDeviceFlow(ctx context.Context, client *http.Client, cfg Config) (CachedToken, error) {
	device, err := requestDeviceCode(ctx, client, cfg)
	if err != nil {
		return CachedToken{}, err
	}
	if device.Message != "" {
		fmt.Fprintln(os.Stderr, device.Message)
	} else {
		fmt.Fprintf(os.Stderr, "Open %s and enter code %s\n", device.VerificationURI, device.UserCode)
	}
	return pollDeviceCode(ctx, client, cfg, device)
}

func requestDeviceCode(ctx context.Context, client *http.Client, cfg Config) (oauth.DeviceCodeResponse, error) {
	values := url.Values{
		"client_id": {cfg.ClientID},
		"scope":     {cfg.Scope},
	}
	resp, err := postForm(ctx, client, deviceCodeURL(cfg.TenantID), values)
	if err != nil {
		return oauth.DeviceCodeResponse{}, err
	}
	var device oauth.DeviceCodeResponse
	if err := json.Unmarshal(resp, &device); err != nil {
		return oauth.DeviceCodeResponse{}, err
	}
	if device.DeviceCode == "" || device.ExpiresIn <= 0 {
		return oauth.DeviceCodeResponse{}, errors.New("device-code response missing device_code or expires_in")
	}
	return device, nil
}

func pollDeviceCode(ctx context.Context, client *http.Client, cfg Config, device oauth.DeviceCodeResponse) (CachedToken, error) {
	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.NewTimer(time.Duration(device.ExpiresIn) * time.Second)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return CachedToken{}, ctx.Err()
		case <-deadline.C:
			return CachedToken{}, errors.New("device code expired before authorization completed")
		case <-time.After(interval):
		}

		values := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {cfg.ClientID},
			"device_code": {device.DeviceCode},
		}
		body, err := postForm(ctx, client, tokenURL(cfg.TenantID), values)
		if err == nil {
			return cachedTokenFromResponse(body, cfg, "")
		}

		var oauthErr oauth.ErrorResponse
		if !errors.As(err, &oauthErr) {
			continue
		}
		switch oauthErr.ErrorCode {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		default:
			return CachedToken{}, oauthErr
		}
	}
}

func cachedTokenFromResponse(body []byte, cfg Config, oldRefresh string) (CachedToken, error) {
	var token oauth.TokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return CachedToken{}, err
	}
	if token.AccessToken == "" || token.ExpiresIn <= 0 {
		return CachedToken{}, errors.New("token response missing access_token or expires_in")
	}
	if token.RefreshToken == "" {
		token.RefreshToken = oldRefresh
	}
	return CachedToken{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		ExpiresOn:    time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).Unix(),
		TenantID:     cfg.TenantID,
		ClientID:     cfg.ClientID,
		Scope:        cfg.Scope,
	}, nil
}

func postForm(ctx context.Context, client *http.Client, endpoint string, values url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("identity endpoint returned HTTP %d", resp.StatusCode)
	}
	var oauthErr oauth.ErrorResponse
	if err := json.Unmarshal(body, &oauthErr); err != nil || oauthErr.ErrorCode == "" {
		return nil, fmt.Errorf("identity endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil, oauthErr
}

func deviceCodeURL(tenant string) string {
	return strings.TrimRight(identityBaseURL, "/") + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/devicecode"
}

func tokenURL(tenant string) string {
	return strings.TrimRight(identityBaseURL, "/") + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
}
