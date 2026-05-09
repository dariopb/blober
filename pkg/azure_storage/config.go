package azure_storage

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	DefaultTenantID     = "common"
	DefaultClientID     = "04b07795-8ddb-461a-bbee-02f9e1bf7b46"
	DefaultTokenFile    = "./azure_storage_token.json"
	StorageScope        = "https://storage.azure.com/.default"
	OfflineAccess       = "offline_access"
	defaultValiditySkew = 5 * 60
)

type Config struct {
	TenantID       string
	ClientID       string
	SubscriptionID string
	AccountName    string
	TokenFile      string
	Scope          string
}

func (c Config) Normalize() Config {
	if c.TenantID == "" {
		c.TenantID = DefaultTenantID
	}
	if c.ClientID == "" {
		c.ClientID = DefaultClientID
	}
	if c.TokenFile == "" {
		c.TokenFile = DefaultTokenFile
	}
	if c.Scope == "" {
		c.Scope = StorageScope + " " + OfflineAccess
	}
	return c
}

func (c Config) ValidateBase() error {
	c = c.Normalize()
	if strings.TrimSpace(c.SubscriptionID) == "" {
		return errors.New("subscription is required")
	}
	return ValidateAccountName(c.AccountName)
}

var (
	accountRE       = regexp.MustCompile(`^[a-z0-9]{3,24}$`)
	containerRE     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,61}[a-z0-9])$`)
	consecutiveDash = regexp.MustCompile(`--`)
)

func ValidateAccountName(account string) error {
	if !accountRE.MatchString(account) {
		return fmt.Errorf("storage account name must be 3-24 lowercase letters and digits")
	}
	return nil
}

func ValidateContainerName(container string) error {
	if !containerRE.MatchString(container) || consecutiveDash.MatchString(container) {
		return fmt.Errorf("container name must be 3-63 lowercase letters, digits, or hyphens, with no leading/trailing or consecutive hyphens")
	}
	return nil
}

func ValidateBlobKey(key string) error {
	if key == "" {
		return errors.New("blob key is required")
	}
	if len([]byte(key)) > 1024 {
		return errors.New("blob key must be no more than 1024 UTF-8 bytes")
	}
	if strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") || strings.Contains(key, "//") {
		return errors.New("blob key must not start or end with / or contain //")
	}
	return nil
}

func ValidateBlobPrefix(prefix string) error {
	if len([]byte(prefix)) > 1024 {
		return errors.New("blob prefix must be no more than 1024 UTF-8 bytes")
	}
	if strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "//") {
		return errors.New("blob prefix must not start with / or contain //")
	}
	return nil
}
