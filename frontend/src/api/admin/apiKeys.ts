/**
 * Admin API Keys API endpoints
 * Handles API key management for administrators
 */

import { apiClient } from '../client'
import type { ApiKey } from '@/types'

export interface UpdateApiKeyResult {
  api_key: ApiKey
  auto_granted_group_access: boolean
  granted_group_id?: number
  granted_group_name?: string
}

export interface AdminUpdateApiKeyRequest {
  group_id?: number // 0 unbinds; omitted leaves the group unchanged
  concurrency?: number // 0 removes the concurrency cap
}

/**
 * Update an API key's administrator-managed settings
 * @param id - API Key ID
 * @param updates - Settings to change; omitted settings remain unchanged
 * @returns Updated API key with auto-grant info
 */
export async function updateApiKey(id: number, updates: AdminUpdateApiKeyRequest): Promise<UpdateApiKeyResult> {
  const { data } = await apiClient.put<UpdateApiKeyResult>(`/admin/api-keys/${id}`, updates)
  return data
}

export const apiKeysAPI = {
  updateApiKey
}

export default apiKeysAPI
