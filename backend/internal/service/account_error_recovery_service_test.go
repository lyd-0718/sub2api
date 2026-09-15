package service

// 历史被禁用账号自动归队（AccountErrorRecoveryService）行为测试：
// 额度判定（快照新鲜 / 过期刷新 / 探测重试上限）、最小请求验证的放行条件、
// CAS 用「探测+验证之后重读的 updated_at」、CAS 失败不消耗退避预算、
// 退避阶梯与 leader 锁。

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type cnRecoveryRestoreCall struct {
	accountID         int64
	expectedUpdatedAt time.Time
}

// cnRecoveryRepoStub 只覆盖自动归队用到的最小仓储面（其余方法照旧 panic）。
type cnRecoveryRepoStub struct {
	AccountRepository
	disabled     []*Account
	byID         map[int64]*Account
	restoreCalls []cnRecoveryRestoreCall
	restoreOK    bool
	restoreErr   error
}

func (r *cnRecoveryRepoStub) ListCNQuotaDisabled(ctx context.Context, platform string) ([]*Account, error) {
	out := make([]*Account, 0, len(r.disabled))
	for _, account := range r.disabled {
		if account.Platform == platform {
			out = append(out, account)
		}
	}
	return out, nil
}

func (r *cnRecoveryRepoStub) GetByID(ctx context.Context, id int64) (*Account, error) {
	account, ok := r.byID[id]
	if !ok {
		return nil, nil
	}
	return account, nil
}

func (r *cnRecoveryRepoStub) RestoreRecoveredAccount(ctx context.Context, accountID int64, expectedUpdatedAt time.Time) (bool, error) {
	r.restoreCalls = append(r.restoreCalls, cnRecoveryRestoreCall{accountID: accountID, expectedUpdatedAt: expectedUpdatedAt})
	if r.restoreErr != nil {
		return false, r.restoreErr
	}
	return r.restoreOK, nil
}

// cnRecoveryProberStub 依次返回预设结果，耗尽后重复最后一项。
type cnRecoveryProberStub struct {
	calls   int
	results []*CNProviderQuotaProbeResult
	errs    []error
}

func (p *cnRecoveryProberStub) QueryUsage(ctx context.Context, accountID int64) (*CNProviderQuotaProbeResult, error) {
	index := p.calls
	p.calls++
	if len(p.errs) > 0 {
		if index < len(p.errs) && p.errs[index] != nil {
			return nil, p.errs[index]
		}
		if len(p.results) == 0 {
			return nil, context.DeadlineExceeded
		}
	}
	if len(p.results) == 0 {
		return nil, context.DeadlineExceeded
	}
	if index >= len(p.results) {
		index = len(p.results) - 1
	}
	return p.results[index], nil
}

// cnRecoveryUpstreamStub 记录验证请求，返回固定状态码与响应体。
type cnRecoveryUpstreamStub struct {
	calls    int
	requests []*http.Request
	status   int
	body     string
	err      error
}

func (u *cnRecoveryUpstreamStub) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	u.calls++
	u.requests = append(u.requests, req)
	if u.err != nil {
		return nil, u.err
	}
	status := u.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(u.body)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func (u *cnRecoveryUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func cnRecoveryProbeResult(tiers ...CNQuotaTier) *CNProviderQuotaProbeResult {
	return &CNProviderQuotaProbeResult{Provider: PlatformKimi, Success: true, CredentialValid: true, Tiers: tiers, StatusCode: http.StatusOK}
}

// cnRecoveryKimiCodingAccount 构造一个被禁用（status='error'）的 kimi Coding Plan 账号。
func cnRecoveryKimiCodingAccount(updatedAt time.Time, extra map[string]any) *Account {
	if extra == nil {
		extra = map[string]any{}
	}
	return &Account{
		ID:           7,
		Name:         "kimi-coding-7",
		Platform:     PlatformKimi,
		Type:         AccountTypeAPIKey,
		Status:       StatusError,
		Schedulable:  false,
		ErrorMessage: "upstream returned 403 (quota exhausted)",
		Concurrency:  3,
		UpdatedAt:    updatedAt,
		Extra:        extra,
		Credentials: map[string]any{
			"account_mode": AccountModeCoding,
			"api_key":      "sk-kimi-test",
			"base_url":     "https://api.kimi.com/coding",
			"api_protocol": APIProtocolAnthropic,
		},
	}
}

// cnRecoveryFreshSnapshot 构造一份「两档均未满」的新鲜额度快照。
func cnRecoveryFreshSnapshot(now time.Time, weeklyUsed, fiveHourUsed float64) map[string]any {
	return map[string]any{
		cnExtraKey(PlatformKimi, cnExtraSuffixUsageUpdated): now.Add(-time.Minute).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):   weeklyUsed,
		cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed):       fiveHourUsed,
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset):  now.Add(48 * time.Hour).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffix5hReset):      now.Add(2 * time.Hour).Format(time.RFC3339),
	}
}

