//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

// cn403RepoStub 只实现本文件断言到的账号仓库方法：其余方法借用嵌入接口
// （不会被这些用例调用），从而不受其它 slice 正在补充的接口方法影响。
type cn403RepoStub struct {
	AccountRepository
	setErrorCalls       int
	tempCalls           int
	lastTempReason      string
	tempUntil           time.Time
	rateLimitedIfLater  []time.Time
	rateLimitedAccount  int64
	setRateLimitedCalls int
}

func (r *cn403RepoStub) SetError(context.Context, int64, string) error {
	r.setErrorCalls++
	return nil
}

func (r *cn403RepoStub) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.tempCalls++
	r.tempUntil = until
	r.lastTempReason = reason
	return nil
}

func (r *cn403RepoStub) SetRateLimitedIfLater(_ context.Context, accountID int64, until time.Time) error {
	r.rateLimitedIfLater = append(r.rateLimitedIfLater, until)
	r.rateLimitedAccount = accountID
	return nil
}

func (r *cn403RepoStub) SetRateLimited(context.Context, int64, time.Time) error {
	r.setRateLimitedCalls++
	return nil
}

// cn403CapStoreStub 只覆盖 SetCap：嵌入契约接口，A 侧新增方法不影响本文件编译。
type cn403CapStoreStub struct {
	ConcurrencyCapStore
	accountIDs []int64
	caps       []int
	reasons    []string
}

func (s *cn403CapStoreStub) SetCap(_ context.Context, accountID int64, cap int, reason string) error {
	s.accountIDs = append(s.accountIDs, accountID)
	s.caps = append(s.caps, cap)
	s.reasons = append(s.reasons, reason)
	return nil
}

const (
	cn403WeeklyQuotaBody       = `{"error":{"message":"Weekly usage limit reached. Resets in 2 days."}}`
	cn403FiveHourQuotaBody     = `{"error":{"message":"5-hour usage limit reached. Resets in 4hr 59min."}}`
	cn403KimiConcurrencyBody   = `{"error":{"type":"permission_error","message":"You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again."}}`
	cn403KimiConcurrencyEvent  = `{"type":"error","error":{"type":"permission_error","status_code":403,"message":"You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again."}}`
	cn403KimiConcurrencyPaired = `{"type":"response.failed","response":{"error":{"type":"permission_error","status_code":403,"message":"You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again."}}}`
	cn403KimiQuotaEvent        = `{"type":"error","error":{"type":"permission_error","status_code":403,"message":"5-hour usage limit reached. Resets in 4hr 59min."}}`
)

func newCN403TestService(repo AccountRepository, capStore ConcurrencyCapStore) *RateLimitService {
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetOpenAI403CounterCache(&openAI403CounterCacheStub{counts: []int64{openAI403DisableThreshold}})
	if capStore != nil {
		svc.SetConcurrencyCapStore(capStore)
	}
	return svc
}

func cn403CodingAccount(extra map[string]any) *Account {
	account := cnPauseWindowAccount(extra)
	account.Type = AccountTypeAPIKey
	account.Name = "kimi-coding"
	return account
}

// TestRateLimitService_CNQuota403NeverTouches403Counter 是本次改动的核心验收：
// 额度文案命中的 403 必须停调到窗口重置点，且【绝不】进入 handleOpenAI403 的连续
// 403 计数（计数到阈值会把账号永久置 status=error，而额度耗尽到窗口重置就会恢复）。
func TestRateLimitService_CNQuota403NeverTouches403Counter(t *testing.T) {
	t.Parallel()
	now := time.Now()
	weeklyReset := now.Add(48 * time.Hour)
	account := cn403CodingAccount(map[string]any{
		"kimi_usage_updated_at":    now.Format(time.RFC3339),
		"kimi_weekly_used_percent": 92.0,
		"kimi_weekly_reset_at":     weeklyReset.Format(time.RFC3339),
		"kimi_5h_used_percent":     10.0,
		"kimi_5h_reset_at":         now.Add(3 * time.Hour).Format(time.RFC3339),
	})
	repo := &cn403RepoStub{}
	svc := newCN403TestService(repo, nil)
	counter := svc.openAI403CounterCache.(*openAI403CounterCacheStub)

	shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, []byte(cn403WeeklyQuotaBody))

	require.True(t, shouldDisable, "额度耗尽的请求仍必须换号")
	require.Len(t, repo.rateLimitedIfLater, 1)
	require.WithinDuration(t, weeklyReset, repo.rateLimitedIfLater[0], 2*time.Second)
	require.Equal(t, account.ID, repo.rateLimitedAccount)
	require.Zero(t, repo.setErrorCalls, "额度耗尽不得永久禁用账号")
	require.Zero(t, repo.tempCalls)
	require.Zero(t, repo.setRateLimitedCalls, "额度停调必须走单调推进的 SetRateLimitedIfLater")
	require.Equal(t, []int64{openAI403DisableThreshold}, counter.counts, "额度文案命中不得消耗 403 计数")
}

