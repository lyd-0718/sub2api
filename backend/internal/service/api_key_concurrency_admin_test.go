//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestAdminUpdateAPIKey_ConcurrencyPartialUpdate(t *testing.T) {
	for _, cap := range []int{0, 3, 2147483647} {
		t.Run(strconv.Itoa(cap), func(t *testing.T) {
			now := time.Now()
			key := &APIKey{ID: 1, UserID: 7, Key: "sk-test", Concurrency: 9, GroupID: int64Ptr(5), QuotaUsed: 27, Usage5h: 12, Window5hStart: &now}
			repo := &apiKeyRepoStubForGroupUpdate{key: key}
			cache := &authCacheInvalidatorStub{}
			svc := &adminServiceImpl{apiKeyRepo: repo, authCacheInvalidator: cache}
			got, err := svc.AdminUpdateAPIKey(context.Background(), 1, AdminUpdateAPIKeyInput{Concurrency: &cap})
			require.NoError(t, err)
			require.Equal(t, cap, got.APIKey.Concurrency)
			require.Equal(t, key.GroupID, got.APIKey.GroupID)
			require.Equal(t, key.QuotaUsed, got.APIKey.QuotaUsed)
			require.Equal(t, key.Usage5h, got.APIKey.Usage5h)
			require.Equal(t, key.Window5hStart, got.APIKey.Window5hStart)
			require.Equal(t, APIKeyUpdateFields{Concurrency: true}, repo.fields)
			require.Equal(t, []string{key.Key}, cache.keys)
		})
	}
}

func TestAdminUpdateAPIKey_GroupOnlyPreservesConcurrency(t *testing.T) {
	repo := &apiKeyRepoStubForGroupUpdate{key: &APIKey{ID: 1, Key: "sk-test", Concurrency: 4, GroupID: int64Ptr(5)}}
	svc := &adminServiceImpl{apiKeyRepo: repo}
	got, err := svc.AdminUpdateAPIKey(context.Background(), 1, AdminUpdateAPIKeyInput{GroupID: int64Ptr(0)})
	require.NoError(t, err)
	require.Equal(t, 4, got.APIKey.Concurrency)
	require.Equal(t, APIKeyUpdateFields{GroupID: true}, repo.fields)
}

func TestAdminUpdateAPIKey_CombinedExclusiveGroupAndCap(t *testing.T) {
	cap := 2
	repo := &apiKeyRepoStubForGroupUpdate{key: &APIKey{ID: 1, UserID: 7, Key: "sk-test", Concurrency: 4}}
	users := &userRepoStubForGroupUpdate{}
	cache := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{
		apiKeyRepo: repo, userRepo: users, authCacheInvalidator: cache,
		groupRepo: &groupRepoStubForGroupUpdate{group: &Group{ID: 5, Name: "exclusive", Status: StatusActive, IsExclusive: true}},
	}
	got, err := svc.AdminUpdateAPIKey(context.Background(), 1, AdminUpdateAPIKeyInput{GroupID: int64Ptr(5), Concurrency: &cap})
	require.NoError(t, err)
	require.True(t, users.addGroupCalled)
	require.True(t, got.AutoGrantedGroupAccess)
	require.Equal(t, int64Ptr(5), got.GrantedGroupID)
	require.Equal(t, "exclusive", got.GrantedGroupName)
	require.Equal(t, 2, got.APIKey.Concurrency)
	require.Equal(t, APIKeyUpdateFields{GroupID: true, Concurrency: true}, repo.fields)
	require.Equal(t, []string{"sk-test"}, cache.keys)
}

func TestAdminUpdateAPIKey_InvalidConcurrencyDoesNotWriteOrGrant(t *testing.T) {
	for _, cap := range []int{-1, 2147483648} {
		repo := &apiKeyRepoStubForGroupUpdate{key: &APIKey{ID: 1, Key: "sk-test", Concurrency: 4, Usage5h: 12}}
		users := &userRepoStubForGroupUpdate{}
		cache := &authCacheInvalidatorStub{}
		svc := &adminServiceImpl{apiKeyRepo: repo, userRepo: users, authCacheInvalidator: cache}
		_, err := svc.AdminUpdateAPIKey(context.Background(), 1, AdminUpdateAPIKeyInput{GroupID: int64Ptr(5), Concurrency: &cap, ResetRateLimitUsage: true})
		require.Equal(t, "INVALID_CONCURRENCY", infraerrors.Reason(err))
		require.Nil(t, repo.updated)
		require.False(t, users.addGroupCalled)
		require.Empty(t, cache.keys)
		require.Equal(t, 4, repo.key.Concurrency)
		require.Equal(t, 12.0, repo.key.Usage5h)
	}
}

