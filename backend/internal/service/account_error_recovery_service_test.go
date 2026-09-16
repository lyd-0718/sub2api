package service

// 历史被禁用账号自动归队（AccountErrorRecoveryService）行为测试：
// 额度判定（本地快照 reset_at：过去=已重置即恢复，未来=仍满且零上游调用；无窗口键=验证仲裁）、
// 最小请求验证的放行条件、CAS 用「验证之后重读的 updated_at」、CAS 失败不消耗退避预算、
// 退避阶梯与 leader 锁。
//
// 设计前提（2026-09-16 需求方拍板）：恢复判定不探测上游额度接口——reset_at 是时间点
// 事实（管理前端额度倒计时读的就是它），不是采样值，快照新旧不影响判定。

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
		Concurrency:  1,
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

// cnRecoverySnapshotAvailable 构造「两档均未耗尽」的快照（用量低、重置点在未来）→ 已恢复。
func cnRecoverySnapshotAvailable(now time.Time) map[string]any {
	return map[string]any{
		cnExtraKey(PlatformKimi, cnExtraSuffixUsageUpdated): now.Add(-time.Minute).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):   10.0,
		cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed):       20.0,
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset):  now.Add(48 * time.Hour).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffix5hReset):      now.Add(2 * time.Hour).Format(time.RFC3339),
	}
}

// cnRecoverySnapshotExhaustedWeekly 构造「周窗口仍满」的快照（重置点在未来）→ 未恢复。
func cnRecoverySnapshotExhaustedWeekly(now time.Time) map[string]any {
	return map[string]any{
		cnExtraKey(PlatformKimi, cnExtraSuffixUsageUpdated): now.Add(-time.Minute).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):   90.0,
		cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed):       20.0,
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset):  now.Add(48 * time.Hour).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffix5hReset):      now.Add(2 * time.Hour).Format(time.RFC3339),
	}
}

// cnRecoverySnapshotResetPassed 构造「用量读数仍高但重置点已过」的快照（无论多旧）→
// 已恢复：reset_at 是时间点事实，窗口已经滚过。
func cnRecoverySnapshotResetPassed(now time.Time) map[string]any {
	return map[string]any{
		cnExtraKey(PlatformKimi, cnExtraSuffixUsageUpdated): now.Add(-72 * time.Hour).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):   99.0,
		cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed):       99.0,
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset):  now.Add(-time.Hour).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffix5hReset):      now.Add(-time.Minute).Format(time.RFC3339),
	}
}

func newCNRecoveryTestService(t *testing.T, now time.Time, repo AccountRepository, upstream HTTPUpstream) *AccountErrorRecoveryService {
	t.Helper()
	svc := NewAccountErrorRecoveryService(repo, upstream, &config.Config{}, time.Minute)
	svc.now = func() time.Time { return now }
	return svc
}

func TestAccountErrorRecoveryService_RestoresAccountUsingReloadedUpdatedAt(t *testing.T) {
	now := time.Now()
	scannedUpdatedAt := now.Add(-30 * time.Minute)
	// 扫描读到的是旧 updated_at；验证后重读到的才是 CAS 要比对的值。
	reloadedUpdatedAt := now.Add(-2 * time.Second)
	account := cnRecoveryKimiCodingAccount(scannedUpdatedAt, cnRecoverySnapshotAvailable(now))
	byID := &Account{}
	*byID = *account
	byID.UpdatedAt = reloadedUpdatedAt

	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: byID},
		restoreOK: true,
	}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1","type":"message"}`}

	svc := newCNRecoveryTestService(t, now, repo, upstream)
	svc.runOnce()

	require.Equal(t, 1, upstream.calls, "快照判定已恢复后必须发出最小验证请求")
	require.Len(t, repo.restoreCalls, 1)
	require.Equal(t, account.ID, repo.restoreCalls[0].accountID)
	require.True(t, repo.restoreCalls[0].expectedUpdatedAt.Equal(reloadedUpdatedAt),
		"CAS 必须用验证之后重读的 updated_at，而不是扫描开始时的旧值")
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

