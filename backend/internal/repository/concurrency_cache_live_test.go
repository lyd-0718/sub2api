package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestLiveLeaseReplacesRegularSlotsAndCountsTowardLimits(t *testing.T) {
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	regular := NewConcurrencyCache(client, 15, 900)
	live, ok := regular.(service.LiveConcurrencyCache)
	require.True(t, ok)
	ctx := context.Background()

	accountAcquired, err := regular.AcquireAccountSlot(ctx, 10, 1, "regular-account")
	require.NoError(t, err)
	require.True(t, accountAcquired)
	userAcquired, err := regular.AcquireUserSlot(ctx, 20, 1, "regular-user")
	require.NoError(t, err)
	require.True(t, userAcquired)

	acquired, err := live.AcquireLiveLease(ctx, 10, 1, 20, 1, 30, 1, "live-lease", true)
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, regular.ReleaseAccountSlot(ctx, 10, "regular-account"))
	require.NoError(t, regular.ReleaseUserSlot(ctx, 20, "regular-user"))

	accountCount, err := regular.GetAccountConcurrency(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, accountCount)
	userCount, err := regular.GetUserConcurrency(ctx, 20)
	require.NoError(t, err)
	require.Equal(t, 1, userCount)
	accountAcquired, err = regular.AcquireAccountSlot(ctx, 10, 1, "ordinary-blocked")
	require.NoError(t, err)
	require.False(t, accountAcquired)

	refreshed, err := live.RefreshLiveLease(ctx, 10, 20, 30, "live-lease")
	require.NoError(t, err)
	require.True(t, refreshed)
	require.NoError(t, live.ReleaseLiveLease(ctx, 10, 20, 30, "live-lease"))
	accountAcquired, err = regular.AcquireAccountSlot(ctx, 10, 1, "ordinary-allowed")
	require.NoError(t, err)
	require.True(t, accountAcquired)
}

func TestLiveLeaseExpiresWithoutRefresh(t *testing.T) {
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	regular := NewConcurrencyCache(client, 15, 900)
	live, ok := regular.(service.LiveConcurrencyCache)
	require.True(t, ok)
	ctx := context.Background()

	acquired, err := live.AcquireLiveLease(ctx, 10, 1, 20, 1, 30, 1, "expired-live", false)
	require.NoError(t, err)
	require.True(t, acquired)

	redisServer.FastForward(61 * time.Second)
	acquired, err = regular.AcquireAccountSlot(ctx, 10, 1, "ordinary-after-expiry")
	require.NoError(t, err)
	require.True(t, acquired)
	refreshed, err := live.RefreshLiveLease(ctx, 10, 20, 30, "expired-live")
	require.NoError(t, err)
	require.False(t, refreshed)
}

func TestLiveLeaseAPIKeyLimitAcrossAccounts(t *testing.T) {
	for _, replacing := range []bool{false, true} {
		name := "independent"
		if replacing {
			name = "replacing_regular_slots"
		}
		t.Run(name, func(t *testing.T) {
			redisServer := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
			regular := NewConcurrencyCache(client, 15, 900)
			live, ok := regular.(service.LiveConcurrencyCache)
			require.True(t, ok)
			ctx := context.Background()

			acquired, err := live.AcquireLiveLease(ctx, 10, 0, 20, 0, 30, 1, "first-live", replacing)
			require.NoError(t, err)
			require.True(t, acquired)
			acquired, err = live.AcquireLiveLease(ctx, 11, 0, 21, 0, 30, 1, "second-live", replacing)
			require.NoError(t, err)
			require.False(t, acquired)

			accountCount, err := regular.GetAccountConcurrency(ctx, 11)
			require.NoError(t, err)
			require.Zero(t, accountCount)
			userCount, err := regular.GetUserConcurrency(ctx, 21)
			require.NoError(t, err)
			require.Zero(t, userCount)
			keyCounts, err := regular.(service.APIKeyConcurrencyCache).GetAPIKeyConcurrencyBatch(ctx, []int64{30})
			require.NoError(t, err)
			require.Equal(t, 1, keyCounts[30])

			require.NoError(t, live.ReleaseLiveLease(ctx, 10, 20, 30, "first-live"))
			acquired, err = live.AcquireLiveLease(ctx, 11, 0, 21, 0, 30, 1, "second-live", replacing)
			require.NoError(t, err)
			require.True(t, acquired)
		})
	}
}