// TestRateLimitService_CNQuota403_MissingSnapshotUsesShortCooldown 快照缺失时不能
// 落回 403 计数，而是短冷却 + 等下一轮额度探测刷新快照。
func TestRateLimitService_CNQuota403_MissingSnapshotUsesShortCooldown(t *testing.T) {
	t.Parallel()
	account := cn403CodingAccount(nil)
	repo := &cn403RepoStub{}
	svc := newCN403TestService(repo, nil)
	counter := svc.openAI403CounterCache.(*openAI403CounterCacheStub)

	before := time.Now()
	shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, []byte(cn403FiveHourQuotaBody))

	require.True(t, shouldDisable)
	require.Len(t, repo.rateLimitedIfLater, 1)
	require.WithinDuration(t, before.Add(60*time.Second), repo.rateLimitedIfLater[0], 3*time.Second)
	require.Zero(t, repo.setErrorCalls)
	require.Equal(t, []int64{openAI403DisableThreshold}, counter.counts, "快照缺失同样不得进入 403 计数")
}

// TestRateLimitService_CNConcurrency403SetsCapAndParks 并发文案命中：写 cap=1（探测
// 阶梯的起点）+ 保留 30s 临时停车，且不消耗 403 计数。
func TestRateLimitService_CNConcurrency403SetsCapAndParks(t *testing.T) {
	t.Parallel()
	account := cn403CodingAccount(nil)
	account.Platform = PlatformKimi
	repo := &cn403RepoStub{}
	capStore := &cn403CapStoreStub{}
	svc := newCN403TestService(repo, capStore)
	counter := svc.openAI403CounterCache.(*openAI403CounterCacheStub)

	shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, []byte(cn403KimiConcurrencyBody))

	require.True(t, shouldDisable)
	require.Equal(t, []int64{account.ID}, capStore.accountIDs)
	require.Equal(t, []int{1}, capStore.caps)
	require.Equal(t, []string{cnConcurrencyCapReason}, capStore.reasons)
	require.Equal(t, 1, repo.tempCalls)
	require.Contains(t, repo.lastTempReason, cnConcurrencyLimitReasonPrefix)
	require.Less(t, time.Until(repo.tempUntil), time.Minute, "并发超限是秒级信号，冷却必须远短于 403 默认 10 分钟")
	require.Zero(t, repo.setErrorCalls)
	require.Equal(t, []int64{openAI403DisableThreshold}, counter.counts, "并发文案命中不得消耗 403 计数")
}

// TestRateLimitService_CNQuota403AppliesInPoolMode 池模式账号同样必须被额度停调：
// 分类器收口早于池模式/自定义错误码/临时不可调度的早退分支。
func TestRateLimitService_CNQuota403AppliesInPoolMode(t *testing.T) {
	t.Parallel()
	now := time.Now()
	account := cn403CodingAccount(map[string]any{
		"kimi_weekly_used_percent": 92.0,
		"kimi_weekly_reset_at":     now.Add(48 * time.Hour).Format(time.RFC3339),
	})
	account.Credentials["pool_mode"] = true
	require.True(t, account.IsPoolMode())

	repo := &cn403RepoStub{}
	svc := newCN403TestService(repo, nil)

	shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, []byte(cn403WeeklyQuotaBody))

	require.True(t, shouldDisable)
	require.Len(t, repo.rateLimitedIfLater, 1, "池模式账号额度耗尽同样停调（否则会反复撞同一批耗尽的账号）")
	require.Zero(t, repo.setErrorCalls)
}

// TestRateLimitService_CN403SideEffectDedupedPerRequest 幂等键 = 请求 ID + 账号 + 分类：
// 同一请求（error + response.failed 双事件、HTTP 与流内重复命中）只写一次副作用。
func TestRateLimitService_CN403SideEffectDedupedPerRequest(t *testing.T) {
	t.Parallel()
	account := cn403CodingAccount(nil)
	repo := &cn403RepoStub{}
	capStore := &cn403CapStoreStub{}
	svc := newCN403TestService(repo, capStore)

	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "req-cn-403-1")
	require.True(t, svc.HandleCNClassifiedUpstreamError(ctx, account, http.StatusForbidden, []byte(cn403KimiConcurrencyBody)))
	require.True(t, svc.HandleCNClassifiedUpstreamError(ctx, account, http.StatusForbidden, []byte(cn403KimiConcurrencyBody)))
	require.Equal(t, 1, repo.tempCalls, "同一请求同一分类只写一次副作用")
	require.Len(t, capStore.caps, 1)

	otherCtx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "req-cn-403-2")
	require.True(t, svc.HandleCNClassifiedUpstreamError(otherCtx, account, http.StatusForbidden, []byte(cn403KimiConcurrencyBody)))
	require.Equal(t, 2, repo.tempCalls, "不同请求命中同一账号是新的副作用")
}