func TestAccountErrorRecoveryService_ResetPassedSnapshotRecoversWithoutProbe(t *testing.T) {
	now := time.Now()
	// 快照很旧且用量读数仍是 99%，但 reset_at 已过——窗口已滚过，直接判定恢复，
	// 全程零额度探测（本服务已不再持有额度探测依赖）。
	account := cnRecoveryKimiCodingAccount(now.Add(-72*time.Hour), cnRecoverySnapshotResetPassed(now))
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := newCNRecoveryTestService(t, now, repo, upstream)
	svc.runOnce()

	require.Equal(t, 1, upstream.calls, "reset_at 已过即判定恢复，只需最小请求验证")
	require.Len(t, repo.restoreCalls, 1)
}

func TestAccountErrorRecoveryService_ExhaustedQuotaSkipsVerifyAndBacksOff(t *testing.T) {
	now := time.Now()
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), cnRecoverySnapshotExhaustedWeekly(now))
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := newCNRecoveryTestService(t, now, repo, upstream)
	svc.runOnce()
	require.Zero(t, upstream.calls, "周窗口仍满（reset_at 在未来）时不得发出任何上游请求")
	require.Empty(t, repo.restoreCalls)

	// 退避已生效：下一轮仍不处理。
	svc.runOnce()
	require.Zero(t, upstream.calls, "退避窗口内不得重复处理")
}

func TestAccountErrorRecoveryService_NoSnapshotFallsBackToVerify(t *testing.T) {
	now := time.Now()
	// 快照里连窗口重置键都没有（从未探测过）→ 恢复判定不可知，
	// 由最小请求验证仲裁：验证成功即恢复。
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), nil)
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := newCNRecoveryTestService(t, now, repo, upstream)
	svc.runOnce()

	require.Equal(t, 1, upstream.calls, "无快照时必须用最小请求验证仲裁")
	require.Len(t, repo.restoreCalls, 1)
}

func TestAccountErrorRecoveryService_CASConflictDoesNotConsumeBackoffBudget(t *testing.T) {
	now := time.Now()
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), cnRecoverySnapshotAvailable(now))
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: false, // 并发改写：影响行数 0
	}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := newCNRecoveryTestService(t, now, repo, upstream)
	svc.runOnce()
	require.Len(t, repo.restoreCalls, 1)

	// CAS 冲突不消耗退避预算：下一轮立即重排并再次尝试。
	svc.runOnce()
	require.Equal(t, 2, upstream.calls, "CAS 冲突不得消耗退避预算")
	require.Len(t, repo.restoreCalls, 2)
	require.True(t, svc.due(account.ID, now), "CAS 冲突后账号应在本轮即可重排")
}

func TestAccountErrorRecoveryService_VerifyFailureBacksOff(t *testing.T) {
	now := time.Now()
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), cnRecoverySnapshotAvailable(now))
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	// HTTP 2xx 但携业务错误（流内错误落到响应体）同样算验证失败。
	upstream := &cnRecoveryUpstreamStub{body: `{"type":"error","error":{"type":"rate_limit_error"}}`}

	svc := newCNRecoveryTestService(t, now, repo, upstream)
	svc.runOnce()

	require.Equal(t, 1, upstream.calls)
	require.Empty(t, repo.restoreCalls, "验证失败不得恢复账号")
	require.False(t, svc.due(account.ID, now), "验证失败必须进入退避窗口")

	svc.runOnce()
	require.Equal(t, 1, upstream.calls, "退避窗口内不得重复验证")
}

