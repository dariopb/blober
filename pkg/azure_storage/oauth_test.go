package azure_storage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/dariopb/azure-storage/pkg/azure_storage/internal/oauth"
)

func TestRefreshCachedToken(t *testing.T) {
	cfg := testConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+cfg.TenantID+"/oauth2/v2.0/token" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		assertForm(t, r.Form, "grant_type", "refresh_token")
		assertForm(t, r.Form, "client_id", cfg.ClientID)
		assertForm(t, r.Form, "refresh_token", "old-refresh")
		assertForm(t, r.Form, "scope", cfg.Scope)
		_ = json.NewEncoder(w).Encode(oauth.TokenResponse{
			AccessToken:  "new-access",
			RefreshToken: "new-refresh",
			ExpiresIn:    3600,
		})
	}))
	defer server.Close()
	restore := setIdentityBaseURL(t, server.URL)
	defer restore()

	token, err := refreshCachedToken(context.Background(), server.Client(), cfg, "old-refresh")
	if err != nil {
		t.Fatalf("refreshCachedToken: %v", err)
	}
	if token.AccessToken != "new-access" || token.RefreshToken != "new-refresh" {
		t.Fatalf("token = %#v", token)
	}
}

func TestRequestDeviceCode(t *testing.T) {
	cfg := testConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+cfg.TenantID+"/oauth2/v2.0/devicecode" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		assertForm(t, r.Form, "client_id", cfg.ClientID)
		assertForm(t, r.Form, "scope", cfg.Scope)
		_ = json.NewEncoder(w).Encode(oauth.DeviceCodeResponse{
			UserCode:        "ABCD",
			DeviceCode:      "device",
			VerificationURI: "https://example.test/device",
			ExpiresIn:       60,
			Interval:        1,
			Message:         "message",
		})
	}))
	defer server.Close()
	restore := setIdentityBaseURL(t, server.URL)
	defer restore()

	device, err := requestDeviceCode(context.Background(), server.Client(), cfg)
	if err != nil {
		t.Fatalf("requestDeviceCode: %v", err)
	}
	if device.DeviceCode != "device" || device.UserCode != "ABCD" {
		t.Fatalf("device = %#v", device)
	}
}

func TestPollDeviceCodeAuthorizationPendingThenSuccess(t *testing.T) {
	cfg := testConfig(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		assertForm(t, r.Form, "grant_type", "urn:ietf:params:oauth:grant-type:device_code")
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(oauth.ErrorResponse{ErrorCode: "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(oauth.TokenResponse{
			AccessToken:  "access",
			RefreshToken: "refresh",
			ExpiresIn:    3600,
		})
	}))
	defer server.Close()
	restore := setIdentityBaseURL(t, server.URL)
	defer restore()

	token, err := pollDeviceCode(context.Background(), server.Client(), cfg, oauth.DeviceCodeResponse{
		DeviceCode: "device",
		ExpiresIn:  5,
		Interval:   1,
	})
	if err != nil {
		t.Fatalf("pollDeviceCode: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if token.AccessToken != "access" || token.RefreshToken != "refresh" {
		t.Fatalf("token = %#v", token)
	}
}

func setIdentityBaseURL(t *testing.T, value string) func() {
	t.Helper()
	old := identityBaseURL
	identityBaseURL = value
	return func() { identityBaseURL = old }
}

func assertForm(t *testing.T, form url.Values, key, want string) {
	t.Helper()
	if got := form.Get(key); got != want {
		t.Fatalf("form[%s] = %q, want %q", key, got, want)
	}
}
