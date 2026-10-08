package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthSnapshotConcurrencyRoundtrip(t *testing.T) {
	svc := &APIKeyService{}
	for _, cap := range []int{0, 4, 2147483647} {
		key := &APIKey{ID: 1, UserID: 7, Key: "sk-concurrency", Concurrency: cap, Status: StatusActive, User: &User{ID: 7, Status: StatusActive}}
		payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: svc.snapshotFromAPIKey(context.Background(), key)})
		require.NoError(t, err)
		var cached APIKeyAuthCacheEntry
		require.NoError(t, json.Unmarshal(payload, &cached))
		got, used, err := svc.applyAuthCacheEntry(key.Key, &cached)
		require.NoError(t, err)
		require.True(t, used)
		require.Equal(t, cap, got.Concurrency)
	}
}

func TestAPIKeyAuthSnapshotPreConcurrencyVersionRequiresReload(t *testing.T) {
	svc := &APIKeyService{}
	// v24 does not project the key cap; accepting it would silently allow unlimited requests.
	got, used, err := svc.applyAuthCacheEntry("sk-concurrency", &APIKeyAuthCacheEntry{Snapshot: &APIKeyAuthSnapshot{Version: 24, APIKeyID: 1, UserID: 7, User: APIKeyAuthUserSnapshot{ID: 7, Status: StatusActive}}})
	require.NoError(t, err)
	require.False(t, used)
	require.Nil(t, got)
}
