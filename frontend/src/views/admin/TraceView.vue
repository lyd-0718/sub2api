<template>
  <AppLayout>
    <div class="space-y-6">
      <!-- 页头 -->
      <div class="flex flex-wrap items-center justify-between gap-4">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ t('admin.trace.title') }}</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.trace.description') }}</p>
        </div>
        <div class="flex items-center gap-2">
          <button @click="openSettings" class="btn btn-secondary btn-sm">{{ t('admin.trace.archiveSettings') }}</button>
          <button @click="openArchive" class="btn btn-primary btn-sm">{{ t('admin.trace.archiveNow') }}</button>
        </div>
      </div>

      <!-- 统计卡片 -->
      <div class="grid grid-cols-2 gap-4 lg:grid-cols-4">
        <div class="card p-4">
          <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.trace.todayTurns') }}</div>
          <div class="mt-1 text-2xl font-bold text-gray-900 dark:text-white tabular-nums">{{ stats?.today_turns ?? '-' }}</div>
        </div>
        <div class="card p-4">
          <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.trace.hotSize') }}</div>
          <div class="mt-1 text-2xl font-bold text-gray-900 dark:text-white tabular-nums">{{ formatBytes(stats?.hot_bytes ?? 0) }}</div>
        </div>
        <div class="card p-4">
          <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.trace.archivedSize') }}</div>
          <div class="mt-1 text-2xl font-bold text-gray-900 dark:text-white tabular-nums">{{ formatBytes(stats?.archived_bytes ?? 0) }}</div>
        </div>
        <div class="card p-4">
          <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.trace.totalSessions') }}</div>
          <div class="mt-1 text-2xl font-bold text-gray-900 dark:text-white tabular-nums">{{ stats?.total_sessions ?? '-' }}</div>
        </div>
      </div>

      <!-- 列表卡片 -->
      <div class="card">
        <!-- 筛选行：状态 / 日期 / 搜索 / Key 全为下拉或输入 -->
        <div class="flex flex-wrap items-center gap-3 border-b border-gray-100 p-4 dark:border-dark-700/50">
          <div class="w-36">
            <Select v-model="filters.status" :options="statusOptions" @change="loadSessions(true)" />
          </div>
          <div class="w-36">
            <Select v-model="filters.date" :options="dateOptions" @change="loadSessions(true)" />
          </div>
          <input
            v-model.trim="filters.sessionId"
            type="text"
            :placeholder="t('admin.trace.searchSession')"
            class="input input-sm w-56"
            @input="debouncedLoad"
          />
          <div class="w-44">
            <Select v-model="filters.apiKeyId" :options="keyFilterOptions" @change="loadSessions(true)" />
          </div>
          <button @click="loadSessions(true)" class="btn btn-ghost btn-sm ml-auto" :disabled="loading">
            {{ t('common.refresh') }}
          </button>
        </div>

        <!-- 表格 -->
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-gray-100 text-left text-xs text-gray-400 dark:border-dark-700/50 dark:text-gray-500">
                <th class="px-4 py-3 font-medium">{{ t('admin.trace.colDate') }}</th>
                <th class="px-4 py-3 font-medium">{{ t('admin.trace.colSession') }}</th>
                <th class="px-4 py-3 font-medium">{{ t('admin.trace.colApiKey') }}</th>
                <th class="px-4 py-3 font-medium text-right">{{ t('admin.trace.colTurns') }}</th>
                <th class="px-4 py-3 font-medium text-right">{{ t('admin.trace.colSize') }}</th>
                <th class="px-4 py-3 font-medium">{{ t('admin.trace.colLastActive') }}</th>
                <th class="px-4 py-3 font-medium">{{ t('admin.trace.colStatus') }}</th>
                <th class="px-4 py-3 font-medium text-right">{{ t('admin.trace.colActions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-50 dark:divide-dark-700/30">
              <tr v-if="loading">
                <td colspan="8" class="px-4 py-10 text-center text-gray-400">{{ t('common.loading') }}</td>
              </tr>
              <tr v-else-if="sessions.length === 0">
                <td colspan="8" class="px-4 py-10 text-center text-gray-400">{{ t('admin.trace.empty') }}</td>
              </tr>
              <tr
                v-else
                v-for="s in sessions"
                :key="`${s.date}-${s.session_id}`"
                class="hover:bg-gray-50/60 dark:hover:bg-dark-700/20"
              >
                <td class="px-4 py-3 tabular-nums text-gray-500 dark:text-gray-400">{{ formatDate(s.date) }}</td>
                <td class="px-4 py-3">
                  <button
                    @click="copySessionId(s.session_id)"
                    class="font-mono text-xs text-gray-700 hover:text-primary-600 dark:text-gray-300 dark:hover:text-primary-400"
                    :title="s.session_id"
                  >{{ shortenSessionId(s.session_id) }}</button>
                </td>
                <td class="px-4 py-3 text-gray-600 dark:text-gray-300">{{ s.api_key_name || '-' }}</td>
                <td class="px-4 py-3 text-right tabular-nums">{{ s.turns }}</td>
                <td class="px-4 py-3 text-right tabular-nums text-gray-500 dark:text-gray-400">{{ formatBytes(s.size_bytes) }}</td>
                <td class="px-4 py-3 text-gray-500 dark:text-gray-400 tabular-nums">{{ formatLastActive(s.last_at) }}</td>
                <td class="px-4 py-3">
                  <span
                    v-if="!s.archived"
                    class="inline-flex items-center gap-1.5 rounded-full bg-green-50 px-2.5 py-0.5 text-xs font-medium text-green-700 dark:bg-green-500/10 dark:text-green-400"
                  ><span class="h-1.5 w-1.5 rounded-full bg-green-500"></span>{{ t('admin.trace.statusHot') }}</span>
                  <span
                    v-else
                    class="inline-flex items-center gap-1.5 rounded-full bg-gray-100 px-2.5 py-0.5 text-xs font-medium text-gray-600 dark:bg-dark-600 dark:text-gray-400"
                  >{{ t('admin.trace.statusArchived') }}</span>
                </td>
                <td class="px-4 py-3 text-right">
                  <button
                    @click="download(s)"
                    class="btn btn-ghost btn-sm"
                    :disabled="downloading === `${s.date}-${s.session_id}`"
                  >
                    <template v-if="downloading === `${s.date}-${s.session_id}`">{{ t('admin.trace.compressing') }}</template>
                    <template v-else>{{ s.archived ? t('admin.trace.download') : t('admin.trace.compressDownload') }}</template>
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <!-- 分页 -->
        <div class="border-t border-gray-100 px-4 py-3 dark:border-dark-700/50">
          <Pagination
            :page="page"
            :total="total"
            :page-size="pageSize"
            @update:page="(p) => { page = p; loadSessions() }"
            @update:pageSize="(s) => { pageSize = s; page = 1; loadSessions() }"
          />
        </div>
      </div>

      <!-- 归档设置弹窗 -->
      <div v-if="showSettings" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" @click.self="showSettings = false">
        <div class="card w-full max-w-md p-6">
          <h3 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.trace.archiveSettings') }}</h3>
          <div class="mt-5 space-y-4">
            <label class="flex items-center justify-between">
              <span class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.trace.enableAutoArchive') }}</span>
              <input v-model="settingsForm.enabled" type="checkbox" class="h-4 w-4 rounded accent-teal-500" />
            </label>
            <label class="flex items-center justify-between gap-4">
              <span class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.trace.archiveTime') }}</span>
              <input v-model="settingsForm.time_of_day" type="time" class="input input-sm w-32" />
            </label>
            <label class="flex items-center justify-between gap-4">
              <span class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.trace.keepHotDays') }}</span>
              <input v-model.number="settingsForm.keep_hot_days" type="number" min="1" max="30" class="input input-sm w-32" />
            </label>
            <p class="text-xs text-gray-400">{{ t('admin.trace.keepHotDaysHint') }}</p>
          </div>
          <div class="mt-6 flex justify-end gap-2">
            <button @click="showSettings = false" class="btn btn-ghost btn-sm">{{ t('common.cancel') }}</button>
            <button @click="saveSettings" class="btn btn-primary btn-sm" :disabled="savingSettings">
              {{ savingSettings ? t('common.saving') : t('common.save') }}
            </button>
          </div>
        </div>
      </div>

      <!-- 立即归档弹窗 -->
      <div v-if="showArchive" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" @click.self="showArchive = false">
        <div class="card w-full max-w-md p-6">
          <h3 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.trace.archiveNow') }}</h3>
          <p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.trace.archiveHint') }}</p>
          <div class="mt-4">
            <label class="mb-1.5 block text-sm text-gray-700 dark:text-gray-300">{{ t('admin.trace.archiveDate') }}</label>
            <input v-model="archiveDate" type="date" class="input w-full" :max="todayISO" />
          </div>
          <p class="mt-3 text-xs text-amber-600 dark:text-amber-400">{{ t('admin.trace.archiveWarning') }}</p>
          <div class="mt-6 flex justify-end gap-2">
            <button @click="showArchive = false" class="btn btn-ghost btn-sm">{{ t('common.cancel') }}</button>
            <button @click="runArchive" class="btn btn-primary btn-sm" :disabled="archiving || !archiveDate">
              {{ archiving ? t('admin.trace.archiving') : t('admin.trace.confirmArchive') }}
            </button>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Select from '@/components/common/Select.vue'
import Pagination from '@/components/common/Pagination.vue'
import { useAppStore } from '@/stores/app'
import { traceAdminAPI, type TraceSession, type TraceStats, type TraceArchiveSettings } from '@/api/traceAdmin'
import { formatBytes } from '@/utils/format'

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const sessions = ref<TraceSession[]>([])
const page = ref(1)
const pageSize = ref(20)
const total = ref(0)
const keyNames = ref<Record<string, string>>({})
const seenDates = ref<string[]>([])

// 日期下拉选项：全部 + 有数据的日期（新→旧）
const dateOptions = computed(() => [
  { value: 'all', label: t('admin.trace.allDates') },
  ...seenDates.value.map((d) => ({ value: d, label: formatDate(d) }))
])

// Key 下拉选项：全部 + 有数据的 key（名字来自列表行 join 结果）
const keyFilterOptions = computed(() => [
  { value: 'all', label: t('admin.trace.allApiKeys') },
  ...Object.entries(keyNames.value)
    .sort((a, b) => a[1].localeCompare(b[1]))
    .map(([id, name]) => ({ value: id, label: name || `Key ${id}` }))
])
const stats = ref<TraceStats | null>(null)
const downloading = ref('')
const showSettings = ref(false)
const showArchive = ref(false)
const savingSettings = ref(false)
const archiving = ref(false)
const archiveDate = ref('')
const todayISO = new Date().toISOString().slice(0, 10)

const filters = reactive({ status: 'all', date: 'all', sessionId: '', apiKeyId: 'all' })
const statusOptions = [
  { value: 'all', label: t('admin.trace.statusAll') },
  { value: 'hot', label: t('admin.trace.statusHot') },
  { value: 'archived', label: t('admin.trace.statusArchived') }
]

const settingsForm = reactive<TraceArchiveSettings>({ enabled: true, time_of_day: '03:00', keep_hot_days: 1 })

let debounceTimer: ReturnType<typeof setTimeout> | undefined
const debouncedLoad = () => {
  clearTimeout(debounceTimer)
  debounceTimer = setTimeout(() => loadSessions(true), 400)
}

const loadSessions = async (resetPage = false) => {
  if (resetPage) page.value = 1
  loading.value = true
  try {
    const res = await traceAdminAPI.sessions({
      status: filters.status,
      date: filters.date === 'all' ? undefined : filters.date,
      session_id: filters.sessionId || undefined,
      api_key_id: filters.apiKeyId === 'all' ? undefined : Number(filters.apiKeyId),
      page: page.value,
      page_size: pageSize.value
    })
    sessions.value = res.items || []
    total.value = res.total || 0
    const resDates = (res as { dates?: string[] }).dates || []
    if (resDates.length) {
      seenDates.value = [...new Set([...seenDates.value, ...resDates])].sort().reverse()
    }
    for (const s of sessions.value) {
      if (s.api_key_name) keyNames.value[String(s.api_key_id)] = s.api_key_name
    }
  } catch {
    appStore.showError(t('admin.trace.loadFailed'))
  } finally {
    loading.value = false
  }
}

const loadStats = async () => {
  try {
    stats.value = await traceAdminAPI.stats()
  } catch {
    /* 统计卡失败不阻塞列表 */
  }
}

const copySessionId = async (id: string) => {
  try {
    await navigator.clipboard.writeText(id)
    appStore.showSuccess(t('common.copied'))
  } catch {
    /* 剪贴板不可用时静默 */
  }
}

const download = async (s: TraceSession) => {
  const key = `${s.date}-${s.session_id}`
  downloading.value = key
  try {
    await traceAdminAPI.download(s.api_key_id, s.date, s.session_id)
  } catch {
    appStore.showError(t('admin.trace.downloadFailed'))
  } finally {
    downloading.value = ''
  }
}

const openSettings = async () => {
  try {
    Object.assign(settingsForm, await traceAdminAPI.getSettings())
  } catch {
    /* 用默认值 */
  }
  showSettings.value = true
}

const saveSettings = async () => {
  savingSettings.value = true
  try {
    await traceAdminAPI.saveSettings({ ...settingsForm })
    appStore.showSuccess(t('common.saved'))
    showSettings.value = false
  } catch {
    appStore.showError(t('common.saveFailed'))
  } finally {
    savingSettings.value = false
  }
}

const openArchive = () => {
  const yesterday = new Date()
  yesterday.setDate(yesterday.getDate() - 1)
  archiveDate.value = yesterday.toISOString().slice(0, 10)
  showArchive.value = true
}

const runArchive = async () => {
  archiving.value = true
  try {
    const res = await traceAdminAPI.archive(archiveDate.value.replace(/-/g, ''))
    appStore.showSuccess(t('admin.trace.archiveDone', { sessions: res.sessions, turns: res.turns }))
    showArchive.value = false
    await Promise.all([loadSessions(), loadStats()])
  } catch {
    appStore.showError(t('admin.trace.archiveFailed'))
  } finally {
    archiving.value = false
  }
}

const formatDate = (d: string) => (d.length === 8 ? `${d.slice(0, 4)}-${d.slice(4, 6)}-${d.slice(6, 8)}` : d)
const shortenSessionId = (id: string) => (id.length > 18 ? `${id.slice(0, 8)}…${id.slice(-6)}` : id)
const formatLastActive = (iso: string) => {
  if (!iso) return '-'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '-' : d.toLocaleString()
}

onMounted(() => {
  loadSessions()
  loadStats()
})
</script>
