package service

import (
	"net/http"
	"strings"
)

// CN 平台（kimi/zhipu/minimax）上游错误的统一分类器。
//
// 背景：国产供应商把两类「可恢复」的账号级限制都回成 403——
//   - 并发超限（秒级瞬时，在途流结束即释放）；
//   - 滚动窗口额度耗尽（周 / 5h，到窗口重置点必然恢复）。
//
// 二者若落进通用 403 路径（计数 → 3 次永久 SetError），账号会被永久卡死在 error，
// 且在 failover 状态集里逐账号重放，足以把整组账号打下线。因此所有 403 落点
// （HTTP、Anthropic 流内、Responses 流内、WS）都必须先过本分类器：
// 命中并发/额度即走专用副作用并早退，绝不进入 403 计数。

// UpstreamErrorClass 是一次上游错误分类的结果。三类互斥，并发优先于额度。
type UpstreamErrorClass int

const (
	// UpstreamErrorOther 未被分类器识别：调用方维持既有 403 处理（计数 → 3 次禁用）。
	UpstreamErrorOther UpstreamErrorClass = iota
	// UpstreamErrorConcurrentLimit 账号并发超限：秒级瞬时信号，降 cap + 短冷却。
	UpstreamErrorConcurrentLimit
	// UpstreamErrorQuotaExhausted 滚动窗口额度耗尽：停调到窗口重置点。
	UpstreamErrorQuotaExhausted
	// UpstreamErrorAuth 鉴权/凭据失效：维持既有认证错误处理。
	UpstreamErrorAuth
)

// String 返回稳定的小写分类名，用于日志与指标标签。
func (c UpstreamErrorClass) String() string {
	switch c {
	case UpstreamErrorConcurrentLimit:
		return "concurrent_limit"
	case UpstreamErrorQuotaExhausted:
		return "quota_exhausted"
	case UpstreamErrorAuth:
		return "auth"
	default:
		return "other"
	}
}

// cnConcurrentLimitLooseWording 是并发超限文案的宽松兜底片段（大小写不敏感）。
// 上游可能改写标点或尾部文案（例如把句末的 "try again." 换成 "contact support."），
// kimi 精确文案失配时靠它兜住——否则流内 403 会被静默丢弃（不写 cap、不换号）。
const cnConcurrentLimitLooseWording = "concurrent request limit"

// cnQuotaWordingWindowTokens / cnQuotaWordingExhaustionTokens 共同定义「额度耗尽文案」：
// 必须同时命中一个窗口标识与一个耗尽语义，才判为额度类。
// 只命中窗口标识（请求日志里随便出现 "5h"）或只命中耗尽语义（普通 403 里的 "limit"）
// 都不足以写成额度停调——那会把无关 403 停调到数天后的窗口重置点。
var (
	cnQuotaWordingWeeklyWindowTokens   = []string{"weekly", "7-day", "7 day"}
	cnQuotaWordingFiveHourWindowTokens = []string{"5-hour", "5 hour", "5h", "five-hour"}
	cnQuotaWordingExhaustionTokens     = []string{
		"limit", "exceed", "exhaust", "quota", "usage", "reached", "out of",
		"额度", "配额", "用量", "超限", "耗尽", "用尽",
	}
)

// ClassifyCNUpstreamError 对 CN 平台（kimi/zhipu/minimax）的上游错误分类。
//
// 判定顺序（互斥，并发优先）：
//  1. 401 → 鉴权；
//  2. 403/429：kimi 并发文案（精确 + 宽松兜底）→ 并发限流；
//  3. 403/429：周（weekly/7-day）或 5h 额度文案 → 额度耗尽；
//  4. 403/429：凭据类文案 → 鉴权；
//  5. 其余 → 未识别。
//
// 非 CN 平台一律返回 UpstreamErrorOther：openai/anthropic/grok 各有自己的访问态、
// 凭据与窗口处理路径，误分类会把它们的正常 403 改道到 CN 停调逻辑。
// status 只接受 401/403/429，其它状态码返回未识别（调用方传流内语义状态码）。
func ClassifyCNUpstreamError(platform string, status int, body []byte) UpstreamErrorClass {
	if !IsCNProvider(platform) {
		return UpstreamErrorOther
	}
	switch status {
	case http.StatusUnauthorized:
		return UpstreamErrorAuth
	case http.StatusForbidden, http.StatusTooManyRequests:
	default:
		return UpstreamErrorOther
	}

	// 并发优先：kimi 并发文案与额度文案互斥，显式排序避免新增文案时优先级漂移。
	if platform == PlatformKimi {
		if cnConcurrentLimitExactWording(strings.TrimSpace(extractUpstreamErrorMessage(body))) ||
			cnBodyContainsFold(body, cnConcurrentLimitLooseWording) {
			return UpstreamErrorConcurrentLimit
		}
	}
	if cnQuotaWordingWindow(body) != "" {
		return UpstreamErrorQuotaExhausted
	}
	if openAIStreamCredentialAuthFailure(body) {
		return UpstreamErrorAuth
	}
	return UpstreamErrorOther
}

// cnConcurrentLimitExactWording 判断是否为 kimi 精确的并发超限文案（去首尾空白后全等）。
// 与停调 side effect 的 reason 共用 kimiConcurrentRequestLimitMessage，避免文案漂移。
func cnConcurrentLimitExactWording(upstreamMsg string) bool {
	return strings.TrimSpace(upstreamMsg) == kimiConcurrentRequestLimitMessage
}

// cnQuotaWordingWindow 从响应体文案识别额度耗尽的窗口：
// 命中周文案（weekly / 7-day / 7 day）→ "weekly"；否则命中 5h 文案 → "5h"；否则 ""。
//
// 仅用于分类判定与日志观测：停调终点一律取快照分档结果（见 cnQuotaPauseWindowAt），
// 文案只说「哪个窗口被拒」，不能说「窗口什么时候重置」。
func cnQuotaWordingWindow(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	text := strings.ToLower(string(body))
	if !cnTextContainsAny(text, cnQuotaWordingExhaustionTokens) {
		return ""
	}
	if cnTextContainsAny(text, cnQuotaWordingWeeklyWindowTokens) {
		return "weekly"
	}
	if cnTextContainsAny(text, cnQuotaWordingFiveHourWindowTokens) {
		return "5h"
	}
	return ""
}

// cnBodyContainsFold 对原始 body 做大小写不敏感的子串匹配（含非 JSON / 嵌套结构）。
func cnBodyContainsFold(body []byte, fragment string) bool {
	if len(body) == 0 || fragment == "" {
		return false
	}
	return strings.Contains(strings.ToLower(string(body)), strings.ToLower(fragment))
}

func cnTextContainsAny(lowerText string, tokens []string) bool {
	for _, token := range tokens {
		if strings.Contains(lowerText, token) {
			return true
		}
	}
	return false
}
