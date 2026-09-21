package tlsfingerprint

import (
	"crypto/md5" // #nosec G501 -- JA3 is an interoperability fingerprint, not cryptography.
	"encoding/hex"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const capturedBusyBoxWgetJA3 = "771,4866-4867-4865-49196-49200-159-52393-52392-52394-49195-49199-158-49188-49192-107-49187-49191-103-49162-49172-57-49161-49171-51-157-156-61-60-53-47-255,0-11-10-35-22-23-13-43-45-51,29-23-30-25-24-256-257-258-259-260,0-1-2"

func TestBuiltinBusyBoxWgetProfileMatchesProductionCapture(t *testing.T) {
	profile := BuiltinBusyBoxWgetProfile()
	spec := buildClientHelloSpecFromProfile(profile)

	require.Equal(t, capturedBusyBoxWgetJA3, ja3StringForProfile(profile))
	sum := md5.Sum([]byte(capturedBusyBoxWgetJA3)) // #nosec G401 -- required by the JA3 standard.
	require.Equal(t, "a3afc2c46ba4a7d7fbe1cfb7a3031c2f", hex.EncodeToString(sum[:]))
	require.Equal(t, profile.CipherSuites, spec.CipherSuites)
	require.Len(t, spec.Extensions, len(profile.Extensions))
}

func ja3StringForProfile(profile *Profile) string {
	return strings.Join([]string{
		"771",
		joinUint16(profile.CipherSuites),
		joinUint16(profile.Extensions),
		joinUint16(profile.Curves),
		joinUint16(profile.PointFormats),
	}, ",")
}

func joinUint16(values []uint16) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.FormatUint(uint64(value), 10)
	}
	return strings.Join(parts, "-")
}
