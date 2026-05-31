package azure_storage

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

var ErrTokenExpired = errors.New("access token is expired")

type refreshableTokenCredential struct {
	mu     sync.Mutex
	cfg    Config
	client *http.Client
	token  CachedToken
}

func (c *refreshableTokenCredential) GetToken(ctx context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !isTokenUsable(c.token, time.Now()) {
		if err := c.refreshLocked(ctx); err != nil {
			return azcore.AccessToken{}, err
		}
	}
	return c.accessTokenLocked(), nil
}

func newRefreshableTokenCredential(ctx context.Context, cfg Config, client *http.Client, token CachedToken) azcore.TokenCredential {
	if client == nil {
		client = http.DefaultClient
	}
	cred := &refreshableTokenCredential{
		cfg:    cfg.Normalize(),
		client: client,
		token:  token,
	}
	if token.RefreshToken != "" {
		go cred.refreshLoop(ctx)
	}
	return cred
}

func (c *refreshableTokenCredential) refreshLoop(ctx context.Context) {
	for {
		c.mu.Lock()
		wait := time.Until(time.Unix(c.token.ExpiresOn, 0).Add(-defaultValiditySkew * time.Second))
		c.mu.Unlock()
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}

		c.mu.Lock()
		err := c.refreshLocked(ctx)
		c.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			timer := time.NewTimer(time.Minute)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

func (c *refreshableTokenCredential) refreshLocked(ctx context.Context) error {
	if c.token.RefreshToken == "" {
		return ErrTokenExpired
	}
	refreshed, err := refreshCachedToken(ctx, c.client, c.cfg, c.token.RefreshToken)
	if err != nil {
		return err
	}
	if err := saveCachedToken(c.cfg.TokenFile, refreshed); err != nil {
		return err
	}
	c.token = refreshed
	return nil
}

func (c *refreshableTokenCredential) accessTokenLocked() azcore.AccessToken {
	return azcore.AccessToken{
		Token:     c.token.AccessToken,
		ExpiresOn: time.Unix(c.token.ExpiresOn, 0),
	}
}