func TestLiveLeaseAPIKeyLimitIncludesRegularSlots(t *testing.T) {
	for _, replacing := range []bool{false, true} {
		name := "independent"
		if replacing {
			name = "replacing_regular_slots"
		}
		t.Run(name, func(t *testing.T) {
			redisServer := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
			regular := NewConcurrencyCache(client, 15, 900)
			live, ok := regular.(service.LiveConcurrencyCache)
			require.True(t, ok)
			keyLimiter, ok := regular.(service.APIKeyConcurrencyLimiter)
			require.True(t, ok)
			ctx := context.Background()

			acquired, err := keyLimiter.AcquireAPIKeySlot(ctx, 30, 1, "ordinary")
			require.NoError(t, err)
			require.True(t, acquired)
			acquired, err = live.AcquireLiveLease(ctx, 10, 0, 20, 0, 30, 1, "live", replacing)
			require.NoError(t, err)
			require.False(t, acquired)
			accountCount, err := regular.GetAccountConcurrency(ctx, 10)
			require.NoError(t, err)
			require.Zero(t, accountCount)
			userCount, err := regular.GetUserConcurrency(ctx, 20)
			require.NoError(t, err)
			require.Zero(t, userCount)

			require.NoError(t, keyLimiter.ReleaseAPIKeySlot(ctx, 30, "ordinary"))
			acquired, err = live.AcquireLiveLease(ctx, 10, 0, 20, 0, 30, 1, "live", replacing)
			require.NoError(t, err)
			require.True(t, acquired)
			acquired, err = keyLimiter.AcquireAPIKeySlot(ctx, 30, 1, "ordinary-after-live")
			require.NoError(t, err)
			require.False(t, acquired)
			keyCounts, err := regular.(service.APIKeyConcurrencyCache).GetAPIKeyConcurrencyBatch(ctx, []int64{30})
			require.NoError(t, err)
			require.Equal(t, 1, keyCounts[30])

			require.NoError(t, live.ReleaseLiveLease(ctx, 10, 20, 30, "live"))
			acquired, err = keyLimiter.AcquireAPIKeySlot(ctx, 30, 1, "ordinary-after-live")
			require.NoError(t, err)
			require.True(t, acquired)
		})
	}
}

func TestLiveLeaseAPIKeyLimitAllowsReuseAfterExpiry(t *testing.T) {
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	regular := NewConcurrencyCache(client, 15, 900)
	live, ok := regular.(service.LiveConcurrencyCache)
	require.True(t, ok)
	keyLimiter, ok := regular.(service.APIKeyConcurrencyLimiter)
	require.True(t, ok)
	ctx := context.Background()

	acquired, err := live.AcquireLiveLease(ctx, 10, 0, 20, 0, 30, 1, "expired-live", false)
	require.NoError(t, err)
	require.True(t, acquired)
	redisServer.FastForward(61 * time.Second)

	acquired, err = keyLimiter.AcquireAPIKeySlot(ctx, 30, 1, "ordinary-after-expiry")
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, keyLimiter.ReleaseAPIKeySlot(ctx, 30, "ordinary-after-expiry"))
	acquired, err = live.AcquireLiveLease(ctx, 11, 0, 21, 0, 30, 1, "live-after-expiry", true)
	require.NoError(t, err)
	require.True(t, acquired)
	keyCounts, err := regular.(service.APIKeyConcurrencyCache).GetAPIKeyConcurrencyBatch(ctx, []int64{30})
	require.NoError(t, err)
	require.Equal(t, 1, keyCounts[30])
}

func TestLiveLeaseAPIKeyZeroCapRemainsUnlimited(t *testing.T) {
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	regular := NewConcurrencyCache(client, 15, 900)
	live, ok := regular.(service.LiveConcurrencyCache)
	require.True(t, ok)
	keyLimiter, ok := regular.(service.APIKeyConcurrencyLimiter)
	require.True(t, ok)
	ctx := context.Background()

	acquired, err := keyLimiter.AcquireAPIKeySlot(ctx, 30, 1, "ordinary")
	require.NoError(t, err)
	require.True(t, acquired)
	acquired, err = live.AcquireLiveLease(ctx, 10, 0, 20, 0, 30, 0, "first-live", true)
	require.NoError(t, err)
	require.True(t, acquired)
	acquired, err = live.AcquireLiveLease(ctx, 11, 0, 21, 0, 30, 0, "second-live", true)
	require.NoError(t, err)
	require.True(t, acquired)
	keyCounts, err := regular.(service.APIKeyConcurrencyCache).GetAPIKeyConcurrencyBatch(ctx, []int64{30})
	require.NoError(t, err)
	require.Equal(t, 3, keyCounts[30])
}

func TestLiveLeaseAPIKeyLimitAllowsReuseAfterRegularSlotExpiry(t *testing.T) {
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	regular := NewConcurrencyCache(client, 1, 900)
	live, ok := regular.(service.LiveConcurrencyCache)
	require.True(t, ok)
	keyLimiter, ok := regular.(service.APIKeyConcurrencyLimiter)
	require.True(t, ok)
	ctx := context.Background()

	acquired, err := keyLimiter.AcquireAPIKeySlot(ctx, 30, 1, "expired-ordinary")
	require.NoError(t, err)
	require.True(t, acquired)
	acquired, err = live.AcquireLiveLease(ctx, 10, 0, 20, 0, 30, 1, "live", true)
	require.NoError(t, err)
	require.False(t, acquired)
	redisServer.FastForward(61 * time.Second)

	acquired, err = live.AcquireLiveLease(ctx, 10, 0, 20, 0, 30, 1, "live", true)
	require.NoError(t, err)
	require.True(t, acquired)
	keyCounts, err := regular.(service.APIKeyConcurrencyCache).GetAPIKeyConcurrencyBatch(ctx, []int64{30})
	require.NoError(t, err)
	require.Equal(t, 1, keyCounts[30])
}
