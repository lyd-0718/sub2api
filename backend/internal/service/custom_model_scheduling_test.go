package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

type customRoutingAccountRepo struct {
	groupAwareStubOpenAIAccountRepo
}

func (r customRoutingAccountRepo) ListSchedulableByGroupIDAndPlatforms(ctx context.Context, groupID int64, platforms []string) ([]Account, error) {
	var accounts []Account
	for _, platform := range platforms {
		selected, err := r.ListSchedulableByGroupIDAndPlatform(ctx, groupID, platform)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, selected...)
	}
	return accounts, nil
}

type customRoutingCache struct {
	stubGatewayCache
}

func (c *customRoutingCache) GetSessionAccountID(ctx context.Context, groupID int64, session string) (int64, error) {
	return c.stubGatewayCache.GetSessionAccountID(ctx, groupID, fmt.Sprintf("%d:%s", groupID, session))
}

func (c *customRoutingCache) SetSessionAccountID(ctx context.Context, groupID int64, session string, accountID int64, ttl time.Duration) error {
	return c.stubGatewayCache.SetSessionAccountID(ctx, groupID, fmt.Sprintf("%d:%s", groupID, session), accountID, ttl)
}

func (c *customRoutingCache) DeleteSessionAccountID(ctx context.Context, groupID int64, session string) error {
	return c.stubGatewayCache.DeleteSessionAccountID(ctx, groupID, fmt.Sprintf("%d:%s", groupID, session))
}

func customRoutingFixture(platform string) (context.Context, *APIKey, *Group, customRoutingAccountRepo) {
	downstream := &Group{ID: 9101, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true, RateMultiplier: 2}
	upstream := &Group{ID: 9102, Platform: platform, Status: StatusActive, Hydrated: true, RateMultiplier: 0.5}
	key := &APIKey{ID: 9100, GroupID: &downstream.ID, Group: downstream}
	ctx := context.WithValue(context.Background(), ctxkey.Group, downstream)
	ctx = WithCustomModelResolution(ctx, &CustomModelResolution{
		ModelID: "custom-public", UpstreamGroupID: upstream.ID, UpstreamGroup: upstream, UpstreamModel: "model-upstream",
	}, downstream.ID)
	accounts := []Account{
		{ID: 1, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{downstream.ID}, AccountGroups: []AccountGroup{{GroupID: downstream.ID}}},
		{ID: 2, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 10, GroupIDs: []int64{upstream.ID}, AccountGroups: []AccountGroup{{GroupID: upstream.ID}}, Extra: map[string]any{"privacy_mode": PrivacyModeTrainingOff}},
		{ID: 3, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{9103}, AccountGroups: []AccountGroup{{GroupID: 9103}}},
	}
	return ctx, key, upstream, customRoutingAccountRepo{groupAwareStubOpenAIAccountRepo{stubOpenAIAccountRepo{accounts: accounts}}}
}

func TestCustomModelRoutingOpenAISelectionPreservesDownstreamIdentity(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformKimi} {
		for _, path := range []string{"legacy", "token_count", "load", "scheduler_legacy", "scheduler_advanced"} {
			t.Run(platform+"/"+path, func(t *testing.T) {
				resetOpenAIAdvancedSchedulerSettingCacheForTest()
				ctx, key, upstream, repo := customRoutingFixture(platform)
				downstream := key.Group
				cache := &customRoutingCache{}
				svc := &OpenAIGatewayService{accountRepo: repo, cache: cache, cfg: &config.Config{RunMode: config.RunModeSimple}}
				if path == "scheduler_advanced" {
					svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true")
				}
				// A stale upstream binding to a downstream account must never escape
				// group membership checks, even under simple-mode scheduling.
				require.NoError(t, cache.SetSessionAccountID(ctx, upstream.ID, "openai:session", 1, time.Hour))
				require.NoError(t, cache.SetSessionAccountID(ctx, downstream.ID, "openai:session", 1, time.Hour))
				var selected *Account
				var err error
				switch path {
				case "legacy":
					selected, err = svc.SelectAccountForModelWithExclusions(ctx, key.GroupID, "session", "model-upstream", nil)
				case "token_count":
					selected, err = svc.SelectAccountForTokenCount(ctx, key.GroupID, "session", "model-upstream", "", PlatformAnthropic)
				case "load":
					var result *AccountSelectionResult
					result, err = svc.SelectAccountWithLoadAwareness(ctx, key.GroupID, "session", "model-upstream", nil)
					if result != nil {
						selected = result.Account
						if result.ReleaseFunc != nil {
							result.ReleaseFunc()
						}
					}
				default:
					var result *AccountSelectionResult
					result, _, err = svc.SelectAccountWithSchedulerForCapability(ctx, key.GroupID, "", "session", "model-upstream", nil, OpenAIUpstreamTransportAny, "", false, false, false, PlatformAnthropic)
					if result != nil {
						selected = result.Account
						if result.ReleaseFunc != nil {
							result.ReleaseFunc()
						}
					}
				}
				require.NoError(t, err)
				require.NotNil(t, selected)
				require.Equal(t, int64(2), selected.ID)
				require.Same(t, downstream, key.Group)
				require.Equal(t, downstream.ID, *key.GroupID)
				require.Same(t, downstream, ctx.Value(ctxkey.Group))
				require.Equal(t, PlatformAnthropic, QuotaPlatform(ctx, key))
				require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, key.GroupID, "session", selected.ID))
				bound, err := cache.GetSessionAccountID(ctx, upstream.ID, "openai:session")
				require.NoError(t, err)
				require.Equal(t, int64(2), bound)
				bound, err = cache.GetSessionAccountID(ctx, downstream.ID, "openai:session")
				require.NoError(t, err)
				require.Equal(t, int64(1), bound)
			})
		}
	}
}

