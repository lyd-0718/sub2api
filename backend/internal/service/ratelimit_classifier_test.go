//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClassifyCNUpstreamError 表驱动覆盖六类输入：kimi 精确并发文案、宽松兜底、
// 周额度文案、5h 额度文案、鉴权、未知。分类互斥且并发优先。
func TestClassifyCNUpstreamError(t *testing.T) {
	t.Parallel()

	const exactKimiConcurrency = `{"error":{"type":"permission_error","message":"You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again."}}`
	const looseKimiConcurrency = `{"error":{"message":"You've reached your concurrent request limit. Please contact support."}}`
	const upperCaseLooseWording = `{"error":{"message":"CONCURRENT REQUEST LIMIT reached for this key"}}`
	const weeklyQuota = `{"error":{"message":"Weekly usage limit reached. Resets in 2 days."}}`
	const sevenDayQuota = `{"error":{"message":"Your 7-day usage limit has been exceeded"}}`
	const fiveHourQuota = `{"error":{"message":"5-hour usage limit reached. Resets in 4hr 59min."}}`
	const compactFiveHourQuota = `{"error":{"message":"5h quota exhausted, resets at 15:00 UTC"}}`
	const structuredAuth = `{"error":{"type":"authentication_error","code":"invalid_api_key","message":"credential rejected"}}`
	const unknownForbidden = `{"error":{"message":"forbidden"}}`
	const htmlForbidden = `<html><body>Access denied by CDN</body></html>`

	cases := map[string]struct {
		platform string
		status   int
		body     string
		want     UpstreamErrorClass
	}{
		"kimi 精确并发文案":       {PlatformKimi, http.StatusForbidden, exactKimiConcurrency, UpstreamErrorConcurrentLimit},
		"kimi 宽松兜底并发文案":     {PlatformKimi, http.StatusForbidden, looseKimiConcurrency, UpstreamErrorConcurrentLimit},
		"kimi 宽松兜底大小写不敏感":   {PlatformKimi, http.StatusForbidden, upperCaseLooseWording, UpstreamErrorConcurrentLimit},
		"kimi 周额度文案":        {PlatformKimi, http.StatusForbidden, weeklyQuota, UpstreamErrorQuotaExhausted},
		"kimi 7-day 额度文案":   {PlatformKimi, http.StatusForbidden, sevenDayQuota, UpstreamErrorQuotaExhausted},
		"kimi 5h 额度文案":      {PlatformKimi, http.StatusForbidden, fiveHourQuota, UpstreamErrorQuotaExhausted},
		"kimi 5h 额度文案紧凑写法":  {PlatformKimi, http.StatusTooManyRequests, compactFiveHourQuota, UpstreamErrorQuotaExhausted},
		"kimi 401 鉴权":       {PlatformKimi, http.StatusUnauthorized, structuredAuth, UpstreamErrorAuth},
		"kimi 结构化凭据 403":    {PlatformKimi, http.StatusForbidden, structuredAuth, UpstreamErrorAuth},
		"kimi 未知文案":         {PlatformKimi, http.StatusForbidden, unknownForbidden, UpstreamErrorOther},
		"kimi HTML 403":     {PlatformKimi, http.StatusForbidden, htmlForbidden, UpstreamErrorOther},
		"kimi 空 body":       {PlatformKimi, http.StatusForbidden, "", UpstreamErrorOther},
		"非窗口状态码不分类":         {PlatformKimi, http.StatusBadRequest, weeklyQuota, UpstreamErrorOther},
		"其它 CN 平台无已知并发文案":   {PlatformZhipu, http.StatusForbidden, exactKimiConcurrency, UpstreamErrorOther},
		"minimax 周额度文案":     {PlatformMiniMax, http.StatusForbidden, weeklyQuota, UpstreamErrorQuotaExhausted},
		"非 CN 平台不参与分类":      {PlatformOpenAI, http.StatusForbidden, weeklyQuota, UpstreamErrorOther},
		"anthropic 平台不参与分类": {PlatformAnthropic, http.StatusForbidden, exactKimiConcurrency, UpstreamErrorOther},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := []byte(tc.body)
			require.Equal(t, tc.want, ClassifyCNUpstreamError(tc.platform, tc.status, body))
		})
	}
}

// TestClassifyCNUpstreamError_NilBodyIsSafe 保证流内空载荷不会 panic。
func TestClassifyCNUpstreamError_NilBodyIsSafe(t *testing.T) {
	t.Parallel()
	require.Equal(t, UpstreamErrorOther, ClassifyCNUpstreamError(PlatformKimi, http.StatusForbidden, nil))
}

// TestCNQuotaWordingWindow 额度文案必须区分周与 5h：周优先，只有窗口标识或只有
// 耗尽语义都不算额度类。
func TestCNQuotaWordingWindow(t *testing.T) {
	t.Parallel()

	require.Equal(t, "weekly", cnQuotaWordingWindow([]byte(`{"error":{"message":"Weekly usage limit reached."}}`)))
	require.Equal(t, "weekly", cnQuotaWordingWindow([]byte(`{"error":{"message":"7-day limit exceeded"}}`)))
	require.Equal(t, "5h", cnQuotaWordingWindow([]byte(`{"error":{"message":"5-hour usage limit reached"}}`)))
	require.Equal(t, "5h", cnQuotaWordingWindow([]byte(`{"error":{"message":"5h quota exhausted"}}`)))
	require.Equal(t, "", cnQuotaWordingWindow([]byte(`{"error":{"message":"weekly plan renewed"}}`)), "只有窗口标识不足以判额度耗尽")
	require.Equal(t, "", cnQuotaWordingWindow([]byte(`{"error":{"message":"request limit exceeded for this endpoint"}}`)), "只有耗尽语义不足以判额度耗尽")
	require.Equal(t, "", cnQuotaWordingWindow(nil))
}

// TestUpstreamErrorClassString 分类名是日志与指标的稳定契约。
func TestUpstreamErrorClassString(t *testing.T) {
	t.Parallel()
	require.Equal(t, "concurrent_limit", UpstreamErrorConcurrentLimit.String())
	require.Equal(t, "quota_exhausted", UpstreamErrorQuotaExhausted.String())
	require.Equal(t, "auth", UpstreamErrorAuth.String())
	require.Equal(t, "other", UpstreamErrorOther.String())
}
