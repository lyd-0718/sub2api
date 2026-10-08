package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type openAIWSAuthRepoStub struct {
	service.APIKeyRepository
	apiKey *service.APIKey
	lookup func(*service.APIKey) (*service.APIKey, error)
}

func (s *openAIWSAuthRepoStub) GetByKeyForAuth(_ context.Context, key string) (*service.APIKey, error) {
	if key != s.apiKey.Key {
		return nil, service.ErrAPIKeyNotFound
	}
	snapshot := *s.apiKey
	if s.lookup != nil {
		return s.lookup(&snapshot)
	}
	return &snapshot, nil
}

func newOpenAIWSAuthService(apiKey *service.APIKey, lookup func(*service.APIKey) (*service.APIKey, error)) *service.APIKeyService {
	return service.NewAPIKeyService(&openAIWSAuthRepoStub{apiKey: apiKey, lookup: lookup}, nil, nil, nil, nil, nil, nil)
}

func TestOpenAIWSUserSlotRelease_NormalTurnKeepsSessionAlive(t *testing.T) {
	sessionCtx, cancelSession := context.WithCancelCause(context.Background())
	defer cancelSession(nil)
	for range 2 {
		leaseCtx, cancelLease := context.WithCancelCause(sessionCtx)
		var releases atomic.Int32
		release := openAIWSUserSlotRelease(sessionCtx, &service.AcquireResult{
			Acquired: true,
			Context:  leaseCtx,
			ReleaseFunc: func() {
				releases.Add(1)
				cancelLease(context.Canceled)
			},
		}, cancelSession)
		release()
		release()
		require.Equal(t, int32(1), releases.Load())
		require.NoError(t, sessionCtx.Err())
		require.ErrorIs(t, context.Cause(leaseCtx), context.Canceled)
	}
}

func TestOpenAIWSUserSlotRelease_LeaseLossCancelsSessionAndReleasesSlots(t *testing.T) {
	sessionCtx, cancelSession := context.WithCancelCause(context.Background())
	defer cancelSession(nil)
	leaseCtx, cancelLease := context.WithCancelCause(sessionCtx)
	var userReleases, accountReleases atomic.Int32
	releaseUser := openAIWSUserSlotRelease(sessionCtx, &service.AcquireResult{
		Acquired: true,
		Context:  leaseCtx,
		ReleaseFunc: func() {
			userReleases.Add(1)
		},
	}, cancelSession)
	releaseAccount := wrapReleaseOnDone(sessionCtx, func() { accountReleases.Add(1) })
	defer releaseUser()
	defer releaseAccount()
	cancelLease(service.ErrAPIKeyConcurrencyLeaseLost)
	require.Eventually(t, func() bool {
		return errors.Is(context.Cause(sessionCtx), service.ErrAPIKeyConcurrencyLeaseLost) &&
			userReleases.Load() == 1 && accountReleases.Load() == 1
	}, time.Second, time.Millisecond)
}

func TestOpenAIWSUserSlotRelease_ReleasedTurnCannotCancelLaterTurn(t *testing.T) {
	sessionCtx, cancelSession := context.WithCancelCause(context.Background())
	defer cancelSession(nil)
	leaseCtx, cancelLease := context.WithCancelCause(sessionCtx)
	release := openAIWSUserSlotRelease(sessionCtx, &service.AcquireResult{
		Acquired: true, Context: leaseCtx, ReleaseFunc: func() {},
	}, cancelSession)
	release()
	cancelLease(service.ErrAPIKeyConcurrencyLeaseLost)
	require.NoError(t, sessionCtx.Err())
}

func TestOpenAIResponsesWebSocket_SubsequentTurnAuthFailureRejects(t *testing.T) {
	runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:            `{"type":"response.create","model":"gpt-5.4"}`,
		secondPayload:           `{"type":"response.create","model":"gpt-5.4"}`,
		secondTurnCloseExpected: true,
		closeStatus:             coderws.StatusInternalError,
		closeReason:             "failed to refresh API key concurrency limit",
		authLookup: func(*service.APIKey) (*service.APIKey, error) {
			return nil, errors.New("auth backend unavailable")
		},
	})
}

func TestOpenAIResponsesWebSocket_SubsequentTurnKeyIdentityChangeRejects(t *testing.T) {
	for _, field := range []string{"key", "user"} {
		t.Run(field, func(t *testing.T) {
			runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
				firstPayload:            `{"type":"response.create","model":"gpt-5.4"}`,
				secondPayload:           `{"type":"response.create","model":"gpt-5.4"}`,
				secondTurnCloseExpected: true,
				closeStatus:             coderws.StatusPolicyViolation,
				closeReason:             "API key identity changed",
				authLookup: func(key *service.APIKey) (*service.APIKey, error) {
					if field == "key" {
						key.ID++
					} else {
						key.UserID++
					}
					return key, nil
				},
			})
		})
	}
}

