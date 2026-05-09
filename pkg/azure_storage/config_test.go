package azure_storage

import "testing"

func TestConfigNormalize(t *testing.T) {
	cfg := (Config{}).Normalize()
	if cfg.TenantID != DefaultTenantID {
		t.Fatalf("TenantID = %q, want %q", cfg.TenantID, DefaultTenantID)
	}
	if cfg.ClientID != DefaultClientID {
		t.Fatalf("ClientID = %q, want %q", cfg.ClientID, DefaultClientID)
	}
	if cfg.TokenFile != DefaultTokenFile {
		t.Fatalf("TokenFile = %q, want %q", cfg.TokenFile, DefaultTokenFile)
	}
	if cfg.Scope != StorageScope+" "+OfflineAccess {
		t.Fatalf("Scope = %q", cfg.Scope)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name    string
		fn      func() error
		wantErr bool
	}{
		{"account ok", func() error { return ValidateAccountName("acct123") }, false},
		{"account uppercase", func() error { return ValidateAccountName("Acct123") }, true},
		{"container ok", func() error { return ValidateContainerName("data-123") }, false},
		{"container short", func() error { return ValidateContainerName("ab") }, true},
		{"blob key ok", func() error { return ValidateBlobKey("reports/2026/05/may.csv") }, false},
		{"blob key leading slash", func() error { return ValidateBlobKey("/reports/may.csv") }, true},
		{"blob key double slash", func() error { return ValidateBlobKey("reports//may.csv") }, true},
		{"blob prefix empty", func() error { return ValidateBlobPrefix("") }, false},
		{"blob prefix virtual folder", func() error { return ValidateBlobPrefix("logs/2026/") }, false},
		{"blob prefix leading slash", func() error { return ValidateBlobPrefix("/logs/") }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
