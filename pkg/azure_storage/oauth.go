package azure_storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/dariopb/blober/pkg/azure_storage/internal/oauth"
	qrterminal "github.com/mdp/qrterminal/v3"
)

var identityBaseURL = "https://login.microsoftonline.com"
var openBrowserFunc = openBrowser
var authOutput io.Writer = os.Stderr

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
			return newRefreshableTokenCredential(ctx, cfg, http.DefaultClient, cached), nil
		}
		if cached.RefreshToken != "" {
			if refreshed, err := refreshCachedToken(ctx, http.DefaultClient, cfg, cached.RefreshToken); err == nil {
				if err := saveCachedToken(cfg.TokenFile, refreshed); err != nil {
					return nil, err
				}
				return newRefreshableTokenCredential(ctx, cfg, http.DefaultClient, refreshed), nil
			}
		}
	}

	var token CachedToken
	var err error
	if cfg.UserFlow {
		token, err = runBrowserFlow(ctx, http.DefaultClient, cfg)
	} else {
		token, err = runDeviceFlow(ctx, http.DefaultClient, cfg)
	}
	if err != nil {
		return nil, err
	}
	if err := saveCachedToken(cfg.TokenFile, token); err != nil {
		return nil, err
	}
	return newRefreshableTokenCredential(ctx, cfg, http.DefaultClient, token), nil
}

func runBrowserFlow(ctx context.Context, client *http.Client, cfg Config) (CachedToken, error) {
	verifier, err := randomURLToken(32)
	if err != nil {
		return CachedToken{}, err
	}
	state, err := randomURLToken(24)
	if err != nil {
		return CachedToken{}, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return CachedToken{}, err
	}
	defer listener.Close()

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return CachedToken{}, err
	}
	redirectURI := "http://localhost:" + port
	callbackCh := make(chan browserCallback, 1)
	server := browserCallbackServer(state, callbackCh)
	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Shutdown(context.Background())

	authURL := authorizationURL(cfg, redirectURI, state, pkceChallenge(verifier))
	if cfg.Prompt != nil {
		cfg.Prompt(AuthPrompt{
			Kind:            "browser",
			Message:         "Complete sign-in in the browser window that just opened.",
			VerificationURL: authURL,
		})
	} else {
		fmt.Fprintf(authOutput, "Listening for OAuth redirect at %s\n", redirectURI)
		fmt.Fprintf(authOutput, "Opening browser for login: %s\n", authURL)
	}
	if err := openBrowserFunc(authURL); err != nil {
		return CachedToken{}, err
	}

	var callback browserCallback
	select {
	case <-ctx.Done():
		return CachedToken{}, ctx.Err()
	case callback = <-callbackCh:
	}
	if callback.err != nil {
		return CachedToken{}, callback.err
	}

	values := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {cfg.ClientID},
		"scope":         {cfg.Scope},
		"code":          {callback.code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	body, err := postForm(ctx, client, tokenURL(cfg.TenantID), values)
	if err != nil {
		return CachedToken{}, err
	}
	return cachedTokenFromResponse(body, cfg, "")
}

type browserCallback struct {
	code string
	err  error
}

func browserCallbackServer(wantState string, callbackCh chan<- browserCallback) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		var result browserCallback
		switch {
		case query.Get("error") != "":
			result.err = fmt.Errorf("browser login failed: %s", query.Get("error"))
		case query.Get("state") != wantState:
			result.err = errors.New("browser login returned invalid state")
		case query.Get("code") == "":
			result.err = errors.New("browser login did not return an authorization code")
		default:
			result.code = query.Get("code")
		}
		if result.err != nil {
			http.Error(w, result.err.Error(), http.StatusBadRequest)
		} else {
			fmt.Fprintln(w, "Authentication complete. You can close this browser tab.")
		}
		select {
		case callbackCh <- result:
		default:
		}
	})
	return &http.Server{Handler: mux}
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
	if cfg.Prompt != nil {
		cfg.Prompt(buildDevicePrompt(device))
	} else {
		writeDeviceFlowInstructions(os.Stderr, device)
	}
	return pollDeviceCode(ctx, client, cfg, device)
}

// buildDevicePrompt renders the device-code instructions (including an ASCII QR
// code) into an AuthPrompt for callers that display sign-in details themselves.
func buildDevicePrompt(device oauth.DeviceCodeResponse) AuthPrompt {
	authURL := deviceAuthURL(device)
	message := device.Message
	if message == "" {
		message = fmt.Sprintf("Open %s and enter code %s", device.VerificationURI, device.UserCode)
	}
	var qr strings.Builder
	if authURL != "" {
		qrterminal.GenerateWithConfig(authURL, qrterminal.Config{
			Level:          qrterminal.L,
			Writer:         &qr,
			HalfBlocks:     true,
			BlackChar:      qrterminal.BLACK_BLACK,
			WhiteBlackChar: qrterminal.WHITE_BLACK,
			WhiteChar:      qrterminal.WHITE_WHITE,
			BlackWhiteChar: qrterminal.BLACK_WHITE,
			QuietZone:      1,
		})
	}
	return AuthPrompt{
		Kind:            "device",
		Message:         message,
		VerificationURL: authURL,
		UserCode:        device.UserCode,
		QRCode:          qr.String(),
	}
}

func writeDeviceFlowInstructions(w io.Writer, device oauth.DeviceCodeResponse) {
	authURL := deviceAuthURL(device)
	if device.Message != "" {
		fmt.Fprintln(w, device.Message)
	} else {
		fmt.Fprintf(w, "Open %s and enter code %s\n", device.VerificationURI, device.UserCode)
	}
	if authURL == "" {
		return
	}
	if device.VerificationURIComplete != "" {
		fmt.Fprintf(w, "Or scan this QR code to open %s\n", authURL)
	} else {
		fmt.Fprintf(w, "Or scan this QR code to open %s, then enter code %s\n", authURL, device.UserCode)
	}
	fmt.Fprintln(w)
	qrterminal.GenerateWithConfig(authURL, qrterminal.Config{
		Level:          qrterminal.L,
		Writer:         w,
		HalfBlocks:     true,
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		QuietZone:      1,
	})
}

func deviceAuthURL(device oauth.DeviceCodeResponse) string {
	if device.VerificationURIComplete != "" {
		return device.VerificationURIComplete
	}
	return device.VerificationURI
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

func authorizationURL(cfg Config, redirectURI, state, codeChallenge string) string {
	values := url.Values{
		"client_id":             {cfg.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"response_mode":         {"query"},
		"scope":                 {cfg.Scope},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	return strings.TrimRight(identityBaseURL, "/") + "/" + url.PathEscape(cfg.TenantID) + "/oauth2/v2.0/authorize?" + values.Encode()
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomURLToken(byteCount int) (string, error) {
	buf := make([]byte, byteCount)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func openBrowser(target string) error {
	if browser := strings.TrimSpace(os.Getenv("BROWSER")); browser != "" {
		parts := strings.Fields(browser)
		if len(parts) == 0 {
			return errors.New("BROWSER is empty")
		}
		fmt.Fprintf(authOutput, "Invoking BROWSER command: %s\n", browser)
		cmd := exec.Command(parts[0], append(parts[1:], target)...)
		cmd.Stdout = authOutput
		cmd.Stderr = authOutput
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() {
			_ = cmd.Wait()
		}()
		return nil
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

func deviceCodeURL(tenant string) string {
	return strings.TrimRight(identityBaseURL, "/") + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/devicecode"
}

func tokenURL(tenant string) string {
	return strings.TrimRight(identityBaseURL, "/") + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
}