func newCNRecoveryTestService(t *testing.T, now time.Time, repo AccountRepository, prober cnQuotaProber, upstream HTTPUpstream) *AccountErrorRecoveryService {
	t.Helper()
	svc := NewAccountErrorRecoveryService(repo, prober, upstream, &config.Config{}, time.Minute)
	svc.now = func() time.Time { return now }
	return svc
}

func TestAccountErrorRecoveryService_RestoresAccountUsingReloadedUpdatedAt(t *testing.T) {
	now := time.Now()
	probeTime := now.Add(-30 * time.Minute)
	// 探测落快照会把 updated_at 推进到探测时刻：CAS 必须用重读后的值。
	reloadedUpdatedAt := now.Add(-2 * time.Second)
	account := cnRecoveryKimiCodingAccount(probeTime, cnRecoveryFreshSnapshot(now, 10, 20))
	byID := &Account{}
	*byID = *account
	byID.UpdatedAt = reloadedUpdatedAt

	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: byID},
		restoreOK: true,
	}
	prober := &cnRecoveryProberStub{}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1","type":"message"}`}

	svc := newCNRecoveryTestService(t, now, repo, prober, upstream)
	svc.runOnce()

	require.Zero(t, prober.calls, "新鲜快照应直接判定，不额外探测上游")
	require.Equal(t, 1, upstream.calls, "额度未满时必须发出最小验证请求")
	require.Len(t, repo.restoreCalls, 1)
	require.Equal(t, account.ID, repo.restoreCalls[0].accountID)
	require.True(t, repo.restoreCalls[0].expectedUpdatedAt.Equal(reloadedUpdatedAt),
		"CAS 必须用探测+验证之后重读的 updated_at，而不是扫描开始时的旧值")
	require.Empty(t, svc.states, "恢复成功后不再保留退避状态")

	// 验证请求走 anthropic 原生端点，并带最小化请求体。
	req := upstream.requests[0]
	require.Equal(t, http.MethodPost, req.Method)
	require.Equal(t, "api.kimi.com", req.URL.Host)
	require.True(t, strings.HasSuffix(req.URL.Path, "/v1/messages"), "验证端点必须与生产 /v1/messages 同源，实际 %q", req.URL.Path)
	require.Equal(t, "2023-06-01", req.Header.Get("anthropic-version"))
	require.Equal(t, "sk-kimi-test", req.Header.Get("x-api-key"))
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), `"max_tokens":8`)
}

func TestAccountErrorRecoveryService_RefreshesStaleSnapshotBeforeJudging(t *testing.T) {
	now := time.Now()
	// 快照已过期（30 分钟前），且内容显示周窗口已满 —— 必须刷新后再判，不能拿旧快照否决。
	stale := cnRecoveryFreshSnapshot(now.Add(-30*time.Minute), 99, 99)
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), stale)
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	prober := &cnRecoveryProberStub{results: []*CNProviderQuotaProbeResult{
		cnRecoveryProbeResult(
			CNQuotaTier{Window: "5h", UsedPercent: 12},
			CNQuotaTier{Window: "weekly", UsedPercent: 31},
		),
	}}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := newCNRecoveryTestService(t, now, repo, prober, upstream)
	svc.runOnce()

	require.Equal(t, 1, prober.calls, "过期快照必须先刷新")
	require.Equal(t, 1, upstream.calls)
	require.Len(t, repo.restoreCalls, 1)
}

func TestAccountErrorRecoveryService_ExhaustedQuotaSkipsVerifyAndBacksOff(t *testing.T) {
	now := time.Now()
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), nil)
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	prober := &cnRecoveryProberStub{results: []*CNProviderQuotaProbeResult{
		cnRecoveryProbeResult(
			CNQuotaTier{Window: "5h", UsedPercent: 20},
			CNQuotaTier{Window: "weekly", UsedPercent: 90},
		),
	}}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := newCNRecoveryTestService(t, now, repo, prober, upstream)
	svc.runOnce()
	require.Equal(t, 1, prober.calls)
	require.Zero(t, upstream.calls, "周窗口仍满时不得发出验证请求")
	require.Empty(t, repo.restoreCalls)

	// 退避已生效：下一轮不重复探测。
	svc.runOnce()
	require.Equal(t, 1, prober.calls, "退避窗口内不得重复探测")
}

