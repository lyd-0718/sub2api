package service

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type accountTLSRecordingUpstream struct {
	plainCalls int
	tlsCalls   int
	profile    *tlsfingerprint.Profile
}

func (u *accountTLSRecordingUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.plainCalls++
	return accountTLSOKResponse(), nil
}

func (u *accountTLSRecordingUpstream) DoWithTLS(_ *http.Request, _ string, _ int64, _ int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.tlsCalls++
	u.profile = profile
	if profile == nil {
		return u.Do(nil, "", 0, 0)
	}
	return accountTLSOKResponse(), nil
}

func accountTLSOKResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}
}

func TestAccountTLSFingerprintToggleIsPlatformAgnostic(t *testing.T) {
	account := &Account{
		Platform: "future-provider",
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"enable_tls_fingerprint": true},
	}
	require.True(t, account.IsTLSFingerprintEnabled())

	account.Extra["enable_tls_fingerprint"] = false
	require.False(t, account.IsTLSFingerprintEnabled())
	require.False(t, (*Account)(nil).IsTLSFingerprintEnabled())
}

func TestOpenAIUpstreamUsesAccountTLSFingerprintPolicy(t *testing.T) {
	upstream := &accountTLSRecordingUpstream{}
	gateway := &OpenAIGatewayService{
		httpUpstream:        upstream,
		tlsFPProfileService: &TLSFingerprintProfileService{},
	}
	account := &Account{
		ID:          36,
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Concurrency: 2,
		Extra:       map[string]any{"enable_tls_fingerprint": true},
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions", strings.NewReader(`{}`))
	require.NoError(t, err)

	resp, err := gateway.doOpenAIUpstream(req, "", account)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 1, upstream.tlsCalls)
	require.Zero(t, upstream.plainCalls)
	require.NotNil(t, upstream.profile)
	require.Equal(t, "Built-in Default (Node.js 24.x)", upstream.profile.Name)
}

func TestOpenAIUpstreamKeepsDefaultTransportWhenFingerprintDisabled(t *testing.T) {
	upstream := &accountTLSRecordingUpstream{}
	gateway := &OpenAIGatewayService{
		httpUpstream:        upstream,
		tlsFPProfileService: &TLSFingerprintProfileService{},
	}
	account := &Account{ID: 37, Platform: PlatformKimi, Type: AccountTypeAPIKey, Concurrency: 1}
	req, err := http.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions", strings.NewReader(`{}`))
	require.NoError(t, err)

	resp, err := gateway.doOpenAIUpstream(req, "", account)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Zero(t, upstream.tlsCalls)
	require.Equal(t, 1, upstream.plainCalls)
	require.Nil(t, upstream.profile)
}
