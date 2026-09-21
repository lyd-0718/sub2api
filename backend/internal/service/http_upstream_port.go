package service

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// HTTPUpstream 上游 HTTP 请求接口
// 用于向上游 API（Claude、OpenAI、Gemini 等）发送请求
type HTTPUpstream interface {
	// Do 执行 HTTP 请求（不启用 TLS 指纹）
	Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error)

	// DoWithTLS 执行带 TLS 指纹伪装的 HTTP 请求
	//
	// profile 参数:
	//   - nil: 不启用 TLS 指纹，行为与 Do 方法相同
	//   - non-nil: 使用指定的 Profile 进行 TLS 指纹伪装
	//
	// Profile 由调用方通过 TLSFingerprintProfileService 解析后传入，
	// 支持按账号绑定的数据库 profile 或内置默认 profile。
	DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error)
}

// doAccountHTTPUpstream 是所有已绑定账号请求的统一出站入口。
// 指纹开关关闭时直接使用普通 Transport；开启时，真实流量、测试与后台探测
// 共享同一套账号级 TLS profile。
func doAccountHTTPUpstream(
	upstream HTTPUpstream,
	profiles *TLSFingerprintProfileService,
	req *http.Request,
	proxyURL string,
	account *Account,
	accountConcurrency int,
) (*http.Response, error) {
	if upstream == nil {
		return nil, errors.New("http upstream is not configured")
	}
	if account == nil {
		return nil, errors.New("account is nil")
	}
	if accountConcurrency <= 0 {
		accountConcurrency = 1
	}
	profile := profiles.ResolveTLSProfile(account)
	if profile == nil {
		return upstream.Do(req, proxyURL, account.ID, accountConcurrency)
	}
	return upstream.DoWithTLS(req, proxyURL, account.ID, accountConcurrency, profile)
}
