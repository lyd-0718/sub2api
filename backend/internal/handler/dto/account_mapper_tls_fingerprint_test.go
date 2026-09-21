package dto

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountFromServiceShallowIncludesTLSFingerprintForNonAnthropicAccount(t *testing.T) {
	account := &service.Account{
		ID:       36,
		Name:     "Kimi relay",
		Platform: service.PlatformKimi,
		Type:     service.AccountTypeAPIKey,
		Extra: map[string]any{
			"enable_tls_fingerprint":     true,
			"tls_fingerprint_profile_id": float64(42),
		},
	}

	mapped := AccountFromServiceShallow(account)

	require.NotNil(t, mapped.EnableTLSFingerprint)
	require.True(t, *mapped.EnableTLSFingerprint)
	require.NotNil(t, mapped.TLSFingerprintProfileID)
	require.Equal(t, int64(42), *mapped.TLSFingerprintProfileID)
}
