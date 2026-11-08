package azure_storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/dariopb/blober/pkg/azure_storage/internal/oauth"
)

func TestLoginAzureCLI(t *testing.T) {
	for _, tt := range []struct {
		name       string
		tenant     string
		output     string
		exitCode   string
		missingCLI bool
		cached     bool
		canceled   bool
		wantDevice bool
	}{
		{name: "default tenant"},
		{name: "explicit tenant", tenant: "11111111-1111-1111-1111-111111111111"},
		{name: "cached token preferred", cached: true},
		{name: "not logged in", exitCode: "1", wantDevice: true},
		{name: "CLI unavailable", missingCLI: true, wantDevice: true},
		{name: "invalid CLI response", output: "invalid JSON", wantDevice: true},
		{name: "canceled", canceled: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.TokenFile = filepath.Join(t.TempDir(), "token.json")
			if tt.tenant != "" {
				cfg.TenantID = tt.tenant
			}
			output := tt.output
			if output == "" {
				body, err := json.Marshal(map[string]any{
					"accessToken": "cli-access",
					"expiresOn":   time.Now().Add(time.Hour).Format("2006-01-02 15:04:05.000000"),
					"expires_on":  time.Now().Add(time.Hour).Unix(),
				})
				if err != nil {
					t.Fatal(err)
				}
				output = string(body)
			}
			argsFile := fakeAzureCLI(t, output, tt.exitCode, tt.missingCLI)
			if tt.cached {
				if err := saveCachedToken(cfg.TokenFile, CachedToken{
					AccessToken: "cached-access",
					ExpiresOn:   time.Now().Add(time.Hour).Unix(),
					TenantID:    cfg.TenantID,
					ClientID:    cfg.ClientID,
					Scope:       cfg.Scope,
				}); err != nil {
					t.Fatal(err)
				}
			}
			prompted := false
			cfg.Prompt = func(AuthPrompt) { prompted = true }
			deviceRequested := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				deviceRequested = true
				if r.URL.Path != "/"+cfg.TenantID+"/oauth2/v2.0/devicecode" {
					t.Errorf("unexpected identity request: %s", r.URL.Path)
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"device_fallback"}`))
			}))
			defer server.Close()
			defer setIdentityBaseURL(t, server.URL)()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.canceled {
				cancel()
			}
			cred, err := Login(ctx, cfg)
			if tt.canceled {
				if !errors.Is(err, context.Canceled) || deviceRequested {
					t.Fatalf("err = %v, device requested = %v", err, deviceRequested)
				}
				return
			}
			if tt.wantDevice {
				var oauthErr oauth.ErrorResponse
				if !errors.As(err, &oauthErr) || oauthErr.ErrorCode != "device_fallback" || !deviceRequested {
					t.Fatalf("err = %v, device requested = %v", err, deviceRequested)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			token, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{StorageScope}})
			if err != nil {
				t.Fatal(err)
			}
			want := "cli-access"
			if tt.cached {
				want = "cached-access"
			}
			if token.Token != want || deviceRequested || prompted {
				t.Fatalf("token = %q, device requested = %v, prompted = %v", token.Token, deviceRequested, prompted)
			}
			args, err := os.ReadFile(argsFile)
			if tt.cached {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cached login invoked Azure CLI: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"account\nget-access-token\n", "--resource\nhttps://storage.azure.com\n", "--subscription\n" + cfg.SubscriptionID + "\n"} {
				if !strings.Contains(string(args), want) {
					t.Errorf("CLI args %q missing %q", args, want)
				}
			}
			if tt.tenant != "" {
				if !strings.Contains(string(args), "--tenant\n"+tt.tenant+"\n") {
					t.Errorf("CLI args missing tenant: %q", args)
				}
			} else if strings.Contains(string(args), "--tenant") {
				t.Errorf("default login should use the CLI tenant: %q", args)
			}
			if _, err := os.Stat(cfg.TokenFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("CLI token should not be written to app cache: %v", err)
			}
		})
	}
}

func TestLoginAzureCLIFallsBackToBrowser(t *testing.T) {
	fakeAzureCLI(t, "", "1", false)
	cfg := testConfig(t)
	cfg.TokenFile = filepath.Join(t.TempDir(), "token.json")
	cfg.UserFlow = true
	cfg.Prompt = func(AuthPrompt) {}
	wantErr := errors.New("browser opened")
	oldOpen := openBrowserFunc
	openBrowserFunc = func(string) error { return wantErr }
	defer func() { openBrowserFunc = oldOpen }()

	if _, err := Login(context.Background(), cfg); !errors.Is(err, wantErr) {
		t.Fatalf("Login error = %v, want browser fallback", err)
	}
}

func fakeAzureCLI(t *testing.T, output, exitCode string, missing bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake Azure CLI uses a POSIX shell")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	t.Setenv("PATH", dir)
	t.Setenv("BLOBER_TEST_AZ_ARGS", argsFile)
	t.Setenv("BLOBER_TEST_AZ_OUTPUT", output)
	if exitCode == "" {
		exitCode = "0"
	}
	t.Setenv("BLOBER_TEST_AZ_EXIT", exitCode)
	if !missing {
		script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$BLOBER_TEST_AZ_ARGS\"\nprintf '%s\\n' \"$BLOBER_TEST_AZ_OUTPUT\"\nexit \"$BLOBER_TEST_AZ_EXIT\"\n"
		if err := os.WriteFile(filepath.Join(dir, "az"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return argsFile
}

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

func TestRefreshableCredentialRenewsBeforeExpiry(t *testing.T) {
	cfg := testConfig(t)
	cfg.TokenFile = filepath.Join(t.TempDir(), "token.json")
	refreshed := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		assertForm(t, r.Form, "grant_type", "refresh_token")
		assertForm(t, r.Form, "refresh_token", "old-refresh")
		_ = json.NewEncoder(w).Encode(oauth.TokenResponse{
			AccessToken:  "new-access",
			RefreshToken: "new-refresh",
			ExpiresIn:    3600,
		})
		refreshed <- struct{}{}
	}))
	defer server.Close()
	restore := setIdentityBaseURL(t, server.URL)
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	token := CachedToken{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresOn:    time.Now().Add(time.Minute).Unix(),
		TenantID:     cfg.TenantID,
		ClientID:     cfg.ClientID,
		Scope:        cfg.Scope,
	}
	cred := newRefreshableTokenCredential(ctx, cfg, server.Client(), token)

	select {
	case <-refreshed:
	case <-time.After(2 * time.Second):
		t.Fatal("credential did not refresh before expiry")
	}
	access, err := cred.GetToken(context.Background(), policy.TokenRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if access.Token != "new-access" {
		t.Fatalf("access token = %q, want new-access", access.Token)
	}
	cached, ok, err := loadCachedToken(cfg.TokenFile, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || cached.AccessToken != "new-access" || cached.RefreshToken != "new-refresh" {
		t.Fatalf("cached token = %#v ok=%v", cached, ok)
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

func TestRunBrowserFlowExchangesCallbackCode(t *testing.T) {
	cfg := testConfig(t)
	var challenge string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+cfg.TenantID+"/oauth2/v2.0/token" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		assertForm(t, r.Form, "grant_type", "authorization_code")
		assertForm(t, r.Form, "client_id", cfg.ClientID)
		assertForm(t, r.Form, "scope", cfg.Scope)
		assertForm(t, r.Form, "code", "auth-code")
		if r.Form.Get("redirect_uri") == "" {
			t.Fatal("missing redirect_uri")
		}
		verifier := r.Form.Get("code_verifier")
		if verifier == "" {
			t.Fatal("missing code_verifier")
		}
		if pkceChallenge(verifier) != challenge {
			t.Fatalf("PKCE challenge mismatch")
		}
		_ = json.NewEncoder(w).Encode(oauth.TokenResponse{
			AccessToken:  "browser-access",
			RefreshToken: "browser-refresh",
			ExpiresIn:    3600,
		})
	}))
	defer server.Close()
	restoreIdentity := setIdentityBaseURL(t, server.URL)
	defer restoreIdentity()
	var authLog bytes.Buffer
	oldOutput := authOutput
	authOutput = &authLog
	defer func() { authOutput = oldOutput }()
	oldOpen := openBrowserFunc
	openBrowserFunc = func(target string) error {
		auth, err := url.Parse(target)
		if err != nil {
			return err
		}
		if auth.Path != "/"+cfg.TenantID+"/oauth2/v2.0/authorize" {
			t.Fatalf("authorize path = %s", auth.Path)
		}
		query := auth.Query()
		assertValues(t, query, "response_type", "code")
		assertValues(t, query, "client_id", cfg.ClientID)
		assertValues(t, query, "scope", cfg.Scope)
		assertValues(t, query, "code_challenge_method", "S256")
		assertValues(t, query, "prompt", "select_account")
		challenge = query.Get("code_challenge")
		redirectURI := query.Get("redirect_uri")
		if redirectURI == "" || query.Get("state") == "" || challenge == "" {
			t.Fatalf("missing authorize parameters: %s", target)
		}
		callback, err := url.Parse(redirectURI)
		if err != nil {
			return err
		}
		params := callback.Query()
		params.Set("code", "auth-code")
		params.Set("state", query.Get("state"))
		callback.RawQuery = params.Encode()
		resp, err := http.Get(callback.String())
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("callback status = %d", resp.StatusCode)
		}
		return nil
	}
	defer func() { openBrowserFunc = oldOpen }()

	token, err := runBrowserFlow(context.Background(), server.Client(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "browser-access" || token.RefreshToken != "browser-refresh" {
		t.Fatalf("token = %#v", token)
	}
	for _, want := range []string{"Listening for OAuth redirect at http://localhost:", "Opening browser for login:"} {
		if !strings.Contains(authLog.String(), want) {
			t.Fatalf("auth output missing %q:\n%s", want, authLog.String())
		}
	}
}

func TestDeviceAuthURLUsesVerificationComplete(t *testing.T) {
	got := deviceAuthURL(oauth.DeviceCodeResponse{
		VerificationURI:         "https://microsoft.com/devicelogin",
		VerificationURIComplete: "https://microsoft.com/devicelogin?otc=ABCD",
		UserCode:                "ignored",
	})
	if got != "https://microsoft.com/devicelogin?otc=ABCD" {
		t.Fatalf("deviceAuthURL = %q", got)
	}
}

func TestDeviceAuthURLDoesNotInventCompleteURL(t *testing.T) {
	got := deviceAuthURL(oauth.DeviceCodeResponse{
		VerificationURI: "https://microsoft.com/devicelogin?existing=1",
		UserCode:        "A B-C",
	})
	want := "https://microsoft.com/devicelogin?existing=1"
	if got != want {
		t.Fatalf("deviceAuthURL = %q, want %q", got, want)
	}
}

func TestWriteDeviceFlowInstructionsPrintsQRCodeURL(t *testing.T) {
	var out bytes.Buffer
	writeDeviceFlowInstructions(&out, oauth.DeviceCodeResponse{
		VerificationURI:         "https://microsoft.com/devicelogin",
		VerificationURIComplete: "https://microsoft.com/devicelogin?otc=ABCD-EFGH",
		UserCode:                "ABCD-EFGH",
		Message:                 "Go authenticate",
	})
	got := out.String()
	for _, want := range []string{"Go authenticate", "https://microsoft.com/devicelogin?otc=ABCD-EFGH", "█"} {
		if !strings.Contains(got, want) {
			t.Fatalf("instructions missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "ABCD-EFGH\n\n") {
		t.Fatalf("expected empty line before QR code:\n%s", got)
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

func assertValues(t *testing.T, values url.Values, key, want string) {
	t.Helper()
	if got := values.Get(key); got != want {
		t.Fatalf("values[%s] = %q, want %q", key, got, want)
	}
}
