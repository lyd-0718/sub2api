package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func newOpenRouterPlatformAccount(id int64, priority int, mapping map[string]any) Account {
	return Account{
		ID:          id,
		Name:        "openrouter",
		Platform:    PlatformOpenRouter,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 5,
		Priority:    priority,
		Credentials: map[string]any{
			"api_key":       "sk-or-test",
			"api_protocol":  APIProtocolAdaptive,
			"account_mode":  AccountModePayG,
			"model_mapping": mapping,
		},
	}
}

func TestOpenRouterPlatformAccountDefaults(t *testing.T) {
	account := newOpenRouterPlatformAccount(1, 1, map[string]any{"z-ai/glm-5.3": "z-ai/glm-5.3"})

	require.True(t, account.IsOpenRouter())
	require.True(t, account.IsMultiProtocolAPIKey())
	require.True(t, account.IsOpenAICompatible())
	require.False(t, account.IsCNProvider(), "OpenRouter must not inherit CN coding-plan / 403 classifier logic")
	require.True(t, account.IsHeaderOverrideEligible())

	require.Equal(t, APIProtocolAdaptive, account.GetAPIProtocol())
	require.Equal(t, DefaultOpenRouterBaseURL, account.GetCNProtocolBaseURL(APIProtocolChatCompletions))
	require.Equal(t, DefaultOpenRouterBaseURL, account.GetCNProtocolBaseURL(APIProtocolResponses))
	require.Equal(t, DefaultOpenRouterAnthropicBaseURL, account.GetCNProtocolBaseURL(APIProtocolAnthropic))
	require.Equal(t, DefaultOpenRouterBaseURL, account.GetOpenAIBaseURL())
	require.Equal(t, DefaultOpenRouterAnthropicBaseURL, account.GetAnthropicProtocolBaseURL())
	require.True(t, account.UsesNativeCNResponses())

	account.Credentials["api_protocol"] = APIProtocolResponses
	require.Equal(t, APIProtocolResponses, account.GetAPIProtocol(), "OpenRouter serves native /responses")

	account.Credentials["account_mode"] = AccountModeCoding
	require.Equal(t, AccountModePayG, account.GetAccountMode(), "OpenRouter is pay-as-you-go only")
	require.False(t, account.IsCodingPlan())

	require.True(t, isOpenRouterBalanceAccount(&account))
	require.Equal(t, "https://openrouter.ai/api/v1/credits", cnBalanceURL(&account))
	require.NoError(t, validatePayGAccount(&account))
	require.True(t, isOpenRouterAccount(&account))
}

func TestCompositeOwnershipSharesModelsBetweenOpenRouterAndOnePlatform(t *testing.T) {
	groupID := int64(7)
	repo := &compositeOwnershipAccountRepo{accounts: []Account{
		{ID: 1, Platform: PlatformDeepseek, Credentials: map[string]any{"model_mapping": map[string]any{
			"deepseek/deepseek-v4.1-flash": "deepseek-v4.1-flash",
			"three-way":                    "deepseek-v4.1-flash",
		}}},
		newOpenRouterPlatformAccount(2, 50, map[string]any{
			"deepseek/deepseek-v4.1-flash": "deepseek/deepseek-v4.1-flash",
			"z-ai/glm-5.3":                 "z-ai/glm-5.3",
			"three-way":                    "deepseek/deepseek-v4.1-flash",
		}),
		{ID: 3, Platform: PlatformOpenAI, Credentials: map[string]any{"model_mapping": map[string]any{"three-way": "gpt-5"}}},
	}}
	svc := &GatewayService{accountRepo: repo}
	ctx := context.Background()

	shared, err := svc.resolveCompositeModelOwnership(ctx, groupID, "deepseek/deepseek-v4.1-flash")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{TargetPlatform: PlatformDeepseek, Matched: true}, shared,
		"a model offered by OpenRouter and one other platform belongs to that platform (OpenRouter joins its pool)")

	openRouterOnly, err := svc.resolveCompositeModelOwnership(ctx, groupID, "z-ai/glm-5.3")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{TargetPlatform: PlatformOpenRouter, Matched: true}, openRouterOnly)

	ambiguous, err := svc.resolveCompositeModelOwnership(ctx, groupID, "three-way")
	require.NoError(t, err)
	require.True(t, ambiguous.Ambiguous, "two non-OpenRouter platforms still conflict")
}

func TestOpenAIAccountPlatformMatchesOpenRouterPool(t *testing.T) {
	openRouter := newOpenRouterPlatformAccount(2, 50, map[string]any{"deepseek/deepseek-v4.1-flash": "deepseek/deepseek-v4.1-flash"})
	deepseek := Account{ID: 1, Platform: PlatformDeepseek}
	model := "deepseek/deepseek-v4.1-flash"
	composite := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{
		Matched: true, TargetPlatform: PlatformDeepseek, PublicModel: model, UpstreamModel: model,
	})

	require.True(t, openAIAccountPlatformMatches(composite, &deepseek, PlatformDeepseek, model))
	require.True(t, openAIAccountPlatformMatches(composite, &openRouter, PlatformDeepseek, model))
	require.True(t, openAIAccountPlatformMatches(composite, &openRouter, PlatformOpenRouter, model))

	require.False(t, openAIAccountPlatformMatches(context.Background(), &openRouter, PlatformDeepseek, model),
		"outside composite groups platforms stay isolated")
	require.False(t, openAIAccountPlatformMatches(composite, &openRouter, PlatformDeepseek, "deepseek-other"),
		"OpenRouter joins only for models it explicitly maps")
	require.False(t, openAIAccountPlatformMatches(composite, &deepseek, PlatformOpenRouter, model))
}

func TestOpenRouterJoinsCompositePlatformPoolForFailover(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(12)
	model := "deepseek/deepseek-v4.1-flash"
	accounts := []Account{
		{
			ID: 34, Name: "sota-deepseek", Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, Concurrency: 5, Priority: 1,
			Credentials: map[string]any{"api_key": "sk-test", "model_mapping": map[string]any{model: "deepseek-v4.1-flash"}},
		},
		newOpenRouterPlatformAccount(22, 50, map[string]any{model: model}),
		newOpenRouterPlatformAccount(38, 50, map[string]any{"z-ai/glm-5.3": "z-ai/glm-5.3"}),
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	composite := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{
		Matched: true, TargetPlatform: PlatformDeepseek, PublicModel: model, UpstreamModel: model,
	})
	selectFor := func(ctx context.Context, excluded map[int64]struct{}) (*AccountSelectionResult, error) {
		selection, _, err := svc.selectAccountWithScheduler(ctx, &groupID, "", "", model, excluded,
			OpenAIUpstreamTransportAny, "", "", false, PlatformDeepseek, false, true)
		return selection, err
	}

	selection, err := selectFor(composite, nil)
	require.NoError(t, err)
	require.Equal(t, int64(34), selection.Account.ID, "higher-priority DeepSeek relay is still preferred")

	selection, err = selectFor(composite, map[int64]struct{}{34: {}})
	require.NoError(t, err)
	require.Equal(t, int64(22), selection.Account.ID, "OpenRouter account takes over when the DeepSeek relay is unavailable")

	_, err = selectFor(composite, map[int64]struct{}{34: {}, 22: {}})
	require.Error(t, err, "an OpenRouter account that does not map the model never joins the pool")

	_, err = selectFor(context.Background(), map[int64]struct{}{34: {}})
	require.Error(t, err, "non-composite deepseek groups do not pull in OpenRouter accounts")
}
