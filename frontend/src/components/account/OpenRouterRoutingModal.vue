<template>
  <BaseDialog
    :show="show"
    :title="dialogTitle"
    width="extra-wide"
    @close="emit('close')"
  >
    <div class="space-y-4">
      <p class="text-sm text-gray-600 dark:text-gray-400">
        {{ t('admin.accounts.openrouterRouting.description') }}
      </p>

      <div class="flex items-center justify-between rounded-lg border border-gray-200 px-4 py-3 dark:border-dark-600">
        <div>
          <div class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.accounts.openrouterRouting.enabled') }}
          </div>
          <div class="text-xs text-gray-500 dark:text-gray-400">
            {{ t('admin.accounts.openrouterRouting.enabledHint') }}
          </div>
        </div>
        <Toggle v-model="enabled" />
      </div>

      <div v-if="loading" class="flex items-center gap-2 py-8 text-sm text-gray-500 dark:text-gray-400">
        <LoadingSpinner size="sm" />
        {{ t('admin.accounts.openrouterRouting.loading') }}
      </div>

      <div v-else-if="loadError" class="rounded-lg bg-red-50 px-4 py-3 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-400">
        {{ loadError }}
      </div>

      <div v-else-if="rows.length === 0" class="py-6 text-sm text-gray-500 dark:text-gray-400">
        {{ t('admin.accounts.openrouterRouting.noModels') }}
      </div>

      <div v-else class="overflow-x-auto">
        <table class="w-full min-w-[720px] text-sm" :class="{ 'opacity-60': !enabled }">
          <thead>
            <tr class="text-left text-xs text-gray-500 dark:text-gray-400">
              <th class="w-1/4 pb-2 pr-3 font-medium">{{ t('admin.accounts.openrouterRouting.model') }}</th>
              <th class="pb-2 pr-3 font-medium">{{ t('admin.accounts.openrouterRouting.primary') }}</th>
              <th class="pb-2 font-medium">{{ t('admin.accounts.openrouterRouting.backup') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="row in rows" :key="row.model" class="border-t border-gray-100 align-top dark:border-dark-700">
              <td class="py-3 pr-3">
                <div class="break-all font-mono text-xs text-gray-900 dark:text-gray-100">{{ row.model }}</div>
                <div v-if="!row.mapped" class="mt-1 text-xs text-amber-600 dark:text-amber-400">
                  {{ t('admin.accounts.openrouterRouting.notMapped') }}
                </div>
                <div v-if="row.error" class="mt-1 text-xs text-red-600 dark:text-red-400">
                  {{ t('admin.accounts.openrouterRouting.providerError', { error: row.error }) }}
                </div>
              </td>
              <td class="py-3 pr-3">
                <Select
                  v-model="row.primary"
                  :options="optionsFor(row)"
                  :placeholder="t('admin.accounts.openrouterRouting.notSet')"
                  :disabled="!enabled"
                  clearable
                  searchable
                  @change="onPrimaryChange(row)"
                />
              </td>
              <td class="py-3">
                <Select
                  v-model="row.backup"
                  :options="optionsFor(row, row.primary)"
                  :placeholder="t('admin.accounts.openrouterRouting.notSet')"
                  :disabled="!enabled || !row.primary"
                  clearable
                  searchable
                />
              </td>
            </tr>
          </tbody>
        </table>
        <p class="mt-3 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.accounts.openrouterRouting.fallbackNote') }}
        </p>
      </div>
    </div>

    <template #footer>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" @click="emit('close')">
          {{ t('common.cancel') }}
        </button>
        <button type="button" class="btn btn-primary" :disabled="loading || saving || !!loadError" @click="save">
          {{ saving ? t('common.saving') : t('common.save') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import {
  getOpenRouterRouting,
  updateOpenRouterRouting,
  type OpenRouterProviderOption
} from '@/api/admin/openrouterRouting'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import type { Account } from '@/types'

const props = defineProps<{
  show: boolean
  account: Account | null
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'saved'): void
}>()

const { t } = useI18n()
const appStore = useAppStore()

interface RoutingRow {
  model: string
  /** 是否仍在账号模型映射里（已从映射删除但还留有配置的模型也展示出来，方便清理） */
  mapped: boolean
  providers: OpenRouterProviderOption[]
  error?: string
  primary: string | null
  backup: string | null
}

// 标题带上账号名与 ID：同一后台常有多个 OpenRouter 账号，名字相近时容易看错改错。
const dialogTitle = computed(() => {
  const base = t('admin.accounts.openrouterRouting.title')
  return props.account ? `${base} · ${props.account.name} (#${props.account.id})` : base
})

const loading = ref(false)
const saving = ref(false)
// 每次加载递增；只采用最后一次加载的响应，避免快速切换账号时旧响应覆盖新账号的数据。
let loadSeq = 0
const loadError = ref('')
const enabled = ref(false)
const rows = ref<RoutingRow[]>([])

function formatPrice(value?: number): string {
  if (value === undefined || value === null) return '-'
  return `$${Number(value.toPrecision(3))}`
}

function providerLabel(p: OpenRouterProviderOption): string {
  const parts = [p.name || p.slug]
  if (p.quantization && p.quantization !== 'unknown') parts.push(p.quantization)
  parts.push(`${formatPrice(p.prompt_price)} / ${formatPrice(p.completion_price)}`)
  if (p.cache_read_price !== undefined) {
    parts.push(`${t('admin.accounts.openrouterRouting.cache')} ${formatPrice(p.cache_read_price)}`)
  }
  if (p.uptime_last_1d !== undefined) {
    parts.push(`${t('admin.accounts.openrouterRouting.uptime')} ${p.uptime_last_1d.toFixed(2)}%`)
  }
  if (p.status !== 0) parts.push(`⚠ ${t('admin.accounts.openrouterRouting.unavailable')}`)
  if (!p.supports_tools) parts.push(t('admin.accounts.openrouterRouting.noTools'))
  return `${parts.join(' · ')}  (${p.slug})`
}

function optionsFor(row: RoutingRow, exclude?: string | null): SelectOption[] {
  const options: SelectOption[] = row.providers
    .filter((p) => p.slug !== exclude)
    .map((p) => ({ value: p.slug, label: providerLabel(p) }))
  // 已保存的 provider 不在当前列表里（下线或拉取失败）时仍保留为可选项，避免打开即丢配置。
  for (const selected of [row.primary, row.backup]) {
    if (selected && selected !== exclude && !options.some((o) => o.value === selected)) {
      options.push({ value: selected, label: `${selected}  (${t('admin.accounts.openrouterRouting.offline')})` })
    }
  }
  return options
}

function onPrimaryChange(row: RoutingRow) {
  if (!row.primary || row.backup === row.primary) {
    row.backup = null
  }
}

async function load(account: Account) {
  const seq = ++loadSeq
  loading.value = true
  loadError.value = ''
  rows.value = []
  try {
    const view = await getOpenRouterRouting(account.id)
    if (seq !== loadSeq) return
    enabled.value = view.routing.enabled
    const configured = view.routing.models || {}
    const mapping = (account.credentials?.model_mapping as Record<string, string> | undefined) || {}
    const mappedTargets = new Set(Object.values(mapping).map((v) => String(v).trim()))
    rows.value = view.models.map((m) => {
      const order = configured[m.model] || []
      return {
        model: m.model,
        mapped: mappedTargets.has(m.model),
        providers: m.providers,
        error: m.error,
        primary: order[0] ?? null,
        backup: order[1] ?? null
      }
    })
  } catch (err) {
    if (seq !== loadSeq) return
    loadError.value = extractApiErrorMessage(err, t('admin.accounts.openrouterRouting.loadFailed'))
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

async function save() {
  if (!props.account) return
  const models: Record<string, string[]> = {}
  for (const row of rows.value) {
    const order = [row.primary, row.backup].filter((v): v is string => !!v)
    if (order.length > 0) models[row.model] = order
  }
  saving.value = true
  try {
    await updateOpenRouterRouting(props.account.id, { enabled: enabled.value, models })
    appStore.showSuccess(t('admin.accounts.openrouterRouting.saved'))
    emit('saved')
    emit('close')
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.accounts.openrouterRouting.saveFailed')))
  } finally {
    saving.value = false
  }
}

watch(
  () => [props.show, props.account?.id] as const,
  ([show]) => {
    if (show && props.account) {
      void load(props.account)
    }
  },
  { immediate: true }
)
</script>
