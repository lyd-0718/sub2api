package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
)

func capTestSettings() concurrencyCapSettings {
	return concurrencyCapSettings{
		Enabled:                true,
		Restricted:             1,
		CapMax:                 3,
		HoldWindow:             72 * time.Hour,
		ObserveWindow:          12 * time.Hour,
		ProbeProtocol:          APIProtocolAnthropic,
		ProbeEndpoint:          "/v1/messages",
		ProbeMaxTokens:         1,
		ProbeTimeout:           30 * time.Second,
		ProbeDrainTimeout:      10 * time.Second,
		ProbeInconclusiveRetry: time.Hour,
		FuseFlapThreshold:      3,
		LeaderLockTTL:          90 * time.Second,
	}
}

func TestPlanConcurrencyCapStep(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	due := now.Add(-time.Minute)
	settings := capTestSettings()
	disabledSettings := settings
	disabledSettings.Enabled = false
	restrictedTwoSettings := settings
	restrictedTwoSettings.Restricted = 2

	tests := []struct {
		name     string
		current  AccountConcurrencyCap
		outcome  capStepOutcome
		flap     int
		settings concurrencyCapSettings
		want     capStepPlan
	}{
		{
			name:     "总开关关闭时不判定",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 1, NextProbeAt: due},
			outcome:  capStepOutcomeUnprobed,
			settings: disabledSettings,
			want:     capStepPlan{Skip: true, SkipReason: capSkipDisabled, Reason: capReasonNoop, NextCap: 1},
		},
		{
			name:     "pinned 跳过自动回升（含熔断判定）",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 1, NextProbeAt: due, Pinned: true},
			outcome:  capStepOutcomePass,
			flap:     5,
			settings: settings,
			want: capStepPlan{
				Skip: true, SkipReason: capSkipPinned, Reason: capReasonNoop,
				NextCap: 1, OccupyLanes: 1, TargetLanes: 2,
			},
		},
		{
			name:     "已到 cap_max 停止回升",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 3, NextProbeAt: due},
			outcome:  capStepOutcomeUnprobed,
			settings: settings,
			want: capStepPlan{
				Skip: true, SkipReason: capSkipCapMax, Reason: capReasonNoop,
				NextCap: 3, OccupyLanes: 3, TargetLanes: 3,
			},
		},
		{
			name:     "熔断（7 天内 flap 达阈值）停止探测并推迟复查",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 1, NextProbeAt: due},
			outcome:  capStepOutcomeUnprobed,
			flap:     3,
			settings: settings,
			want: capStepPlan{
				Skip: true, SkipReason: capSkipFused, Reason: capReasonFuseHold,
				NextCap: 1, OccupyLanes: 1, TargetLanes: 2,
				NextProbeAt: now.Add(time.Hour), Reschedule: capRescheduleBestEffort,
			},
		},
		{
			name:     "flap 未达阈值仍可回升",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 1, NextProbeAt: due},
			outcome:  capStepOutcomePass,
			flap:     2,
			settings: settings,
			want: capStepPlan{
				Reason: capReasonProbePass, NextCap: 2, MutateCap: true, Raised: true,
				OccupyLanes: 1, TargetLanes: 2,
				NextProbeAt: now.Add(12 * time.Hour), Reschedule: capRescheduleBestEffort,
			},
		},
		{
			name:     "未到 next_probe_at 不探测",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 1, NextProbeAt: now.Add(time.Hour)},
			outcome:  capStepOutcomeUnprobed,
			settings: settings,
			want: capStepPlan{
				Skip: true, SkipReason: capSkipNotDue, Reason: capReasonNoop,
				NextCap: 1, OccupyLanes: 1, TargetLanes: 2,
			},
		},
		{
			name:     "未探测且已到期：仅求解跳过判定",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 1, NextProbeAt: due},
			outcome:  capStepOutcomeUnprobed,
			settings: settings,
			want: capStepPlan{
				Skip: true, SkipReason: capSkipNotDue, Reason: capReasonNoop,
				NextCap: 1, OccupyLanes: 1, TargetLanes: 2,
			},
		},
		{
			name:     "cap 记录为 0 时按 restricted 归一",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 0, NextProbeAt: due},
			outcome:  capStepOutcomeUnprobed,
			settings: settings,
			want: capStepPlan{
				Skip: true, SkipReason: capSkipNotDue, Reason: capReasonNoop,
				NextCap: 1, OccupyLanes: 1, TargetLanes: 2,
			},
		},
		{
			name:     "占槽失败：推迟本轮，cap 与 flap 均不变",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 1, NextProbeAt: due},
			outcome:  capStepOutcomeDeferred,
			settings: settings,
			want: capStepPlan{
				Reason: capReasonProbeDeferred, NextCap: 1,
				OccupyLanes: 1, TargetLanes: 2,
				NextProbeAt: now.Add(10 * time.Second), Reschedule: capRescheduleBestEffort,
			},
		},
		{
			name:     "cap=1 探测通过 → 2，等待 12h 观察",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 1, NextProbeAt: due},
			outcome:  capStepOutcomePass,
			settings: settings,
			want: capStepPlan{
				Reason: capReasonProbePass, NextCap: 2, MutateCap: true, Raised: true,
				OccupyLanes: 1, TargetLanes: 2,
				NextProbeAt: now.Add(12 * time.Hour), Reschedule: capRescheduleBestEffort,
			},
		},
		{
			name:     "cap=2 探测通过 → 3（cap_max，停止回升）",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 2, NextProbeAt: due},
			outcome:  capStepOutcomePass,
			settings: settings,
			want: capStepPlan{
				Reason: capReasonProbePass, NextCap: 3, MutateCap: true, Raised: true, StopProbing: true,
				OccupyLanes: 2, TargetLanes: 3,
			},
		},
		{
			name:     "cap=1 并发受限 → 保持 1 并重排 72h，计 flap",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 1, NextProbeAt: due},
			outcome:  capStepOutcomeLimited,
			settings: settings,
			want: capStepPlan{
				Reason: capReasonProbeLimited, NextCap: 1, RecordFlap: true,
				OccupyLanes: 1, TargetLanes: 2,
				NextProbeAt: now.Add(72 * time.Hour), Reschedule: capRescheduleRequired,
			},
		},
		{
			name:     "cap=2 并发受限 → 降回 1 并重排 72h，计 flap",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 2, NextProbeAt: due},
			outcome:  capStepOutcomeLimited,
			settings: settings,
			want: capStepPlan{
				Reason: capReasonProbeLimited, NextCap: 1, MutateCap: true, RecordFlap: true,
				OccupyLanes: 2, TargetLanes: 3,
				NextProbeAt: now.Add(72 * time.Hour), Reschedule: capRescheduleRequired,
			},
		},
		{
			name:     "自定义 restricted=2：失败回到 2 而非 1",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 2, NextProbeAt: due},
			outcome:  capStepOutcomeLimited,
			settings: restrictedTwoSettings,
			want: capStepPlan{
				Reason: capReasonProbeLimited, NextCap: 2, RecordFlap: true,
				OccupyLanes: 2, TargetLanes: 3,
				NextProbeAt: now.Add(72 * time.Hour), Reschedule: capRescheduleRequired,
			},
		},
		{
			name:     "cap=1 不确定 → cap 不变，1h 后重试且不计 flap",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 1, NextProbeAt: due},
			outcome:  capStepOutcomeInconclusive,
			settings: settings,
			want: capStepPlan{
				Reason: capReasonProbeInconclusive, NextCap: 1,
				OccupyLanes: 1, TargetLanes: 2,
				NextProbeAt: now.Add(time.Hour), Reschedule: capRescheduleRequired,
			},
		},
		{
			name:     "cap=2 不确定 → 保持 2，1h 后重试且不计 flap",
			current:  AccountConcurrencyCap{AccountID: 7, Cap: 2, NextProbeAt: due},
			outcome:  capStepOutcomeInconclusive,
			settings: settings,
			want: capStepPlan{
				Reason: capReasonProbeInconclusive, NextCap: 2,
				OccupyLanes: 2, TargetLanes: 3,
				NextProbeAt: now.Add(time.Hour), Reschedule: capRescheduleRequired,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planConcurrencyCapStep(now, tt.current, tt.outcome, tt.flap, tt.settings)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestConcurrencyCapFuseUsesRollingFlapEvents(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	settings := capTestSettings()
	due := now.Add(-time.Minute)

	t.Run("窗口内 3 条 flap 触发熔断", func(t *testing.T) {
		current := AccountConcurrencyCap{
			AccountID:   7,
			Cap:         1,
			NextProbeAt: due,
			FlapEvents:  []time.Time{now.Add(-time.Hour), now.Add(-48 * time.Hour), now.Add(-6 * 24 * time.Hour)},
		}
		flap := rollingFlapCount(current.FlapEvents, now, concurrencyCapFlapWindow)
		require.Equal(t, 3, flap)

		plan := planConcurrencyCapStep(now, current, capStepOutcomeUnprobed, flap, settings)
		require.True(t, plan.Skip)
		require.Equal(t, capSkipFused, plan.SkipReason)
	})

	t.Run("事件滑出 7 天窗口后自动解除并恢复回升", func(t *testing.T) {
		current := AccountConcurrencyCap{
			AccountID:   7,
			Cap:         1,
			NextProbeAt: due,
			FlapEvents: []time.Time{
				now.Add(-8 * 24 * time.Hour),
				now.Add(-concurrencyCapFlapWindow - time.Minute),
				now.Add(-30 * 24 * time.Hour),
			},
		}
		flap := rollingFlapCount(current.FlapEvents, now, concurrencyCapFlapWindow)
		require.Equal(t, 0, flap)

		plan := planConcurrencyCapStep(now, current, capStepOutcomePass, flap, settings)
		require.False(t, plan.Skip)
		require.Equal(t, 2, plan.NextCap)
		require.True(t, plan.Raised)
	})
}

func TestConcurrencyCapRunBudgetStaysWithinLeaderLease(t *testing.T) {
	settings := capTestSettings()
	require.Equal(t, 75*time.Second, concurrencyCapRunBudgetFor(settings))

	withoutLease := settings
	withoutLease.LeaderLockTTL = 0
	require.Equal(t, concurrencyCapRunBudget, concurrencyCapRunBudgetFor(withoutLease))

	longLease := settings
	longLease.LeaderLockTTL = time.Hour
	require.Equal(t, concurrencyCapRunBudget, concurrencyCapRunBudgetFor(longLease))

	tinyLease := settings
	tinyLease.LeaderLockTTL = time.Second
	require.Equal(t, concurrencyCapMinRunBudget, concurrencyCapRunBudgetFor(tinyLease))
}

func TestCapProbeEndpointPath(t *testing.T) {
	require.Equal(t, "/v1/messages", capProbeEndpointPath(""))
	require.Equal(t, "/v1/messages", capProbeEndpointPath("  "))
	require.Equal(t, "/v1/messages", capProbeEndpointPath("/v1/messages"))
	require.Equal(t, "/custom/messages", capProbeEndpointPath("custom/messages"))
}

func TestProbeTimeoutFallsBackToDefault(t *testing.T) {
	settings := capTestSettings()
	require.Equal(t, 30*time.Second, probeTimeout(settings))

	settings.ProbeTimeout = 0
	require.Equal(t, concurrencyCapDefaultProbeTimeout, probeTimeout(settings))

	settings.ProbeTimeout = 5 * time.Second
	require.Equal(t, 5*time.Second, probeTimeout(settings))
}

func TestRollingFlapCount(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	window := concurrencyCapFlapWindow

	tests := []struct {
		name   string
		events []time.Time
		want   int
	}{
		{name: "无事件", events: nil, want: 0},
		{name: "窗口内计数", events: []time.Time{now, now.Add(-time.Minute), now.Add(-6 * 24 * time.Hour)}, want: 3},
		{name: "边界上的事件计入", events: []time.Time{now.Add(-window)}, want: 1},
		{name: "刚滑出窗口的事件不计", events: []time.Time{now.Add(-window - time.Second)}, want: 0},
		{name: "零值时间戳忽略", events: []time.Time{{}, now.Add(-time.Hour)}, want: 1},
		{
			name:   "部分滑出",
			events: []time.Time{now.Add(-window - time.Hour), now.Add(-time.Hour), now.Add(-72 * time.Hour)},
			want:   2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, rollingFlapCount(tt.events, now, window))
		})
	}
}