type openAIWSKeyConcurrencyCache struct {
	*concurrencyCacheMock
	mu            sync.Mutex
	slots         map[string]struct{}
	externalSlots int
	caps          []int
	acquireErr    error
	ttl           time.Duration
	loseLease     atomic.Bool
}

func newOpenAIWSKeyConcurrencyCache() *openAIWSKeyConcurrencyCache {
	return &openAIWSKeyConcurrencyCache{
		concurrencyCacheMock: &concurrencyCacheMock{
			acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
			acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
		},
		slots: make(map[string]struct{}),
	}
}

func (s *openAIWSKeyConcurrencyCache) AcquireAPIKeySlot(_ context.Context, _ int64, max int, requestID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.caps = append(s.caps, max)
	if s.acquireErr != nil {
		return false, s.acquireErr
	}
	if len(s.slots)+s.externalSlots >= max {
		return false, nil
	}
	s.slots[requestID] = struct{}{}
	return true, nil
}

func (s *openAIWSKeyConcurrencyCache) RefreshAPIKeySlot(_ context.Context, _ int64, requestID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.slots[requestID]
	return exists && !s.loseLease.Load(), nil
}

func (s *openAIWSKeyConcurrencyCache) ReleaseAPIKeySlot(_ context.Context, _ int64, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.slots, requestID)
	return nil
}

func (s *openAIWSKeyConcurrencyCache) APIKeySlotTTL() time.Duration {
	if s.ttl > 0 {
		return s.ttl
	}
	return 15 * time.Minute
}

func (s *openAIWSKeyConcurrencyCache) activeSlots() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.slots)
}

func TestOpenAIResponsesWebSocket_FirstTurnKeyFullClosesBusy(t *testing.T) {
	cache := newOpenAIWSKeyConcurrencyCache()
	_, releaseHTTP := acquireOpenAIHTTPKeySlot(t, cache, 1)
	defer releaseHTTP()
	runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:            `{"type":"response.create","model":"gpt-5.4"}`,
		apiKeyConcurrency:       1,
		concurrencyCache:        cache,
		firstFrameCloseExpected: true,
		closeStatus:             coderws.StatusTryAgainLater,
		closeReason:             "too many concurrent requests",
	})
	require.Zero(t, atomic.LoadInt32(&cache.releaseUserCalled), "Key denial must occur before user acquisition")
}

func TestOpenAIResponsesWebSocket_FirstTurnKeyBackendFailureClosesInternalError(t *testing.T) {
	cache := newOpenAIWSKeyConcurrencyCache()
	cache.acquireErr = errors.New("Redis unavailable")
	runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:            `{"type":"response.create","model":"gpt-5.4"}`,
		apiKeyConcurrency:       1,
		concurrencyCache:        cache,
		firstFrameCloseExpected: true,
		closeStatus:             coderws.StatusInternalError,
		closeReason:             "failed to acquire user concurrency slot",
	})
}

func TestOpenAIResponsesWebSocket_LimitedTurnsReleaseAndReacquire(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeDedicated} {
		t.Run(mode, func(t *testing.T) {
			cache := newOpenAIWSKeyConcurrencyCache()
			runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
				firstPayload:      `{"type":"response.create","model":"gpt-5.4"}`,
				secondPayload:     `{"type":"response.create","model":"gpt-5.4"}`,
				ingressMode:       mode,
				apiKeyConcurrency: 1,
				concurrencyCache:  cache,
				beforeSecondTurn: func() {
					require.Eventually(t, func() bool {
						return cache.activeSlots() == 0 && atomic.LoadInt32(&cache.releaseUserCalled) == 1 &&
							atomic.LoadInt32(&cache.releaseAccountCalled) == 1
					}, time.Second, time.Millisecond, "idle connection must release every turn slot")
				},
			})
			require.Eventually(t, func() bool {
				return cache.activeSlots() == 0 && atomic.LoadInt32(&cache.releaseUserCalled) == 2 &&
					atomic.LoadInt32(&cache.releaseAccountCalled) == 2
			}, time.Second, time.Millisecond)
			cache.mu.Lock()
			caps := cache.caps
			cache.mu.Unlock()
			require.Equal(t, []int{1, 1}, caps)
		})
	}
}

