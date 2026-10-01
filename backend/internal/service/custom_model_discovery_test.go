package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppendCustomCodexModelsPreservesNativeMetadata(t *testing.T) {
	body := []byte(`{"models":[{"slug":"native-model","display_name":"Native","context_window":12345,"vendor":{"key":true}}],"native_envelope":true}`)
	merged, err := AppendCustomCodexModelsManifest(body, []string{"native-model", "public-custom", "public-custom"})
	require.NoError(t, err)
	var result struct {
		Models         []json.RawMessage `json:"models"`
		NativeEnvelope bool              `json:"native_envelope"`
	}
	require.NoError(t, json.Unmarshal(merged, &result))
	require.True(t, result.NativeEnvelope)
	require.Len(t, result.Models, 2)
	require.JSONEq(t, `{"slug":"native-model","display_name":"Native","context_window":12345,"vendor":{"key":true}}`, string(result.Models[0]))
	var custom map[string]any
	require.NoError(t, json.Unmarshal(result.Models[1], &custom))
	require.Equal(t, "public-custom", custom["slug"])
	require.Equal(t, "public-custom", custom["display_name"])
	require.Equal(t, "list", custom["visibility"])
	require.Equal(t, true, custom["supported_in_api"])
}