func TestCapProbePayloadStreamsWithMinimalTokens(t *testing.T) {
	tests := []struct {
		protocol   string
		tokenKey   string
		maxTokens  int
		wantTokens int
	}{
		{protocol: APIProtocolAnthropic, tokenKey: "max_tokens", maxTokens: 1, wantTokens: 1},
		{protocol: APIProtocolChatCompletions, tokenKey: "max_tokens", maxTokens: 4, wantTokens: 4},
		{protocol: APIProtocolResponses, tokenKey: "max_output_tokens", maxTokens: 1, wantTokens: 1},
		{protocol: APIProtocolAdaptive, tokenKey: "max_tokens", maxTokens: 1, wantTokens: 1},
	}

	for _, tt := range tests {
		t.Run(tt.protocol, func(t *testing.T) {
			body, err := capProbePayload(tt.protocol, "kimi-k2.5", tt.maxTokens)
			require.NoError(t, err)

			var payload map[string]any
			require.NoError(t, json.Unmarshal(body, &payload))
			require.Equal(t, "kimi-k2.5", payload["model"])
			require.Equal(t, true, payload["stream"])
			require.EqualValues(t, tt.wantTokens, payload[tt.tokenKey])
		})
	}

	// max_tokens <= 0 时归一为最小合法值 1。
	body, err := capProbePayload(APIProtocolAnthropic, "kimi-k2.5", 0)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(body, &payload))
	require.EqualValues(t, 1, payload["max_tokens"])
}

