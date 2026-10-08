import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { AdminUser, ApiKey } from '@/types'
import UserApiKeysModal from '../UserApiKeysModal.vue'

const { getUserApiKeys, getAllGroups, updateApiKey, showError, showSuccess } = vi.hoisted(() => ({
  getUserApiKeys: vi.fn(),
  getAllGroups: vi.fn(),
  updateApiKey: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    users: { getUserApiKeys },
    groups: { getAll: getAllGroups },
    apiKeys: { updateApiKey },
  },
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError, showSuccess }) }))
vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => params?.limit !== undefined ? `${key}: ${params.limit}` : key,
  }),
}))

const createKey = (concurrency = 5): ApiKey => ({
  id: 10, user_id: 1, name: 'Primary key', key: 'sk-test-primary-key',
  group_id: null, status: 'active', ip_whitelist: [], ip_blacklist: [],
  last_used_at: null, last_used_ip: null, quota: 0, quota_used: 0, expires_at: null,
  created_at: '2026-10-08T00:00:00Z', updated_at: '2026-10-08T00:00:00Z',
  concurrency, current_concurrency: 2, rate_limit_5h: 0, rate_limit_1d: 0, rate_limit_7d: 0,
  usage_5h: 0, usage_1d: 0, usage_7d: 0,
  window_5h_start: null, window_1d_start: null, window_7d_start: null,
  reset_5h_at: null, reset_1d_at: null, reset_7d_at: null,
})
const user = { id: 1, email: 'owner@example.com', username: 'owner' } as AdminUser

const mountModal = async () => {
  const wrapper = mount(UserApiKeysModal, {
    props: { show: false, user },
    global: {
      stubs: {
        BaseDialog: {
          props: ['show'], emits: ['close'],
          template: '<div v-if="show" role="dialog"><button data-test="close" @click="$emit(\'close\')">Close</button><slot /></div>',
        },
        GroupBadge: true,
        GroupOptionItem: true,
      },
    },
  })
  await wrapper.setProps({ show: true })
  await flushPromises()
  return wrapper
}

beforeEach(() => {
  vi.clearAllMocks()
  getUserApiKeys.mockResolvedValue({ items: [createKey()] })
  getAllGroups.mockResolvedValue([])
})

describe('administrator API key concurrency editor', () => {
  it.each(['-1', '1.5', '2147483648', ''])('rejects invalid cap %s before calling the API', async (value) => {
    const wrapper = await mountModal()
    await wrapper.get('input[type="number"]').setValue(value)
    expect(wrapper.get('input').attributes('aria-invalid')).toBe('true')
    expect(wrapper.get('button[type="submit"]').attributes('disabled')).toBeDefined()
    await wrapper.get('form').trigger('submit')
    expect(updateApiKey).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it.each([3, 0, 2147483647])('saves cap %s and uses the returned key as current state', async (cap) => {
    const wrapper = await mountModal()
    updateApiKey.mockResolvedValue({ api_key: createKey(cap), auto_granted_group_access: false })
    await wrapper.get('input').setValue(String(cap))
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(updateApiKey).toHaveBeenCalledWith(10, { concurrency: cap })
    expect(wrapper.get('input').element.value).toBe(String(cap))
    expect(wrapper.get('button[type="submit"]').attributes('disabled')).toBeDefined()
    expect(wrapper.text()).toContain(`admin.users.apiKeyConcurrencyCurrent: ${cap > 0 ? cap : 'keys.unlimited'}`)
    expect(showSuccess).toHaveBeenCalledWith('admin.users.apiKeyConcurrencySaved')
    wrapper.unmount()
  })

  it('retains the failed draft and persisted cap and permits a retry', async () => {
    const wrapper = await mountModal()
    updateApiKey.mockRejectedValueOnce(new Error('Server unavailable'))
    await wrapper.get('input').setValue('7')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.get('input').element.value).toBe('7')
    expect(wrapper.text()).toContain('admin.users.apiKeyConcurrencyCurrent: 5')
    expect(wrapper.get('button[type="submit"]').attributes('disabled')).toBeUndefined()
    expect(showError).toHaveBeenCalledWith('Server unavailable')
    updateApiKey.mockResolvedValueOnce({ api_key: createKey(7), auto_granted_group_access: false })
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('admin.users.apiKeyConcurrencyCurrent: 7')
    wrapper.unmount()
  })

  it('blocks duplicate saves and group changes until the save completes', async () => {
    const { promise, resolve } = Promise.withResolvers<unknown>()
    updateApiKey.mockReturnValue(promise)
    const wrapper = await mountModal()
    await wrapper.get('input').setValue('7')
    await wrapper.get('form').trigger('submit')
    await wrapper.get('form').trigger('submit')
    expect(updateApiKey).toHaveBeenCalledTimes(1)
    expect(wrapper.get('input').attributes('disabled')).toBeDefined()
    expect(wrapper.get('button[type="submit"]').attributes('disabled')).toBeDefined()
    expect(wrapper.findAll('button').find((button) => button.text().includes('admin.users.none'))?.attributes('disabled')).toBeDefined()
    resolve({ api_key: createKey(7), auto_granted_group_access: false })
    await flushPromises()
    wrapper.unmount()
  })

  it('ignores an old save response after switching users', async () => {
    const { promise, resolve } = Promise.withResolvers<unknown>()
    updateApiKey.mockReturnValue(promise)
    const wrapper = await mountModal()
    await wrapper.get('input').setValue('7')
    await wrapper.get('form').trigger('submit')
    getUserApiKeys.mockResolvedValueOnce({ items: [{ ...createKey(9), user_id: 2 }] })
    await wrapper.setProps({ user: { ...user, id: 2, email: 'other@example.com' } })
    await flushPromises()
    resolve({ api_key: createKey(7), auto_granted_group_access: false })
    await flushPromises()
    expect(wrapper.get('input').element.value).toBe('9')
    expect(wrapper.text()).toContain('admin.users.apiKeyConcurrencyCurrent: 9')
    expect(showSuccess).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('ignores errors from a save after the modal closes', async () => {
    const { promise, reject } = Promise.withResolvers<unknown>()
    updateApiKey.mockReturnValue(promise)
    const wrapper = await mountModal()
    await wrapper.get('input').setValue('7')
    await wrapper.get('form').trigger('submit')
    await wrapper.get('[data-test="close"]').trigger('click')
    expect(wrapper.emitted('close')).toHaveLength(1)
    reject(new Error('Stale error'))
    await flushPromises()
    expect(showError).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