func TestAccountErrorRecoveryService_VerifyRequestRespectsURLAllowlist(t *testing.T) {
	now := time.Now()
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), cnRecoverySnapshotAvailable(now))
	account.Credentials["base_url"] = "https://relay.attacker.example/api.kimi.com/coding"
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := NewAccountErrorRecoveryService(repo, upstream, cnProbeAllowlistConfig("api.kimi.com"), time.Minute)
	svc.now = func() time.Time { return now }
	svc.runOnce()

	require.Zero(t, upstream.calls, "出站 URL 被策略拒绝时不得发出请求（API key 不出站）")
	require.Empty(t, repo.restoreCalls)
}

func TestAccountErrorRecoveryService_SkipsAccountsWithoutCodingPlanSignal(t *testing.T) {
	now := time.Now()
	payg := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), cnRecoverySnapshotAvailable(now))
	payg.Credentials["account_mode"] = AccountModePayG
	relay := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), cnRecoverySnapshotAvailable(now))
	relay.Credentials["base_url"] = "https://relay.example.com/v1"

	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{payg, relay},
		byID:      map[int64]*Account{},
		restoreOK: true,
	}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := newCNRecoveryTestService(t, now, repo, upstream)
	svc.runOnce()

	require.Zero(t, upstream.calls, "无 coding plan 额度信号的账号不得发出任何上游请求")
	require.Empty(t, repo.restoreCalls)
}

func TestAccountErrorRecoveryService_HonorsLeaderLock(t *testing.T) {
	now := time.Now()
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), cnRecoverySnapshotAvailable(now))
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}
	cache := &fakeLeaderLockCache{}
	acquired, _ := cache.TryAcquireLeaderLock(context.Background(), cnRecoveryLeaderLockKey, "peer", time.Minute)
	require.True(t, acquired)

	svc := newCNRecoveryTestService(t, now, repo, upstream)
	svc.SetLeaderLock(cache, nil)
	svc.runOnce()

	require.Zero(t, upstream.calls, "非 leader 实例必须跳过本轮扫描")
	require.Empty(t, repo.restoreCalls)
}

func TestAccountErrorRecoveryService_BackoffLadderCapsAtLastEntry(t *testing.T) {
	now := time.Now()
	svc := newCNRecoveryTestService(t, now, &cnRecoveryRepoStub{}, &cnRecoveryUpstreamStub{})

	delays := make([]time.Duration, 0, len(defaultCNRecoveryBackoff)+2)
	for range len(defaultCNRecoveryBackoff) + 2 {
		delays = append(delays, svc.advanceBackoff(1, now))
	}
	require.Equal(t, defaultCNRecoveryBackoff, delays[:len(defaultCNRecoveryBackoff)])
	require.Equal(t, defaultCNRecoveryBackoff[len(defaultCNRecoveryBackoff)-1], delays[len(delays)-1],
		"退避必须封顶在最后一档（6h）")
}

func TestAccountErrorRecoveryService_RoundBudgetStaysInsideLeaderLockTTL(t *testing.T) {
	svc := NewAccountErrorRecoveryService(&cnRecoveryRepoStub{}, &cnRecoveryUpstreamStub{}, &config.Config{}, time.Minute)
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

func TestCNQuotaNextResetAt(t *testing.T) {
	now := time.Now()
	threshold := 85.0

	// 仅周窗口耗尽 → 排在周重置点。
	weeklyOnly := cnRecoveryKimiCodingAccount(now, cnRecoverySnapshotExhaustedWeekly(now))
	require.True(t, now.Add(48*time.Hour).Truncate(time.Second).Equal(cnQuotaNextResetAt(weeklyOnly, now, threshold)))

	// 两个窗口都耗尽 → 排在【最晚】的重置点（任一窗口仍满即未恢复）。
	both := cnRecoveryKimiCodingAccount(now, map[string]any{
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):  90.0,
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset): now.Add(48 * time.Hour).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed):      95.0,
		cnExtraKey(PlatformKimi, cnExtraSuffix5hReset):     now.Add(2 * time.Hour).Format(time.RFC3339),
	})
	require.True(t, now.Add(48*time.Hour).Truncate(time.Second).Equal(cnQuotaNextResetAt(both, now, threshold)))

	// 仅 5h 耗尽 → 排在 5h 重置点。
	fiveHourOnly := cnRecoveryKimiCodingAccount(now, map[string]any{
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):  10.0,
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset): now.Add(48 * time.Hour).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed):      95.0,
		cnExtraKey(PlatformKimi, cnExtraSuffix5hReset):     now.Add(2 * time.Hour).Format(time.RFC3339),
	})
	require.True(t, now.Add(2*time.Hour).Truncate(time.Second).Equal(cnQuotaNextResetAt(fiveHourOnly, now, threshold)))

	// 无耗尽窗口 → 零值。
	require.True(t, cnQuotaNextResetAt(cnRecoveryKimiCodingAccount(now, cnRecoverySnapshotAvailable(now)), now, threshold).IsZero())
}

