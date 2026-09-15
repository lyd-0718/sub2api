import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { mount } from '@vue/test-utils'

const { getAccountConcurrencyCapMock, authIsSimpleMode } = vi.hoisted(() => ({
  getAccountConcurrencyCapMock: vi.fn(),
  authIsSimpleMode: { value: true }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get isSimpleMode() {
      return authIsSimpleMode.value
    }
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      update: vi.fn(),
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }),
      getAccountConcurrencyCap: getAccountConcurrencyCapMock
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([])
    }
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${JSON.stringify(params)}` : key
    })
  }
})

import EditAccountModal from '../EditAccountModal.vue'
import type { Account } from '@/types'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: {
    show: {
      type: Boolean,
      default: false
    }
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

// 只填充弹窗渲染所需的字段，其余字段不影响本用例断言。
function buildKimiAccount(): Account {
  return {
    id: 5,
    name: 'Kimi',
    notes: '',
    platform: 'kimi',
    type: 'apikey',
    credentials: {},
    credentials_status: {},
    extra: {},
    proxy_id: null,
    concurrency: 10,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as unknown as Account
}

function mountModal(account: Account) {
  return mount(EditAccountModal, {
    props: {
      show: true,
      account,
      proxies: [],
      groups: []
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: true,
        Icon: true,
        ProxySelector: true,
        GroupSelector: true,
        ModelWhitelistSelector: true
      }
    }
  })
}

describe('EditAccountModal concurrency cap display', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    getAccountConcurrencyCapMock.mockReset()
  })

  it('renders the effective concurrency cap alongside the configured concurrency', async () => {
    getAccountConcurrencyCapMock.mockResolvedValue({
      account_id: 5,
      platform: 'kimi',
      cap_max: 3,
      known: true,
      cap: 1,
      restricted: true,
      pinned: false,
      reason: 'concurrency_403',
      configured_concurrency: 10,
      effective_concurrency: 1,
      flap_count_7d: 0,
      fuse_flap_threshold: 3,
      fused: false,
      version: 2
    })

    const wrapper = mountModal(buildKimiAccount())
    await vi.waitFor(() =>
      expect(wrapper.find('[data-testid="account-concurrency-cap"]').exists()).toBe(true)
    )

    const text = wrapper.get('[data-testid="account-concurrency-cap"]').text()
    expect(text).toContain('"effective":1')
    expect(text).toContain('"configured":10')
    expect(text).toContain('admin.accounts.concurrencyCapRestricted')
  })

  it('surfaces pinned, fused and next probe markers', async () => {
    getAccountConcurrencyCapMock.mockResolvedValue({
      account_id: 5,
      platform: 'kimi',
      cap_max: 3,
      known: true,
      cap: 2,
      restricted: true,
      pinned: true,
      reason: 'probe_pass',
      next_probe_at: '2026-09-16T08:00:00Z',
      configured_concurrency: 10,
      effective_concurrency: 2,
      flap_count_7d: 3,
      fuse_flap_threshold: 3,
      fused: true,
      version: 4
    })

    const wrapper = mountModal(buildKimiAccount())
    await vi.waitFor(() =>
      expect(wrapper.find('[data-testid="account-concurrency-cap"]').exists()).toBe(true)
    )

    const text = wrapper.get('[data-testid="account-concurrency-cap"]').text()
    expect(text).toContain('admin.accounts.concurrencyCapPinned')
    expect(text).toContain('"count":3')
    expect(text).toContain('admin.accounts.concurrencyCapNextProbe')
  })

  it('keeps the modal usable when the cap endpoint is unavailable', async () => {
    getAccountConcurrencyCapMock.mockRejectedValue(new Error('endpoint missing'))

    const wrapper = mountModal(buildKimiAccount())
    await vi.waitFor(() => expect(getAccountConcurrencyCapMock).toHaveBeenCalledWith(5))

    expect(wrapper.find('[data-testid="account-concurrency-cap"]').exists()).toBe(false)
    expect(wrapper.get('form#edit-account-form').exists()).toBe(true)
  })
})