func TestAdminUpdateAPIKey_InvalidGroupDoesNotResetUsageOrCap(t *testing.T) {
	cap := 2
	repo := &apiKeyRepoStubForGroupUpdate{key: &APIKey{ID: 1, Key: "sk-test", Concurrency: 4, Usage5h: 12}}
	cache := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{apiKeyRepo: repo, authCacheInvalidator: cache, groupRepo: &groupRepoStubForGroupUpdate{group: &Group{ID: 5, Status: StatusDisabled}}}
	_, err := svc.AdminUpdateAPIKey(context.Background(), 1, AdminUpdateAPIKeyInput{GroupID: int64Ptr(5), Concurrency: &cap, ResetRateLimitUsage: true})
	require.Equal(t, "GROUP_NOT_ACTIVE", infraerrors.Reason(err))
	require.Nil(t, repo.updated)
	require.Empty(t, cache.keys)
	require.Equal(t, 4, repo.key.Concurrency)
	require.Equal(t, 12.0, repo.key.Usage5h)
}

func TestAdminUpdateAPIKey_FailedCapWriteDoesNotInvalidate(t *testing.T) {
	cap := 0
	repo := &apiKeyRepoStubForGroupUpdate{key: &APIKey{ID: 1, Key: "sk-test", Concurrency: 4}, updateErr: errors.New("write failed")}
	cache := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{apiKeyRepo: repo, authCacheInvalidator: cache}
	_, err := svc.AdminUpdateAPIKey(context.Background(), 1, AdminUpdateAPIKeyInput{Concurrency: &cap})
	require.Error(t, err)
	require.Empty(t, cache.keys)
}

func TestAPIKeyOwnerUpdateCannotOverwriteAdminConcurrency(t *testing.T) {
	for _, payload := range []string{`{"name":"renamed","concurrency":0}`, `{"name":"renamed","concurrency":99}`} {
		var req UpdateAPIKeyRequest
		require.NoError(t, json.Unmarshal([]byte(payload), &req))
		svc, repo := newUpdateFieldsAPIKeyService(&APIKey{ID: 1, UserID: 7, Key: "sk-test", Status: StatusActive, Concurrency: 4})
		got, err := svc.Update(context.Background(), 1, 7, req)
		require.NoError(t, err)
		require.Equal(t, 4, got.Concurrency)
		require.Equal(t, []APIKeyUpdateFields{{Name: true}}, repo.updateFields)
	}
}

func TestAPIKeyOwnerCreateCannotSetAdminConcurrency(t *testing.T) {
	var req CreateAPIKeyRequest
	require.NoError(t, json.Unmarshal([]byte(`{"name":"key","concurrency":99}`), &req))
	repo, cache := newCreateLimitStubs()
	svc := newCreateLimitService(repo, cache, 0, 0)
	got, err := svc.Create(context.Background(), 7, req)
	require.NoError(t, err)
	require.Zero(t, got.Concurrency)
	require.Len(t, repo.created, 1)
	require.Zero(t, repo.created[0].Concurrency)
}

func TestAdminUpdateAPIKey_CombinedResetWritesOnlyRequestedColumns(t *testing.T) {
	now := time.Now()
	cap := 2
	repo := &apiKeyRepoStubForGroupUpdate{key: &APIKey{
		ID: 1, Key: "sk-test", Concurrency: 4, GroupID: int64Ptr(5), QuotaUsed: 27,
		Usage5h: 12, Usage1d: 13, Usage7d: 14, Window5hStart: &now, Window1dStart: &now, Window7dStart: &now,
	}}
	cache := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{apiKeyRepo: repo, authCacheInvalidator: cache}
	got, err := svc.AdminUpdateAPIKey(context.Background(), 1, AdminUpdateAPIKeyInput{GroupID: int64Ptr(0), Concurrency: &cap, ResetRateLimitUsage: true})
	require.NoError(t, err)
	require.Equal(t, APIKeyUpdateFields{GroupID: true, Concurrency: true, RateLimitUsage: true}, repo.fields)
	require.Equal(t, 2, got.APIKey.Concurrency)
	require.Nil(t, got.APIKey.GroupID)
	require.Equal(t, 27.0, got.APIKey.QuotaUsed)
	require.Zero(t, got.APIKey.Usage5h)
	require.Zero(t, got.APIKey.Usage1d)
	require.Zero(t, got.APIKey.Usage7d)
	require.Nil(t, got.APIKey.Window5hStart)
	require.Nil(t, got.APIKey.Window1dStart)
	require.Nil(t, got.APIKey.Window7dStart)
	require.Equal(t, []string{"sk-test"}, cache.keys)
}
