/**
 * OpenRouter 账号的供应商路由（首选 / 备选 provider）。
 * 对应后端 GET/PUT /admin/cn-providers/accounts/:id/openrouter-routing。
 */

import { apiClient } from '../client'

/** OpenRouter 上某个模型的一家供应商（价格单位：美元 / 百万 token）。 */
export interface OpenRouterProviderOption {
  slug: string
  name: string
  quantization?: string
  /** 0 = 正常，负数 = OpenRouter 标记的异常 */
  status: number
  uptime_last_1d?: number
  uptime_last_30m?: number
  context_length?: number
  prompt_price?: number
  completion_price?: number
  cache_read_price?: number
  discount?: number
  supports_tools: boolean
}

export interface OpenRouterModelProviders {
  model: string
  providers: OpenRouterProviderOption[]
  error?: string
}

/** models: 上游模型名 → [首选, 备选] provider slug。 */
export interface OpenRouterProviderRouting {
  enabled: boolean
  models: Record<string, string[]>
}

export interface OpenRouterProviderRoutingView {
  account_id: number
  routing: OpenRouterProviderRouting
  models: OpenRouterModelProviders[]
}

export async function getOpenRouterRouting(accountId: number): Promise<OpenRouterProviderRoutingView> {
  const { data } = await apiClient.get<OpenRouterProviderRoutingView>(
    `/admin/cn-providers/accounts/${accountId}/openrouter-routing`
  )
  return data
}

export async function updateOpenRouterRouting(
  accountId: number,
  routing: OpenRouterProviderRouting
): Promise<OpenRouterProviderRouting> {
  const { data } = await apiClient.put<OpenRouterProviderRouting>(
    `/admin/cn-providers/accounts/${accountId}/openrouter-routing`,
    routing
  )
  return data
}

export default {
  getOpenRouterRouting,
  updateOpenRouterRouting
}
