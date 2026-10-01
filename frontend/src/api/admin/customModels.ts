/**
 * Custom Models API endpoints (admin only)
 * Handles custom model management operations
 */

import { apiClient } from '../client'
import type { BasePaginationResponse } from '@/types'

export interface CustomModel {
  id: number
  model_id: string
  upstream_group_id: number
  upstream_model: string
  system_prompt: string | null
  injection_mode: string
  description: string | null
  enabled: boolean
  downstream_groups: number[]
  created_at: string
  updated_at: string
}

export interface CreateCustomModelRequest {
  model_id: string
  upstream_group_id: number
  upstream_model: string
  system_prompt?: string
  injection_mode?: string
  description?: string
  enabled?: boolean
  downstream_groups: number[]
}

export interface UpdateCustomModelRequest {
  upstream_group_id?: number
  upstream_model?: string
  system_prompt?: string
  injection_mode?: string
  description?: string
  enabled?: boolean
  downstream_groups?: number[]
}

export interface ListCustomModelsParams {
  page?: number
  page_size?: number
  enabled?: boolean
  search?: string
}

/**
 * List all custom models with pagination
 */
export async function listCustomModels(
  params?: ListCustomModelsParams
): Promise<BasePaginationResponse<CustomModel>> {
  const { data } = await apiClient.get<BasePaginationResponse<CustomModel>>(
    '/admin/custom-models',
    { params }
  )
  return data
}

/**
 * Get a single custom model by ID
 */
export async function getCustomModel(id: number): Promise<CustomModel> {
  const { data } = await apiClient.get<CustomModel>(`/admin/custom-models/${id}`)
  return data
}

/**
 * Create a new custom model
 */
export async function createCustomModel(
  request: CreateCustomModelRequest
): Promise<CustomModel> {
  const { data } = await apiClient.post<CustomModel>('/admin/custom-models', request)
  return data
}

/**
 * Update an existing custom model
 */
export async function updateCustomModel(
  id: number,
  request: UpdateCustomModelRequest
): Promise<CustomModel> {
  const { data } = await apiClient.put<CustomModel>(`/admin/custom-models/${id}`, request)
  return data
}

/**
 * Delete a custom model
 */
export async function deleteCustomModel(id: number): Promise<void> {
  await apiClient.delete(`/admin/custom-models/${id}`)
}

export const customModelsAPI = {
  listCustomModels,
  getCustomModel,
  createCustomModel,
  updateCustomModel,
  deleteCustomModel
}

export default customModelsAPI
