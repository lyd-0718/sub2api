import { describe, expect, it } from 'vitest'
import {
  cnBalanceCellVisible,
  cnPaygOnlyPlatform,
  cnSupportsNativeResponses,
  defaultCNAdaptiveBaseUrls,
  isCNProviderPlatform,
  isHeaderOverrideCapable,
  isMultiProtocolApiKeyPlatform,
  isOpenRouterAccount
} from '../credentialsBuilder'

describe('OpenRouter platform helpers', () => {
  it('treats OpenRouter as a pay-as-you-go multi-protocol API key platform', () => {
    expect(isCNProviderPlatform('openrouter')).toBe(true)
    expect(isMultiProtocolApiKeyPlatform('openrouter')).toBe(true)
    expect(cnSupportsNativeResponses('openrouter')).toBe(true)
    expect(isHeaderOverrideCapable('openrouter', 'apikey')).toBe(true)
    expect(cnBalanceCellVisible('openrouter', 'payg')).toBe(true)

    expect(cnPaygOnlyPlatform('openrouter')).toBe(true)
    expect(cnPaygOnlyPlatform('deepseek')).toBe(true)
    expect(cnPaygOnlyPlatform('kimi')).toBe(false)
  })

  it('defaults adaptive endpoints to OpenRouter native URLs', () => {
    expect(defaultCNAdaptiveBaseUrls('openrouter', 'payg')).toEqual({
      chat_completions: 'https://openrouter.ai/api/v1',
      anthropic: 'https://openrouter.ai/api',
      responses: 'https://openrouter.ai/api/v1'
    })
  })

  it('detects OpenRouter accounts by platform or by an openrouter.ai base URL', () => {
    expect(isOpenRouterAccount({ platform: 'openrouter', type: 'apikey' })).toBe(true)
    expect(
      isOpenRouterAccount({
        platform: 'deepseek',
        type: 'apikey',
        credentials: { base_url: 'https://openrouter.ai/api/v1' }
      })
    ).toBe(true)
    expect(
      isOpenRouterAccount({
        platform: 'zhipu',
        type: 'apikey',
        credentials: { api_base_urls: { anthropic: 'https://openrouter.ai/api' } }
      })
    ).toBe(true)
    expect(
      isOpenRouterAccount({ platform: 'deepseek', type: 'apikey', credentials: { base_url: 'https://api.deepseek.com' } })
    ).toBe(false)
    expect(
      isOpenRouterAccount({ platform: 'deepseek', type: 'apikey', credentials: { base_url: 'https://evil.example/openrouter.ai' } })
    ).toBe(false)
    expect(
      isOpenRouterAccount({ platform: 'openai', type: 'oauth', credentials: { base_url: 'https://openrouter.ai/api/v1' } })
    ).toBe(false)
    expect(isOpenRouterAccount(null)).toBe(false)
  })
})