func TestCapProbeStreamErrorMessage(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantMessage string
		wantOK      bool
	}{
		{
			name:        "Anthropic 流内 error",
			body:        "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"You've reached your concurrent request limit.\"}}\n\n",
			wantMessage: "You've reached your concurrent request limit.",
			wantOK:      true,
		},
		{
			name:        "Responses response.failed",
			body:        "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"boom\"}}}\n\n",
			wantMessage: "boom",
			wantOK:      true,
		},
		{
			name:        "Chat Completions 裸 error",
			body:        "data: {\"error\":{\"message\":\"bad request\"}}\n\n",
			wantMessage: "bad request",
			wantOK:      true,
		},
		{
			name:   "正常流式内容不算错误",
			body:   "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"p\"}}\n\ndata: [DONE]\n\n",
			wantOK: false,
		},
		{
			name:   "非 SSE 文本不算流内错误",
			body:   "{\"type\":\"error\",\"error\":{\"message\":\"raw json\"}}",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message, ok := capProbeStreamErrorMessage([]byte(tt.body))
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantMessage, message)
		})
	}
}

func TestCapProbeMaxTokensRejected(t *testing.T) {
	require.False(t, capProbeMaxTokensRejected(nil))
	require.False(t, capProbeMaxTokensRejected([]byte(`{"error":{"message":"invalid model"}}`)))
	require.True(t, capProbeMaxTokensRejected([]byte(`{"error":{"message":"max_tokens must be greater than 0"}}`)))
	require.True(t, capProbeMaxTokensRejected([]byte(`{"error":{"message":"max_output_tokens is too small"}}`)))
}

