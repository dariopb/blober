package oauth

import "strings"

type DeviceCodeResponse struct {
	UserCode        string `json:"user_code"`
	DeviceCode      string `json:"device_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Message         string `json:"message"`
}

type TokenResponse struct {
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int    `json:"expires_in"`
	ExtExpiresIn int    `json:"ext_expires_in"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type ErrorResponse struct {
	ErrorCode        string `json:"error"`
	ErrorDescription string `json:"error_description"`
	ErrorCodes       []int  `json:"error_codes"`
	Timestamp        string `json:"timestamp"`
	TraceID          string `json:"trace_id"`
	CorrelationID    string `json:"correlation_id"`
}

func (e ErrorResponse) Error() string {
	parts := []string{e.ErrorCode}
	if e.ErrorDescription != "" {
		parts = append(parts, e.ErrorDescription)
	}
	if e.TraceID != "" {
		parts = append(parts, "trace_id="+e.TraceID)
	}
	if e.CorrelationID != "" {
		parts = append(parts, "correlation_id="+e.CorrelationID)
	}
	return strings.Join(parts, ": ")
}
