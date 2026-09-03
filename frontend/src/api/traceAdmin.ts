/**
 * 二开模块 API：会话 Trace 管理 + 账号用量导出
 */
import { apiClient } from './client'

// ---------- 会话 Trace ----------

export interface TraceSession {
  date: string
  session_id: string
  turns: number
  size_bytes: number
  archived: boolean
  first_at: string
  last_at: string
  model: string
  api_key_id: number
  api_key_name: string
  user_id: number
}

export interface TraceStats {
  today_turns: number
  hot_bytes: number
  archived_bytes: number
  total_sessions: number
}

export interface TraceArchiveSettings {
  enabled: boolean
  time_of_day: string
  keep_hot_days: number
}

export interface TraceArchiveResult {
  date: string
  sessions: number
  turns: number
  kept_originals: boolean
  archived: string[]
  skipped: string[]
}

export const traceAdminAPI = {
  async sessions(params?: { date?: string; status?: string; session_id?: string; api_key_id?: number; page?: number; page_size?: number }) {
    const query: Record<string, string | number> = {}
    if (params?.date) query.date = params.date
    if (params?.session_id) query.session_id = params.session_id
    if (params?.api_key_id) query.api_key_id = params.api_key_id
    if (params?.page) query.page = params.page
    if (params?.page_size) query.page_size = params.page_size
    if (params?.status === 'archived') query.archived = 'true'
    if (params?.status === 'hot') query.archived = 'false'
    const { data } = await apiClient.get<{ items: TraceSession[]; total: number; page: number; page_size: number; key_counts?: Record<string, number>; dates?: string[] }>(
      '/admin/traces/sessions',
      { params: query, timeout: 60000 }
    )
    return data
  },

  async stats() {
    const { data } = await apiClient.get<TraceStats>('/admin/traces/stats', { timeout: 60000 })
    return data
  },

  async archive(date: string) {
    const { data } = await apiClient.post<TraceArchiveResult>(
      '/admin/traces/archive',
      { date },
      { timeout: 300000 } // 大日期目录归档可能耗时数分钟
    )
    return data
  },

  async getSettings() {
    const { data } = await apiClient.get<TraceArchiveSettings>('/admin/traces/settings')
    return data
  },

  async saveSettings(settings: TraceArchiveSettings) {
    const { data } = await apiClient.put<TraceArchiveSettings>('/admin/traces/settings', settings)
    return data
  },

  /** 下载会话（热数据现场压缩，已归档直接取包）。前端 axios 带鉴权，blob 落地。 */
  async download(apiKeyId: number, date: string, sessionId: string, onProgress?: (percent: number) => void) {
    const { data, headers } = await apiClient.get('/admin/traces/download', {
      params: { key: apiKeyId, date, session: sessionId },
      responseType: 'blob',
      timeout: 300000,
      onDownloadProgress: (e) => {
        if (onProgress && e.total) onProgress(Math.round((e.loaded / e.total) * 100))
      }
    })
    const dispo = String(headers?.['content-disposition'] || '')
    const m = dispo.match(/filename="?([^";]+)"?/)
    const filename = m?.[1] || `trace_${apiKeyId}_${date}_${sessionId}.tar.zst`
    const url = URL.createObjectURL(data as Blob)
    const a = document.createElement('a')
    a.href = url
    a.download = filename
    a.click()
    URL.revokeObjectURL(url)
  }
}

// ---------- 账号用量导出 ----------

export interface AccountUsageRow {
  account_id: number
  account_name: string
  period: string
  model: string
  requests: number
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_creation_tokens: number
  cost: number
  cost_known: boolean
  currency: string
  excluded: boolean
}

export interface ExportModelPricing {
  input: number
  output: number
  cache_read: number
  excluded?: boolean
}

export interface ExportPricing {
  currency: string
  models: Record<string, ExportModelPricing>
  aliases?: Record<string, string>
}

export const accountUsageExportAPI = {
  async usage(params: {
    start: string
    end: string
    granularity: string
    account_ids?: number[]
  }) {
    const { data } = await apiClient.get<{
        items: AccountUsageRow[]
        total: number
        total_cost: number
        cost_complete: boolean
        currency: string
        total_requests: number
        total_input: number
        total_output: number
        total_cache: number
      }>('/admin/account-usage-export', {
      params: {
        start: params.start,
        end: params.end,
        granularity: params.granularity,
        account_ids: params.account_ids?.length ? params.account_ids.join(',') : undefined
      },
      timeout: 60000
    })
    return data
  },

  async downloadCSV(params: { start: string; end: string; granularity: string; account_ids?: number[] }) {
    const { data } = await apiClient.get('/admin/account-usage-export', {
      params: {
        start: params.start,
        end: params.end,
        granularity: params.granularity,
        account_ids: params.account_ids?.length ? params.account_ids.join(',') : undefined,
        format: 'csv'
      },
      responseType: 'blob',
      timeout: 120000
    })
    const filename = `account-usage_${params.start}_${params.end}.csv`
    const url = URL.createObjectURL(data as Blob)
    const a = document.createElement('a')
    a.href = url
    a.download = filename
    a.click()
    URL.revokeObjectURL(url)
  },

  async getPricing() {
    const { data } = await apiClient.get<{ pricing: ExportPricing; models_seen: Record<string, number> }>('/admin/export-pricing')
    return data
  },

  async savePricing(pricing: ExportPricing) {
    const { data } = await apiClient.put<ExportPricing>('/admin/export-pricing', pricing)
    return data
  }
}