// TestRateLimitService_CN403ClassifierLeavesOtherErrorsAlone 未识别/非 CN 平台的 403
// 必须保持既有处理路径（分类器不得改道普通 403 策略）。
func TestRateLimitService_CN403ClassifierLeavesOtherErrorsAlone(t *testing.T) {
	t.Parallel()

	unknownCN := cn403CodingAccount(nil)
	require.False(t, (&RateLimitService{}).HandleCNClassifiedUpstreamError(context.Background(), unknownCN, http.StatusForbidden, []byte(`{"error":{"message":"forbidden"}}`)))

	openAI := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.False(t, (&RateLimitService{}).HandleCNClassifiedUpstreamError(context.Background(), openAI, http.StatusForbidden, []byte(cn403WeeklyQuotaBody)))
}

// TestOpenAIGatewayService_WSStream403CNClassesApplySideEffects 回归保护：WS/HTTP 桥
// 的流内 403 必须在 openAIStream403AccountFailure 谓词【之前】过分类器。kimi 并发文案
// 不命中该谓词，先过谓词会让流内 403 被静默丢弃（不写 cap、不换号）。
func TestOpenAIGatewayService_WSStream403CNClassesApplySideEffects(t *testing.T) {
	t.Parallel()
	account := cn403CodingAccount(nil)
	repo := &cn403RepoStub{}
	capStore := &cn403CapStoreStub{}
	svc := newCN403TestService(repo, capStore)
	gateway := &OpenAIGatewayService{rateLimitService: svc}

	require.False(t, openAIStream403AccountFailure([]byte(cn403KimiConcurrencyEvent), ""),
		"前提：kimi 并发文案不命中原有的 403 谓词")

	applied := gateway.handleOpenAIWSFailureAccountSideEffects(
		context.Background(), account, "kimi-k2", http.Header{}, []byte(cn403KimiConcurrencyEvent))

	require.True(t, applied)
	require.Equal(t, []int{1}, capStore.caps, "WS 流内 403 必须写出 cap=1")
	require.Equal(t, 1, repo.tempCalls)
}

// TestOpenAIGatewayService_WSStream403PairedEventsMergeOnce 同一逻辑错误产生的
// error + response.failed 两个事件只施加一次副作用。
func TestOpenAIGatewayService_WSStream403PairedEventsMergeOnce(t *testing.T) {
	t.Parallel()
	account := cn403CodingAccount(nil)
	repo := &cn403RepoStub{}
	capStore := &cn403CapStoreStub{}
	svc := newCN403TestService(repo, capStore)
	gateway := &OpenAIGatewayService{rateLimitService: svc}
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "req-ws-pair-1")

	require.True(t, gateway.handleOpenAIWSFailureAccountSideEffects(ctx, account, "kimi-k2", http.Header{}, []byte(cn403KimiConcurrencyEvent)))
	require.True(t, gateway.handleOpenAIWSFailureAccountSideEffects(ctx, account, "kimi-k2", http.Header{}, []byte(cn403KimiConcurrencyPaired)))

	require.Equal(t, 1, repo.tempCalls, "error + response.failed 必须合并为一次副作用")
	require.Len(t, capStore.caps, 1)
}

// TestOpenAIGatewayService_ResponsesStream403CNQuotaPauses 回归保护：Responses 流内
// error/response.failed 入口的额度耗尽同样要停调到窗口重置点（此前被静默丢弃）。
func TestOpenAIGatewayService_ResponsesStream403CNQuotaPauses(t *testing.T) {
	t.Parallel()
	now := time.Now()
	resetAt := now.Add(4*time.Hour + 30*time.Minute)
	account := cn403CodingAccount(map[string]any{
		"kimi_5h_used_percent": 96.0,
		"kimi_5h_reset_at":     resetAt.Format(time.RFC3339),
	})
	repo := &cn403RepoStub{}
	svc := newCN403TestService(repo, nil)
	gateway := &OpenAIGatewayService{rateLimitService: svc}

	status, applied := gateway.handleOpenAIStreamTerminalAccountSideEffects(
		nil, account, []byte(cn403KimiQuotaEvent), "5-hour usage limit reached. Resets in 4hr 59min.", http.Header{}, "kimi-k2")

	require.Equal(t, http.StatusForbidden, status)
	require.True(t, applied)
	require.Len(t, repo.rateLimitedIfLater, 1)
	require.WithinDuration(t, resetAt, repo.rateLimitedIfLater[0], 2*time.Second)
	require.Zero(t, repo.setErrorCalls)
}