func TestAccountErrorRecoveryService_CASConflictDoesNotConsumeBackoffBudget(t *testing.T) {
	now := time.Now()
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), nil)
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: false, // 并发改写：影响行数 0
	}
	prober := &cnRecoveryProberStub{results: []*CNProviderQuotaProbeResult{
		cnRecoveryProbeResult(
			CNQuotaTier{Window: "5h", UsedPercent: 5},
			CNQuotaTier{Window: "weekly", UsedPercent: 5},
		),
	}}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := newCNRecoveryTestService(t, now, repo, prober, upstream)
	svc.runOnce()
	require.Len(t, repo.restoreCalls, 1)

	// CAS 冲突不消耗退避预算：下一轮立即重排并再次尝试。
	svc.runOnce()
	require.Equal(t, 2, prober.calls, "CAS 冲突不得消耗退避预算")
	require.Equal(t, 2, upstream.calls)
	require.Len(t, repo.restoreCalls, 2)
	require.True(t, svc.due(account.ID, now), "CAS 冲突后账号应在本轮即可重排")
}

func TestAccountErrorRecoveryService_VerifyFailureBacksOff(t *testing.T) {
	now := time.Now()
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), cnRecoveryFreshSnapshot(now, 5, 5))
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	prober := &cnRecoveryProberStub{}
	// HTTP 2xx 但携业务错误（流内错误落到响应体）同样算验证失败。
	upstream := &cnRecoveryUpstreamStub{body: `{"type":"error","error":{"type":"rate_limit_error"}}`}

	svc := newCNRecoveryTestService(t, now, repo, prober, upstream)
	svc.runOnce()

	require.Equal(t, 1, upstream.calls)
	require.Empty(t, repo.restoreCalls, "验证失败不得恢复账号")
	require.False(t, svc.due(account.ID, now), "验证失败必须进入退避窗口")

	svc.runOnce()
	require.Equal(t, 1, upstream.calls, "退避窗口内不得重复验证")
}

func TestAccountErrorRecoveryService_VerifyRequestRespectsURLAllowlist(t *testing.T) {
	now := time.Now()
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), cnRecoveryFreshSnapshot(now, 5, 5))
	account.Credentials["base_url"] = "https://relay.attacker.example/api.kimi.com/coding"
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := NewAccountErrorRecoveryService(repo, &cnRecoveryProberStub{}, upstream, cnProbeAllowlistConfig("api.kimi.com"), time.Minute)
	svc.now = func() time.Time { return now }
	svc.runOnce()

	require.Zero(t, upstream.calls, "出站 URL 被策略拒绝时不得发出请求（API key 不出站）")
	require.Empty(t, repo.restoreCalls)
}

func TestAccountErrorRecoveryService_SkipsAccountsWithoutCodingPlanSignal(t *testing.T) {
	now := time.Now()
	payg := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), cnRecoveryFreshSnapshot(now, 5, 5))
	payg.Credentials["account_mode"] = AccountModePayG
	relay := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), cnRecoveryFreshSnapshot(now, 5, 5))
	relay.Credentials["base_url"] = "https://relay.example.com/v1"

	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{payg, relay},
		byID:      map[int64]*Account{},
		restoreOK: true,
	}
	prober := &cnRecoveryProberStub{}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := newCNRecoveryTestService(t, now, repo, prober, upstream)
	svc.runOnce()

	require.Zero(t, prober.calls, "无 coding plan 额度信号的账号不得探测")
	require.Zero(t, upstream.calls)
	require.Empty(t, repo.restoreCalls)
}

func TestAccountErrorRecoveryService_ProbeAttemptsAreBoundedPerRound(t *testing.T) {
	now := time.Now()
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), nil)
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	prober := &cnRecoveryProberStub{errs: []error{context.DeadlineExceeded}}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := newCNRecoveryTestService(t, now, repo, prober, upstream)
	svc.runOnce()

	require.Equal(t, cnRecoveryProbeAttemptsPerRound, prober.calls, "单账号单轮探测次数必须有上限")
	require.Zero(t, upstream.calls)
	require.Empty(t, repo.restoreCalls)
}

