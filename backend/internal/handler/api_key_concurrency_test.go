package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newKeyLimitHelper(t *testing.T) (*ConcurrencyHelper, service.ConcurrencyCache, *gin.Context) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := repository.NewConcurrencyCache(client, 15, 900)
	helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 16, Concurrency: 1})
	return helper, cache, c
}

func TestKeyCapRejectsBeforeUserWaitQueue(t *testing.T) {
	helper, cache, c := newKeyLimitHelper(t)
	ctx := context.Background()
	limiter := cache.(service.APIKeyConcurrencyLimiter)
	keyAcquired, err := limiter.AcquireAPIKeySlot(ctx, 16, 1, "existing-key-request")
	require.NoError(t, err)
	require.True(t, keyAcquired)
	userAcquired, err := cache.AcquireUserSlot(ctx, 7, 1, "existing-user-request")
	require.NoError(t, err)
	require.True(t, userAcquired)

	started := false
	release, err := helper.acquireUserSlotWithWaitTimeout(c, 7, 1, 5*time.Millisecond, true, &started)
	require.Nil(t, release)
	var full *ConcurrencyError
	require.ErrorAs(t, err, &full)
	require.Equal(t, "api_key", full.SlotType)
	require.False(t, full.IsTimeout)
	require.False(t, started, "key rejection must not begin a streaming response")
	status, errType, code, _ := concurrencyErrorResponse(err, "user")
	require.Equal(t, http.StatusTooManyRequests, status)
	require.Equal(t, "rate_limit_error", errType)
	require.Equal(t, gatewayConcurrencyLimitCode, code)
	load, err := cache.GetUsersLoadBatch(ctx, []service.UserWithConcurrency{{ID: 7, MaxConcurrency: 1}})
	require.NoError(t, err)
	require.Zero(t, load[7].WaitingCount)
	require.Equal(t, 1, load[7].CurrentConcurrency)
}

func TestKeySlotIsReleasedWhenUserAdmissionTimesOut(t *testing.T) {
	helper, cache, c := newKeyLimitHelper(t)
	ctx := context.Background()
	userAcquired, err := cache.AcquireUserSlot(ctx, 7, 1, "other-key-user-request")
	require.NoError(t, err)
	require.True(t, userAcquired)
	started := false
	release, err := helper.acquireUserSlotWithWaitTimeout(c, 7, 1, 5*time.Millisecond, false, &started)
	require.Nil(t, release)
	var full *ConcurrencyError
	require.ErrorAs(t, err, &full)
	require.Equal(t, "user", full.SlotType)
	require.True(t, full.IsTimeout)
	counts, err := cache.(service.APIKeyConcurrencyCache).GetAPIKeyConcurrencyBatch(ctx, []int64{16})
	require.NoError(t, err)
	require.Zero(t, counts[16], "failed user admission must not strand the key's only slot")
	userCount, err := cache.GetUserConcurrency(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, 1, userCount, "another request's user slot must remain owned")
	require.NoError(t, cache.ReleaseUserSlot(ctx, 7, "other-key-user-request"))

	fresh, _ := gin.CreateTestContext(httptest.NewRecorder())
	fresh.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	fresh.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 16, Concurrency: 1})
	release, err = helper.acquireUserSlotWithWaitTimeout(fresh, 7, 1, time.Second, false, &started)
	require.NoError(t, err)
	release()
	counts, err = cache.(service.APIKeyConcurrencyCache).GetAPIKeyConcurrencyBatch(ctx, []int64{16})
	require.NoError(t, err)
	require.Zero(t, counts[16])
}

func TestConfiguredKeyCapFailsClosedWithoutLimiter(t *testing.T) {
	cache := &concurrencyCacheMock{
		acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) {
			t.Fatal("key capacity must be checked before acquiring user capacity")
			return false, nil
		},
	}
	helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 16, Concurrency: 1})
	started := false
	release, err := helper.AcquireUserSlotWithWait(c, 7, 1, false, &started)
	require.Nil(t, release)
	require.Error(t, err)
	var full *ConcurrencyError
	require.False(t, errors.As(err, &full), "missing admission backend is not a user quota violation")
	status, _, _, _ := concurrencyErrorResponse(err, "user")
	require.Equal(t, http.StatusServiceUnavailable, status)
}
