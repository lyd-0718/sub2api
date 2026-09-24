import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import OpenRouterRoutingModal from '../OpenRouterRoutingModal.vue'
import type { Account } from '@/types'

const { getOpenRouterRouting, updateOpenRouterRouting, showSuccess, showError } = vi.hoisted(() => ({
  getOpenRouterRouting: vi.fn(),
  updateOpenRouterRouting: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin/openrouterRouting', () => ({ getOpenRouterRouting, updateOpenRouterRouting }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess, showError }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const BaseDialogStub = defineComponent({
  props: { show: Boolean, title: String },
  setup(props, { slots }) {
    return () => (props.show ? h('div', [slots.default?.(), slots.footer?.()]) : null)
  }
})

// 下拉框桩：渲染成原生 select，便于断言选项与模拟选择。
const SelectStub = defineComponent({
  props: { modelValue: { type: [String, null], default: null }, options: { type: Array, default: () => [] }, disabled: Boolean },
  emits: ['update:modelValue', 'change'],
  setup(props, { emit }) {
    return () =>
      h(
        'select',
        {
          'data-test': 'select',
          value: props.modelValue ?? '',
          disabled: props.disabled,
          onChange: (e: Event) => {
            const value = (e.target as HTMLSelectElement).value || null
            emit('update:modelValue', value)
            emit('change', value)
          }
        },
        [h('option', { value: '' }, '-'), ...(props.options as Array<{ value: string; label: string }>).map((o) => h('option', { value: o.value }, o.label))]
      )
  }
})

const account = {
  id: 22,
  name: 'openrouter',
  platform: 'openrouter',
  type: 'apikey',
  credentials: { model_mapping: { 'z-ai/glm-5.3-flash': 'z-ai/glm-5.3-flash' } }
} as unknown as Account

const provider = (slug: string, name: string) => ({
  slug,
  name,
  status: 0,
  uptime_last_1d: 99.9,
  prompt_price: 0.09,
  completion_price: 0.35,
  cache_read_price: 0.03,
  supports_tools: true
})

function mountModal() {
  return mount(OpenRouterRoutingModal, {
    props: { show: true, account },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: SelectStub,
        Toggle: true,
        LoadingSpinner: true
      }
    }
  })
}

describe('OpenRouterRoutingModal', () => {
  beforeEach(() => {
    getOpenRouterRouting.mockReset()
    updateOpenRouterRouting.mockReset()
    showSuccess.mockReset()
    showError.mockReset()
  })

  it('loads providers per model and saves primary / backup order', async () => {
    getOpenRouterRouting.mockResolvedValue({
      account_id: 22,
      routing: { enabled: true, models: { 'z-ai/glm-5.3-flash': ['wafer'], 'z-ai/removed': ['relace'] } },
      models: [
        { model: 'z-ai/glm-5.3-flash', providers: [provider('wafer', 'Wafer'), provider('relace', 'Relace')] },
        { model: 'z-ai/removed', providers: [] }
      ]
    })
    updateOpenRouterRouting.mockResolvedValue({ enabled: true, models: {} })

    const wrapper = mountModal()
    await flushPromises()

    expect(getOpenRouterRouting).toHaveBeenCalledWith(22)
    expect(wrapper.text()).toContain('z-ai/glm-5.3-flash')
    expect(wrapper.text()).toContain('admin.accounts.openrouterRouting.notMapped')

    const selects = wrapper.findAll('[data-test="select"]')
    expect(selects).toHaveLength(4)
    // 备选下拉不包含已选的首选
    expect(selects[1].findAll('option').map((o) => o.attributes('value'))).toEqual(['', 'relace'])
    await selects[1].setValue('relace')
    // 清空第二个模型（已从映射删除）的首选
    await selects[2].setValue('')

    await wrapper.findAll('button').at(-1)!.trigger('click')
    await flushPromises()

    expect(updateOpenRouterRouting).toHaveBeenCalledWith(22, {
      enabled: true,
      models: { 'z-ai/glm-5.3-flash': ['wafer', 'relace'] }
    })
    expect(showSuccess).toHaveBeenCalled()
    expect(wrapper.emitted('saved')).toBeTruthy()
  })

  it('keeps a saved provider that is no longer listed', async () => {
    getOpenRouterRouting.mockResolvedValue({
      account_id: 22,
      routing: { enabled: true, models: { 'z-ai/glm-5.3-flash': ['gone-provider'] } },
      models: [{ model: 'z-ai/glm-5.3-flash', providers: [provider('wafer', 'Wafer')], error: '' }]
    })

    const wrapper = mountModal()
    await flushPromises()

    const primary = wrapper.findAll('[data-test="select"]')[0]
    expect(primary.findAll('option').map((o) => o.attributes('value'))).toContain('gone-provider')
  })

  it('shows the load error and disables saving', async () => {
    getOpenRouterRouting.mockRejectedValue(new Error('boom'))

    const wrapper = mountModal()
    await flushPromises()

    expect(wrapper.text()).toContain('boom')
    expect(wrapper.findAll('button').at(-1)!.attributes('disabled')).toBeDefined()
  })
})