func TestAccountErrorRecoveryService_QuotaFullSchedulesAtResetNotBackoff(t *testing.T) {
	now := time.Now()
	// 周窗口满，30 分钟后重置：退避阶梯如果生效会推到 10m/20m/40m/6h，
	// 正确行为是恰好排在 reset_at，且不消耗退避阶梯（failures 保持 0）。
	extra := map[string]any{
		cnExtraKey(PlatformKimi, cnExtraSuffixUsageUpdated): now.Add(-time.Minute).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):   100.0,
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset):  now.Add(30 * time.Minute).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed):       0.0,
		cnExtraKey(PlatformKimi, cnExtraSuffix5hReset):      now.Add(2 * time.Hour).Format(time.RFC3339),
	}
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), extra)
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := newCNRecoveryTestService(t, now, repo, upstream)
	svc.runOnce()

	require.Zero(t, upstream.calls, "仍满时零上游调用")
	require.False(t, svc.due(account.ID, now.Add(29*time.Minute)), "重置点前不得处理")
	require.True(t, svc.due(account.ID, now.Add(31*time.Minute)), "reset_at 一过即应到点")
	require.Zero(t, svc.states[account.ID].failures, "额度仍满不得消耗退避阶梯")

	// 到点后：快照已刷新（用量归零）→ 验证 → 恢复。
	account.Extra = cnRecoverySnapshotAvailable(now)
	svc.now = func() time.Time { return now.Add(31 * time.Minute) }
	svc.runOnce()
	require.Equal(t, 1, upstream.calls)
	require.Len(t, repo.restoreCalls, 1)
}

func TestCNQuotaRecoveredFromSnapshot(t *testing.T) {
	now := time.Now()
	threshold := 85.0

	recovered, known := cnQuotaRecoveredFromSnapshot(
		cnRecoveryKimiCodingAccount(now, cnRecoverySnapshotAvailable(now)), now, threshold)
	require.True(t, known)
	require.True(t, recovered, "两档均未耗尽即已恢复")

	recovered, known = cnQuotaRecoveredFromSnapshot(
		cnRecoveryKimiCodingAccount(now, cnRecoverySnapshotExhaustedWeekly(now)), now, threshold)
	require.True(t, known)
	require.False(t, recovered, "周窗口仍满（reset 在未来）即未恢复")

	recovered, known = cnQuotaRecoveredFromSnapshot(
		cnRecoveryKimiCodingAccount(now, cnRecoverySnapshotResetPassed(now)), now, threshold)
	require.True(t, known)
	require.True(t, recovered, "用量读数高但 reset 已过 = 窗口已滚过，与快照新旧无关")

	_, known = cnQuotaRecoveredFromSnapshot(cnRecoveryKimiCodingAccount(now, nil), now, threshold)
	require.False(t, known, "没有窗口重置键 → 不可知，交给验证请求仲裁")

	onlyUsed := map[string]any{
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed): 96.0,
	}
	_, known = cnQuotaRecoveredFromSnapshot(cnRecoveryKimiCodingAccount(now, onlyUsed), now, threshold)
	require.False(t, known, "只有用量读数没有重置键 → 同样不可知")
}
