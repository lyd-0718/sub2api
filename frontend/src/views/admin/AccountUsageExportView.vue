<template>
  <AppLayout>
    <div class="space-y-6">
      <!-- 页头 + Tab -->
      <div class="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ t('admin.accountExport.title') }}</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.accountExport.description') }}</p>
        </div>
        <div class="flex rounded-xl bg-gray-100 p-1 text-sm dark:bg-dark-700">
          <button
            @click="tab = 'export'"
            :class="tab === 'export' ? tabActive : tabIdle"
          >{{ t('admin.accountExport.tabExport') }}</button>
          <button
            @click="tab = 'pricing'"
            :class="tab === 'pricing' ? tabActive : tabIdle"
          >{{ t('admin.accountExport.tabPricing') }}</button>
        </div>
      </div>

      <!-- ============ 用量导出 Tab ============ -->
      <div v-if="tab === 'export'" class="space-y-5">
        <!-- 筛选卡片：账号 / 时间 / 粒度 同一行 -->
        <div class="card p-5">
          <div class="flex flex-wrap items-end gap-x-6 gap-y-4">
            <!-- 聚合维度 -->
            <div>
              <div class="mb-2 text-xs font-medium uppercase tracking-wide text-gray-400 dark:text-gray-500">{{ t('admin.accountExport.dimension') }}</div>
              <div class="flex rounded-xl bg-gray-100 p-1 text-sm dark:bg-dark-700">
                <button @click="dimension = 'account'; loadUsage()" :class="dimension === 'account' ? tabActiveSm : tabIdleSm">{{ t('admin.accountExport.dimByAccount') }}</button>
                <button @click="dimension = 'model'; loadUsage()" :class="dimension === 'model' ? tabActiveSm : tabIdleSm">{{ t('admin.accountExport.dimAggregated') }}</button>
              </div>
            </div>
            <!-- 账号多选下拉（聚合模式下无意义，禁用） -->
            <div class="relative" :class="{ 'opacity-40 pointer-events-none': dimension === 'model' }">
              <div class="mb-2 text-xs font-medium uppercase tracking-wide text-gray-400 dark:text-gray-500">{{ t('admin.accountExport.accounts') }}</div>
              <button
                @click="showAccountDD = !showAccountDD"
                class="flex items-center gap-2 rounded-xl border border-gray-200 bg-white px-3.5 py-2 text-sm text-gray-700 transition-colors hover:border-gray-300 dark:border-dark-600 dark:bg-dark-800 dark:text-gray-200 dark:hover:border-dark-500"
              >
                <span>{{ accountTriggerLabel }}</span>
                <span class="text-xs text-gray-400">▾</span>
              </button>
              <div v-if="showAccountDD" class="absolute z-20 mt-2 w-64 rounded-xl border border-gray-100 bg-white p-2 shadow-lg dark:border-dark-600 dark:bg-dark-800">
                <label
                  v-for="acc in accounts"
                  :key="acc.id"
                  class="flex cursor-pointer items-center gap-2.5 rounded-lg px-3 py-2 text-sm hover:bg-gray-100 dark:hover:bg-dark-700"
                >
                  <input type="checkbox" :value="acc.id" v-model="selectedAccountIds" class="h-4 w-4 rounded accent-teal-500" @change="loadUsage" />
                  <span class="flex-1 text-gray-700 dark:text-gray-200">{{ acc.name }}</span>
                </label>
                <div class="mt-1.5 flex justify-between border-t border-gray-100 px-2 pb-1 pt-1.5 dark:border-dark-600">
                  <button @click="selectAllAccounts" class="text-xs text-primary-600 hover:underline dark:text-primary-400">{{ t('common.selectAll') }}</button>
                  <button @click="clearAccounts" class="text-xs text-gray-400 hover:underline">{{ t('common.clear') }}</button>
                </div>
              </div>
            </div>
            <!-- 时间范围（仪表盘同款 DateRangePicker） -->
            <div>
              <div class="mb-2 text-xs font-medium uppercase tracking-wide text-gray-400 dark:text-gray-500">{{ t('admin.accountExport.timeRange') }}</div>
              <DateRangePicker v-model:start-date="startDate" v-model:end-date="endDate" @change="loadUsage" />
            </div>
            <!-- 粒度 -->
            <div>
              <div class="mb-2 text-xs font-medium uppercase tracking-wide text-gray-400 dark:text-gray-500">{{ t('admin.accountExport.granularity') }}</div>
              <div class="w-32">
                <Select v-model="granularity" :options="granularityOptions" @change="loadUsage" />
              </div>
            </div>
          </div>
        </div>

        <!-- 预览表格 -->
        <div class="card overflow-hidden">
          <div class="flex items-center justify-between border-b border-gray-100 px-5 py-4 dark:border-dark-700/50">
            <div class="text-sm font-medium text-gray-900 dark:text-white">
              {{ t('admin.accountExport.preview') }}
              <span class="font-normal text-gray-400">· {{ startDate }} ~ {{ endDate }}</span>
            </div>
            <div v-if="!costComplete" class="text-xs text-amber-600 dark:text-amber-400">
              {{ t('admin.accountExport.unpricedWarning') }}
            </div>
          </div>
          <div class="overflow-x-auto">
            <table class="w-full text-sm">
              <thead>
                <tr class="border-b border-gray-100 text-left text-xs text-gray-400 dark:border-dark-700/50 dark:text-gray-500">
                  <th v-if="dimension === 'account'" class="px-5 py-3 font-medium">{{ t('admin.accountExport.colAccount') }}</th>
                  <th class="px-4 py-3 font-medium">{{ t('admin.accountExport.colPeriod') }}</th>
                  <th class="px-4 py-3 font-medium">{{ t('admin.accountExport.colModel') }}</th>
                  <th class="px-4 py-3 font-medium text-right">{{ t('admin.accountExport.colRequests') }}</th>
                  <th class="px-4 py-3 font-medium text-right">{{ t('admin.accountExport.colInput') }}</th>
                  <th class="px-4 py-3 font-medium text-right">{{ t('admin.accountExport.colOutput') }}</th>
                  <th class="px-4 py-3 font-medium text-right">{{ t('admin.accountExport.colCache') }}</th>
                  <th class="px-5 py-3 font-medium text-right">{{ t('admin.accountExport.colCost') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-50 dark:divide-dark-700/30">
                <tr v-if="loadingUsage">
                  <td :colspan="dimension === 'account' ? 8 : 7" class="px-4 py-10 text-center text-gray-400">{{ t('common.loading') }}</td>
                </tr>
                <tr v-else-if="visibleRows.length === 0">
                  <td :colspan="dimension === 'account' ? 8 : 7" class="px-4 py-10 text-center text-gray-400">{{ t('admin.accountExport.empty') }}</td>
                </tr>
                <!-- 排除的模型整行不显示（含合计口径），与 CSV 一致 -->
                <tr v-else v-for="(r, i) in visibleRows" :key="i" class="hover:bg-gray-50/60 dark:hover:bg-dark-700/20">
                  <td v-if="dimension === 'account'" class="px-5 py-3 font-medium text-gray-900 dark:text-white">{{ r.account_name }}</td>
                  <td class="px-4 py-3 tabular-nums text-gray-500 dark:text-gray-400">{{ r.period }}</td>
                  <td class="px-4 py-3">
                    <span class="rounded-md bg-teal-50 px-2 py-0.5 text-xs font-medium text-teal-700 dark:bg-teal-500/10 dark:text-teal-300">{{ r.model }}</span>
                  </td>
                  <td class="px-4 py-3 text-right tabular-nums">{{ formatNumber(r.requests) }}</td>
                  <td class="px-4 py-3 text-right tabular-nums">{{ compactTokens(r.input_tokens) }}</td>
                  <td class="px-4 py-3 text-right tabular-nums">{{ compactTokens(r.output_tokens) }}</td>
                  <td class="px-4 py-3 text-right tabular-nums">{{ compactTokens(r.cache_read_tokens + r.cache_creation_tokens) }}</td>
                  <td class="px-5 py-3 text-right tabular-nums font-medium text-gray-900 dark:text-white">
                    <template v-if="r.cost_known">{{ currencySymbol }}{{ r.cost.toFixed(2) }}</template>
                    <span v-else class="text-gray-300 dark:text-gray-600" :title="t('admin.accountExport.unpriced')">-</span>
                  </td>
                </tr>
              </tbody>
              <tfoot v-if="visibleRows.length > 0">
                <tr class="border-t-2 border-gray-200 bg-gray-50/60 dark:border-dark-600 dark:bg-dark-700/20">
                  <td class="px-5 py-3.5 font-semibold text-gray-900 dark:text-white" :colspan="dimension === 'account' ? 3 : 2">{{ t('admin.accountExport.total') }}</td>
                  <td class="px-4 py-3.5 text-right tabular-nums font-semibold text-gray-900 dark:text-white">{{ formatNumber(totalRequests) }}</td>
                  <td class="px-4 py-3.5 text-right tabular-nums font-semibold text-gray-700 dark:text-gray-300">{{ compactTokens(totalInput) }}</td>
                  <td class="px-4 py-3.5 text-right tabular-nums font-semibold text-gray-700 dark:text-gray-300">{{ compactTokens(totalOutput) }}</td>
                  <td class="px-4 py-3.5 text-right tabular-nums font-semibold text-gray-700 dark:text-gray-300">{{ compactTokens(totalCache) }}</td>
                  <td class="px-5 py-3.5 text-right tabular-nums text-base font-semibold text-primary-600 dark:text-primary-400">
                    {{ costComplete ? currencySymbol + totalCost.toFixed(2) : '-' }}
                  </td>
                </tr>
              </tfoot>
            </table>
          </div>
          <div class="flex items-center justify-between border-t border-gray-100 px-5 py-4 dark:border-dark-700/50">
            <div class="text-xs text-gray-400">{{ t('admin.accountExport.csvHint') }}</div>
            <button @click="exportCSV" :disabled="exporting || usageRows.length === 0" class="btn btn-primary">
              {{ exporting ? t('admin.accountExport.exporting') : t('admin.accountExport.exportCSV') }}
            </button>
          </div>
        </div>
      </div>

      <!-- ============ 模型定价 Tab ============ -->
      <div v-else class="space-y-5">
        <div class="card flex flex-wrap items-center justify-between gap-4 p-5">
          <div>
            <div class="text-sm font-medium text-gray-900 dark:text-white">{{ t('admin.accountExport.pricingTitle') }}</div>
            <p class="mt-1 text-xs text-gray-400">{{ t('admin.accountExport.pricingHint') }}</p>
          </div>
          <div class="flex items-center gap-3">
            <span class="text-xs text-gray-400">{{ t('admin.accountExport.currency') }}</span>
            <div class="flex rounded-xl bg-gray-100 p-1 text-sm dark:bg-dark-700">
              <button @click="pricingForm.currency = 'USD'; markDirty()" :class="pricingForm.currency === 'USD' ? tabActive : tabIdle">USD $</button>
              <button @click="pricingForm.currency = 'CNY'; markDirty()" :class="pricingForm.currency === 'CNY' ? tabActive : tabIdle">CNY ¥</button>
            </div>
          </div>
        </div>

        <div class="card overflow-hidden">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-gray-100 text-left text-xs text-gray-400 dark:border-dark-700/50 dark:text-gray-500">
                <th class="px-5 py-3 font-medium">{{ t('admin.accountExport.colModel') }}</th>
                <th class="px-4 py-3 font-medium text-right">{{ t('admin.accountExport.priceInput') }}</th>
                <th class="px-4 py-3 font-medium text-right">{{ t('admin.accountExport.priceOutput') }}</th>
                <th class="px-4 py-3 font-medium text-right">{{ t('admin.accountExport.priceCache') }}</th>
                <th class="px-5 py-3 font-medium text-right">{{ t('admin.accountExport.calls90d') }}</th>
                <th class="px-4 py-3 font-medium text-center">{{ t('admin.accountExport.includeInExport') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-50 dark:divide-dark-700/30">
              <tr v-if="pricingModels.length === 0">
                <td colspan="5" class="px-4 py-10 text-center text-gray-400">{{ t('common.loading') }}</td>
              </tr>
              <tr v-for="m in pricingModels" :key="m" class="hover:bg-gray-50/60 dark:hover:bg-dark-700/20" :class="{ 'opacity-60': !pricingForm.models[m]?.input && !pricingForm.models[m]?.output && !pricingForm.models[m]?.cache_read }">
                <td class="px-5 py-4">
                  <span class="rounded-md bg-teal-50 px-2 py-0.5 text-xs font-medium text-teal-700 dark:bg-teal-500/10 dark:text-teal-300">{{ m }}</span>
                  <div class="mt-1 text-xs text-gray-400">{{ t('admin.accountExport.callsHint', { n: modelsSeen[m] ?? 0 }) }}</div>
                </td>
                <td v-for="field in (['input', 'output', 'cache_read'] as const)" :key="field" class="px-4 py-4 text-right">
                  <div class="inline-flex items-center gap-1">
                    <span class="text-xs text-gray-400">{{ currencySymbol }}</span>
                    <input
                      type="number"
                      step="0.01"
                      min="0"
                      :value="pricingForm.models[m]?.[field] || ''"
                      @input="setPrice(m, field, ($event.target as HTMLInputElement).value)"
                      :placeholder="t('admin.accountExport.unset')"
                      class="w-24 rounded-lg border border-gray-200 bg-white px-2.5 py-1.5 text-right text-sm tabular-nums focus:border-primary-500 focus:outline-none focus:ring-2 focus:ring-primary-500/30 dark:border-dark-600 dark:bg-dark-800"
                      :class="{ 'border-dashed': !pricingForm.models[m]?.[field] }"
                    />
                  </div>
                </td>
                <td class="px-5 py-4 text-right tabular-nums text-gray-400">{{ formatNumber(modelsSeen[m] ?? 0) }}</td>
                <td class="px-4 py-4 text-center">
                  <input
                    type="checkbox"
                    :checked="!pricingForm.models[m]?.excluded"
                    @change="setExcluded(m, !($event.target as HTMLInputElement).checked)"
                    class="h-4 w-4 rounded accent-teal-500"
                    :title="t('admin.accountExport.includeInExportHint')"
                  />
                </td>
              </tr>
            </tbody>
          </table>
          <!-- 粘性保存条：有改动才出现 -->
          <div
            v-if="pricingDirty"
            class="flex items-center justify-between border-t border-gray-100 bg-primary-500/[.04] px-5 py-4 dark:border-dark-700/50 dark:bg-primary-500/[.06]"
          >
            <div class="flex items-center gap-2 text-xs text-primary-700 dark:text-primary-300">
              <span class="inline-block h-1.5 w-1.5 rounded-full bg-primary-500"></span>
              {{ t('admin.accountExport.unsavedPricing') }}
            </div>
            <div class="flex gap-2">
              <button @click="resetPricing" class="btn btn-ghost btn-sm">{{ t('common.reset') }}</button>
              <button @click="savePricing" :disabled="savingPricing" class="btn btn-primary btn-sm">
                {{ savingPricing ? t('common.saving') : t('admin.accountExport.savePricing') }}
              </button>
            </div>
          </div>
        </div>
        <!-- 模型别名归并 -->
        <div class="card p-5">
          <div class="text-sm font-medium text-gray-900 dark:text-white">{{ t('admin.accountExport.aliasTitle') }}</div>
          <p class="mt-1 text-xs text-gray-400">{{ t('admin.accountExport.aliasHint') }}</p>
          <div class="mt-3 space-y-2">
            <div
              v-for="(target, source) in pricingForm.aliases"
              :key="source"
              class="flex items-center gap-2 text-sm"
            >
              <span class="rounded-md bg-gray-100 px-2 py-0.5 font-mono text-xs text-gray-600 dark:bg-dark-700 dark:text-gray-300">{{ source }}</span>
              <span class="text-gray-400">→</span>
              <span class="rounded-md bg-teal-50 px-2 py-0.5 font-mono text-xs font-medium text-teal-700 dark:bg-teal-500/10 dark:text-teal-300">{{ target }}</span>
              <button @click="removeAlias(String(source))" class="ml-1 text-xs text-red-500 hover:underline">{{ t('common.delete') }}</button>
            </div>
            <div class="flex items-center gap-2 pt-1">
              <input v-model.trim="aliasFrom" type="text" placeholder="k3" class="w-36 rounded-lg border border-gray-200 bg-white px-2.5 py-1.5 font-mono text-xs focus:border-primary-500 focus:outline-none dark:border-dark-600 dark:bg-dark-800" />
              <span class="text-gray-400">→</span>
              <input v-model.trim="aliasTo" type="text" placeholder="kimi-k3" class="w-36 rounded-lg border border-gray-200 bg-white px-2.5 py-1.5 font-mono text-xs focus:border-primary-500 focus:outline-none dark:border-dark-600 dark:bg-dark-800" />
              <button @click="addAlias" :disabled="!aliasFrom || !aliasTo" class="btn btn-secondary btn-sm">{{ t('common.add') }}</button>
            </div>
          </div>
        </div>
        <p class="px-1 text-xs text-gray-400">{{ t('admin.accountExport.pricingNote') }}</p>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Select from '@/components/common/Select.vue'
import DateRangePicker from '@/components/common/DateRangePicker.vue'
import { useAppStore } from '@/stores/app'
import { accountUsageExportAPI, type AccountUsageRow, type ExportPricing } from '@/api/traceAdmin'
import { list as listAccounts } from '@/api/admin/accounts'

const { t } = useI18n()
const appStore = useAppStore()

const tabActive = 'rounded-lg bg-white px-4 py-1.5 font-medium text-gray-900 shadow-sm dark:bg-dark-800 dark:text-white'
const tabIdle = 'rounded-lg px-4 py-1.5 font-medium text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200'

const tab = ref<'export' | 'pricing'>('export')
const dimension = ref<'account' | 'model'>('account')
const tabActiveSm = 'rounded-lg bg-white px-3.5 py-1.5 text-sm font-medium text-gray-900 shadow-sm dark:bg-dark-800 dark:text-white'
const tabIdleSm = 'rounded-lg px-3.5 py-1.5 text-sm font-medium text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200'

// ---------- 筛选状态 ----------
const accounts = ref<{ id: number; name: string }[]>([])
const selectedAccountIds = ref<number[]>([])
const showAccountDD = ref(false)
const today = new Date()
const startDate = ref(new Date(today.getFullYear(), today.getMonth(), 1).toISOString().slice(0, 10))
const endDate = ref(today.toISOString().slice(0, 10))
const granularity = ref('month')
const granularityOptions = computed(() => [
  { value: 'day', label: t('admin.accountExport.granularityDay') },
  { value: 'week', label: t('admin.accountExport.granularityWeek') },
  { value: 'month', label: t('admin.accountExport.granularityMonth') },
  { value: 'total', label: t('admin.accountExport.granularityTotal') }
])

const accountTriggerLabel = computed(() => {
  if (selectedAccountIds.value.length === 0 || selectedAccountIds.value.length === accounts.value.length) {
    return t('admin.accountExport.allAccounts')
  }
  return t('admin.accountExport.selectedAccounts', { n: selectedAccountIds.value.length, total: accounts.value.length })
})

// ---------- 用量数据 ----------
const usageRows = ref<AccountUsageRow[]>([])
const loadingUsage = ref(false)
const exporting = ref(false)
const totalCost = ref(0)
const costComplete = ref(true)
const currency = ref('CNY')

// 排除的模型页面也不显示；合计直接用后端口径（已剔除排除项）
const visibleRows = computed(() => usageRows.value.filter((r) => !r.excluded))
const totalRequests = ref(0)
const totalInput = ref(0)
const totalOutput = ref(0)
const totalCache = ref(0)

const currencySymbol = computed(() => (currency.value === 'USD' ? '$' : '¥'))

const loadUsage = async () => {
  loadingUsage.value = true
  try {
    const res = await accountUsageExportAPI.usage({
      start: startDate.value,
      end: endDate.value,
      granularity: granularity.value,
      account_ids: dimension.value === 'model' ? [] : selectedAccountIds.value,
      group_by: dimension.value
    })
    usageRows.value = res.items || []
    totalCost.value = res.total_cost
    totalRequests.value = res.total_requests
    totalInput.value = res.total_input
    totalOutput.value = res.total_output
    totalCache.value = res.total_cache
    costComplete.value = res.cost_complete
    currency.value = res.currency
  } catch {
    appStore.showError(t('admin.accountExport.loadFailed'))
  } finally {
    loadingUsage.value = false
  }
}

const exportCSV = async () => {
  exporting.value = true
  try {
    await accountUsageExportAPI.downloadCSV({
      start: startDate.value,
      end: endDate.value,
      granularity: granularity.value,
      account_ids: dimension.value === 'model' ? [] : selectedAccountIds.value,
      group_by: dimension.value
    })
  } catch {
    appStore.showError(t('admin.accountExport.exportFailed'))
  } finally {
    exporting.value = false
  }
}

// ---------- 定价 ----------
const pricingForm = reactive<ExportPricing>({ currency: 'CNY', models: {}, aliases: {} })
const aliasFrom = ref('')
const aliasTo = ref('')

const setExcluded = (model: string, excluded: boolean) => {
  if (!pricingForm.models[model]) {
    pricingForm.models[model] = { input: 0, output: 0, cache_read: 0 }
  }
  pricingForm.models[model].excluded = excluded
  markDirty()
}

const addAlias = () => {
  if (!pricingForm.aliases) pricingForm.aliases = {}
  pricingForm.aliases[aliasFrom.value] = aliasTo.value
  aliasFrom.value = ''
  aliasTo.value = ''
  markDirty()
}

const removeAlias = (source: string) => {
  if (pricingForm.aliases) {
    delete pricingForm.aliases[source]
    markDirty()
  }
}
const pricingBackup = ref('')
const pricingDirty = ref(false)
const savingPricing = ref(false)
const modelsSeen = ref<Record<string, number>>({})

const pricingModels = computed(() => {
  const set = new Set([...Object.keys(modelsSeen.value), ...Object.keys(pricingForm.models)])
  return [...set].sort((a, b) => (modelsSeen.value[b] ?? 0) - (modelsSeen.value[a] ?? 0))
})

const setPrice = (model: string, field: 'input' | 'output' | 'cache_read', value: string) => {
  if (!pricingForm.models[model]) {
    pricingForm.models[model] = { input: 0, output: 0, cache_read: 0 }
  }
  pricingForm.models[model][field] = parseFloat(value) || 0
  markDirty()
}

const markDirty = () => {
  pricingDirty.value = JSON.stringify(pricingForm) !== pricingBackup.value
}

const loadPricing = async () => {
  try {
    const res = await accountUsageExportAPI.getPricing()
    Object.assign(pricingForm, { currency: res.pricing.currency, models: { ...res.pricing.models }, aliases: { ...(res.pricing.aliases || {}) } })
    modelsSeen.value = res.models_seen || {}
    pricingBackup.value = JSON.stringify(pricingForm)
    pricingDirty.value = false
  } catch {
    /* 首次使用无定价文件 */
  }
}

const savePricing = async () => {
  savingPricing.value = true
  try {
    const saved = await accountUsageExportAPI.savePricing({ currency: pricingForm.currency, models: pricingForm.models, aliases: pricingForm.aliases })
    pricingForm.models = { ...saved.models }
    pricingBackup.value = JSON.stringify(pricingForm)
    pricingDirty.value = false
    appStore.showSuccess(t('common.saved'))
  } catch {
    appStore.showError(t('common.saveFailed'))
  } finally {
    savingPricing.value = false
  }
}

const resetPricing = () => {
  Object.assign(pricingForm, JSON.parse(pricingBackup.value))
  pricingDirty.value = false
}

// ---------- 工具 ----------
const formatNumber = (n: number) => n.toLocaleString()
const compactTokens = (n: number) => (n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${(n / 1e3).toFixed(1)}K` : String(n))

const selectAllAccounts = () => {
  selectedAccountIds.value = accounts.value.map((a) => a.id)
  loadUsage()
}
const clearAccounts = () => {
  selectedAccountIds.value = []
  loadUsage()
}

onMounted(async () => {
  try {
    const res = await listAccounts(1, 100, { lite: 'true' })
    accounts.value = (res.items || []).map((a) => ({ id: a.id, name: a.name }))
    selectedAccountIds.value = accounts.value.map((a) => a.id)
  } catch {
    appStore.showError(t('admin.accountExport.accountsLoadFailed'))
  }
  await Promise.all([loadUsage(), loadPricing()])
})
</script>