func TestAccountErrorRecoveryService_HonorsLeaderLock(t *testing.T) {
	now := time.Now()
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), cnRecoveryFreshSnapshot(now, 5, 5))
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}
	cache := &fakeLeaderLockCache{}
	acquired, _ := cache.TryAcquireLeaderLock(context.Background(), cnRecoveryLeaderLockKey, "peer", time.Minute)
	require.True(t, acquired)

	svc := newCNRecoveryTestService(t, now, repo, &cnRecoveryProberStub{}, upstream)
	svc.SetLeaderLock(cache, nil)
	svc.runOnce()

	require.Zero(t, upstream.calls, "非 leader 实例必须跳过本轮扫描")
	require.Empty(t, repo.restoreCalls)
}

func TestAccountErrorRecoveryService_BackoffLadderCapsAtLastEntry(t *testing.T) {
	now := time.Now()
	svc := newCNRecoveryTestService(t, now, &cnRecoveryRepoStub{}, &cnRecoveryProberStub{}, &cnRecoveryUpstreamStub{})

	delays := make([]time.Duration, 0, len(defaultCNRecoveryBackoff)+2)
	for range len(defaultCNRecoveryBackoff) + 2 {
		delays = append(delays, svc.advanceBackoff(1, now))
	}
	require.Equal(t, defaultCNRecoveryBackoff, delays[:len(defaultCNRecoveryBackoff)])
	require.Equal(t, defaultCNRecoveryBackoff[len(defaultCNRecoveryBackoff)-1], delays[len(delays)-1],
		"退避必须封顶在最后一档（6h）")
}

func TestAccountErrorRecoveryService_RoundBudgetStaysInsideLeaderLockTTL(t *testing.T) {
	svc := NewAccountErrorRecoveryService(&cnRecoveryRepoStub{}, &cnRecoveryProberStub{}, &cnRecoveryUpstreamStub{}, &config.Config{}, time.Minute)
	require.Equal(t, cnRecoveryRoundBudget, svc.roundBudget(), "单实例（无锁后端）用满单轮预算")

	svc.SetLeaderLock(&fakeLeaderLockCache{}, nil)
	require.Equal(t, cnRecoveryDefaultLeaderLockTTL-cnRecoveryLeaderLockMargin, svc.roundBudget(),
		"有协调后端时单轮预算必须落在 leader 锁 TTL 内（锁不续租）")
}

func TestParseCNRecoveryBackoff(t *testing.T) {
	require.Equal(t,
		[]time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 6 * time.Hour},
		parseCNRecoveryBackoff("10m, 20m,40m,6h"))
	require.Nil(t, parseCNRecoveryBackoff(""))
	require.Nil(t, parseCNRecoveryBackoff("10m,soon"), "任一项非法应整表回落默认值")
	require.Nil(t, parseCNRecoveryBackoff("-5m"))
}

func TestCNQuotaProbeResultExhausted(t *testing.T) {
	require.True(t, cnQuotaProbeResultExhausted(
		cnRecoveryProbeResult(CNQuotaTier{Window: "5h", UsedPercent: 10}, CNQuotaTier{Window: "weekly", UsedPercent: 85}), 85))
	require.True(t, cnQuotaProbeResultExhausted(
		cnRecoveryProbeResult(CNQuotaTier{Window: "5h", UsedPercent: 99}, CNQuotaTier{Window: "weekly", UsedPercent: 10}), 85))
	require.False(t, cnQuotaProbeResultExhausted(
		cnRecoveryProbeResult(CNQuotaTier{Window: "5h", UsedPercent: 84}, CNQuotaTier{Window: "weekly", UsedPercent: 84}), 85))
	require.False(t, cnQuotaProbeResultExhausted(cnRecoveryProbeResult(), 85),
		"缺失档位不算耗尽——最终放行门是最小请求验证")
}

func TestCNRecoverySnapshotFresh(t *testing.T) {
	now := time.Now()
	fresh := cnRecoveryKimiCodingAccount(now, cnRecoveryFreshSnapshot(now, 5, 5))
	require.True(t, cnRecoverySnapshotFresh(fresh, now, cnRecoverySnapshotMaxAge))

	stale := cnRecoveryKimiCodingAccount(now, cnRecoveryFreshSnapshot(now.Add(-11*time.Minute), 5, 5))
	require.False(t, cnRecoverySnapshotFresh(stale, now, cnRecoverySnapshotMaxAge),
		"超过 10 分钟必须视为过期并刷新")

	missing := cnRecoveryKimiCodingAccount(now, nil)
	require.False(t, cnRecoverySnapshotFresh(missing, now, cnRecoverySnapshotMaxAge))
}
