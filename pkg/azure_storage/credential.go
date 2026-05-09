package azure_storage

import (
	"context"
	"errors"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

var ErrTokenExpired = errors.New("access token is expired")

type staticTokenCredential struct {
	token azcore.AccessToken
}

func (c staticTokenCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if time.Now().Add(defaultValiditySkew * time.Second).After(c.token.ExpiresOn) {
		return azcore.AccessToken{}, ErrTokenExpired
	}
	return c.token, nil
}

func credentialFromCachedToken(token CachedToken) azcore.TokenCredential {
	return staticTokenCredential{
		token: azcore.AccessToken{
			Token:     token.AccessToken,
			ExpiresOn: time.Unix(token.ExpiresOn, 0),
		},
	}
}