func TestOpenAIResponsesWebSocket_LimitedKeyWithUnlimitedUserReleasesWhileIdle(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeDedicated} {
		t.Run(mode, func(t *testing.T) {
			cache := newOpenAIWSKeyConcurrencyCache()
			userConcurrency := 0
			runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
				firstPayload:      `{"type":"response.create","model":"gpt-5.4"}`,
				secondPayload:     `{"type":"response.create","model":"gpt-5.4"}`,
				ingressMode:       mode,
				apiKeyConcurrency: 1,
				userConcurrency:   &userConcurrency,
				concurrencyCache:  cache,
				beforeSecondTurn: func() {
					require.Eventually(t, func() bool {
						return cache.activeSlots() == 0 && atomic.LoadInt32(&cache.releaseAccountCalled) == 1
					}, time.Second, time.Millisecond, "Key lease must release even when the user slot is unlimited")
				},
			})
			require.Eventually(t, func() bool {
				return cache.activeSlots() == 0 && atomic.LoadInt32(&cache.releaseAccountCalled) == 2
			}, time.Second, time.Millisecond)
			require.Zero(t, atomic.LoadInt32(&cache.releaseUserCalled), "unlimited users own no user slot")
			cache.mu.Lock()
			caps := cache.caps
			cache.mu.Unlock()
			require.Equal(t, []int{1, 1}, caps)
		})
	}
}

func TestOpenAIResponsesWebSocket_LaterTurnAppliesNewAdminCap(t *testing.T) {
	for _, initialCap := range []int{0, 2} {
		t.Run(strconv.Itoa(initialCap), func(t *testing.T) {
			cache := newOpenAIWSKeyConcurrencyCache()
			runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
				firstPayload:            `{"type":"response.create","model":"gpt-5.4"}`,
				secondPayload:           `{"type":"response.create","model":"gpt-5.4"}`,
				apiKeyConcurrency:       initialCap,
				concurrencyCache:        cache,
				secondTurnCloseExpected: true,
				closeStatus:             coderws.StatusTryAgainLater,
				closeReason:             "too many concurrent requests",
				beforeSecondTurn: func() {
					require.Eventually(t, func() bool {
						return cache.activeSlots() == 0 && atomic.LoadInt32(&cache.releaseUserCalled) == 1
					}, time.Second, time.Millisecond)
					cache.mu.Lock()
					cache.externalSlots = 1
					cache.mu.Unlock()
				},
				authLookup: func(key *service.APIKey) (*service.APIKey, error) {
					key.Concurrency = 1
					return key, nil
				},
			})
			cache.mu.Lock()
			latestCap := cache.caps[len(cache.caps)-1]
			cache.mu.Unlock()
			require.Equal(t, 1, latestCap, "later turn must use the refreshed administrator cap")
		})
	}
}

func TestOpenAIResponsesWebSocket_LeaseLossAbortsActiveForwarding(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeDedicated} {
		t.Run(mode, func(t *testing.T) {
			cache := newOpenAIWSKeyConcurrencyCache()
			cache.ttl = 300 * time.Millisecond
			runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
				firstPayload:           `{"type":"response.create","model":"gpt-5.4"}`,
				ingressMode:            mode,
				apiKeyConcurrency:      1,
				concurrencyCache:       cache,
				firstTurnCloseExpected: true,
				afterFirstUpstreamRequest: func(*service.ChannelService) error {
					cache.loseLease.Store(true)
					return nil
				},
			})
			require.Eventually(t, func() bool {
				return cache.activeSlots() == 0 && atomic.LoadInt32(&cache.releaseUserCalled) == 1 &&
					atomic.LoadInt32(&cache.releaseAccountCalled) == 1
			}, time.Second, time.Millisecond, "lease loss must free Key, user and account slots")
		})
	}
}

func acquireOpenAIHTTPKeySlot(t *testing.T, cache service.ConcurrencyCache, max int) (*gin.Context, func()) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 1801, UserID: 1701, Concurrency: max})
	h := &OpenAIGatewayHandler{
		concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
	}
	streamStarted := false
	release, acquired := h.acquireResponsesUserSlot(c, 1701, 1, false, &streamStarted, zap.NewNop())
	require.True(t, acquired)
	require.NotNil(t, release)
	return c, release
}

func TestAcquireResponsesUserSlot_LeaseLossUsesUpdatedRequestContext(t *testing.T) {
	cache := newOpenAIWSKeyConcurrencyCache()
	cache.ttl = 300 * time.Millisecond
	c, release := acquireOpenAIHTTPKeySlot(t, cache, 1)
	defer release()
	cache.loseLease.Store(true)
	require.Eventually(t, func() bool {
		return errors.Is(context.Cause(c.Request.Context()), service.ErrAPIKeyConcurrencyLeaseLost) &&
			cache.activeSlots() == 0 && atomic.LoadInt32(&cache.releaseUserCalled) == 1
	}, time.Second, time.Millisecond, "HTTP wrapper must observe the leased request context")
}