func TestCustomModelRoutingOpenAIPrivacyAndExplicitFallback(t *testing.T) {
	ctx, key, upstream, repo := customRoutingFixture(PlatformOpenAI)
	upstream.RequirePrivacySet = true
	svc := &OpenAIGatewayService{accountRepo: repo, cache: &customRoutingCache{}}
	selected, err := svc.SelectAccountForTokenCount(ctx, key.GroupID, "", "model-upstream", "", PlatformOpenAI)
	require.NoError(t, err)
	require.Equal(t, int64(2), selected.ID)
	repo.accounts[1].Extra = nil
	_, err = svc.SelectAccountForTokenCount(ctx, key.GroupID, "", "model-upstream", "", PlatformOpenAI)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	fallbackID := int64(9103)
	selected, err = svc.SelectAccountForModelWithExclusions(ctx, &fallbackID, "", "model-upstream", nil)
	require.NoError(t, err)
	require.Equal(t, int64(3), selected.ID)
	require.Equal(t, int64(9101), *key.GroupID)
}

func TestCustomModelRoutingSharedGatewayAndGemini(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformGemini} {
		for _, load := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/load=%t", platform, load), func(t *testing.T) {
				ctx, key, _, repo := customRoutingFixture(platform)
				// Explicit route prefixes must not force the downstream protocol pool.
				ctx = context.WithValue(ctx, ctxkey.ForcePlatform, PlatformOpenAI)
				svc := &GatewayService{accountRepo: repo, cache: &customRoutingCache{}, cfg: &config.Config{RunMode: config.RunModeSimple}}
				var selected *Account
				var err error
				if load {
					result, selectErr := svc.SelectAccountWithLoadAwareness(ctx, key.GroupID, "", "model-upstream", nil, "", 0)
					err = selectErr
					if result != nil {
						selected = result.Account
						if result.ReleaseFunc != nil {
							result.ReleaseFunc()
						}
					}
				} else {
					selected, err = svc.SelectAccountForModelWithExclusions(ctx, key.GroupID, "", "model-upstream", nil)
				}
				require.NoError(t, err)
				require.NotNil(t, selected)
				require.Equal(t, int64(2), selected.ID)
				require.Same(t, key.Group, ctx.Value(ctxkey.Group))
			})
		}
	}
	ctx, key, upstream, repo := customRoutingFixture(PlatformGemini)
	cache := &customRoutingCache{}
	require.NoError(t, cache.SetSessionAccountID(ctx, upstream.ID, "gemini:session", 1, time.Hour))
	svc := &GeminiMessagesCompatService{accountRepo: repo, cache: cache}
	selected, err := svc.SelectAccountForModelWithExclusions(ctx, key.GroupID, "session", "model-upstream", nil)
	require.NoError(t, err)
	require.NotNil(t, selected)
	require.Equal(t, int64(2), selected.ID)
	require.Same(t, key.Group, ctx.Value(ctxkey.Group))
}

func TestCustomModelRoutingPreviousResponseUsesUpstreamNamespace(t *testing.T) {
	ctx, key, upstream, repo := customRoutingFixture(PlatformOpenAI)
	cache := &customRoutingCache{}
	store := NewOpenAIWSStateStore(cache)
	svc := &OpenAIGatewayService{
		accountRepo: repo, cache: cache, cfg: newOpenAIWSV2TestConfig(), openaiWSStateStore: store,
	}
	require.NoError(t, store.BindResponseAccount(ctx, *key.GroupID, "resp_custom", 1, time.Hour))
	require.NoError(t, store.BindResponseAccount(ctx, upstream.ID, "resp_custom", 2, time.Hour))
	selection, err := svc.SelectAccountByPreviousResponseID(ctx, key.GroupID, "resp_custom", "model-upstream", nil, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(2), selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	require.Equal(t, int64(9101), *key.GroupID)
	require.NoError(t, store.BindResponseAccount(ctx, upstream.ID, "resp_custom", 1, time.Hour))
	selection, err = svc.SelectAccountByPreviousResponseID(ctx, key.GroupID, "resp_custom", "model-upstream", nil, false)
	require.NoError(t, err)
	require.Nil(t, selection, "a stale upstream binding must not select a downstream account")
}

func TestCustomModelRoutingCompositeUpstreamUsesResolvedPlatform(t *testing.T) {
	ctx, key, upstream, repo := customRoutingFixture(PlatformKimi)
	upstream.Platform = PlatformComposite
	svc := &OpenAIGatewayService{accountRepo: repo, cache: &customRoutingCache{}}
	account, err := svc.SelectAccountForModelWithExclusions(ctx, key.GroupID, "", "model-upstream", nil)
	require.NoError(t, err)
	require.Equal(t, int64(2), account.ID)
	require.Equal(t, PlatformKimi, account.Platform)
	publicModel, ok := RequestedPublicModelFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, "custom-public", publicModel)
	require.Equal(t, PlatformAnthropic, key.Group.Platform)
}