func TestSelectCapProbeModelPrefersMappingValues(t *testing.T) {
	account := &Account{
		Platform: PlatformKimi,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-sonnet-4-5": "kimi-k2.6",
				"claude-haiku-4-5":  "kimi-k2.5",
				"claude-*":          "kimi-k2",
			},
		},
	}
	require.Equal(t, "kimi-k2", selectCapProbeModel(account))

	empty := &Account{Platform: PlatformKimi}
	require.Equal(t, claude.DefaultTestModel, selectCapProbeModel(empty))
	require.Equal(t, "", selectCapProbeModel(nil))
}

func TestConcurrencyCapProbeMetricsSnapshot(t *testing.T) {
	metrics := NewConcurrencyCapProbeMetrics()
	metrics.IncProbe(2, concurrencyCapProbeResultPass)
	metrics.IncProbe(2, concurrencyCapProbeResultPass)
	metrics.IncProbe(3, concurrencyCapProbeResultLimited)
	metrics.IncProbe(3, concurrencyCapProbeResultInconclusive)
	metrics.IncRaise()
	metrics.IncFlap()
	metrics.IncFlap()
	metrics.IncFuse()

	snapshot := metrics.Snapshot()
	require.Equal(t, []ConcurrencyCapProbeCount{
		{Target: 2, Result: concurrencyCapProbeResultPass, Count: 2},
		{Target: 3, Result: concurrencyCapProbeResultInconclusive, Count: 1},
		{Target: 3, Result: concurrencyCapProbeResultLimited, Count: 1},
	}, snapshot.ProbeTotal)
	require.EqualValues(t, 1, snapshot.RaiseTotal)
	require.EqualValues(t, 2, snapshot.FlapTotal)
	require.EqualValues(t, 1, snapshot.FuseTotal)

	var nilMetrics *ConcurrencyCapProbeMetrics
	nilMetrics.IncProbe(2, concurrencyCapProbeResultPass)
	nilMetrics.IncRaise()
	require.Empty(t, nilMetrics.Snapshot().ProbeTotal)
}
