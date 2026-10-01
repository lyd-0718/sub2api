<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-wrap items-center gap-3">
          <div class="flex-1 sm:max-w-64">
            <input
              v-model="searchQuery"
              type="search"
              :aria-label="t('admin.customModels.searchPlaceholder')"
              :placeholder="t('admin.customModels.searchPlaceholder')"
              class="input"
              @input="handleSearch"
            />
          </div>
          <Select
            v-model="statusFilter"
            :options="filterStatusOptions"
            :aria-label="t('admin.customModels.columns.enabled')"
            class="w-40"
            @change="handleFilterChange"
          />
          <div class="flex flex-1 justify-end gap-2">
            <button
              type="button"
              :disabled="loading || groupsLoading"
              class="btn btn-secondary"
              :title="t('common.refresh')"
              :aria-label="t('common.refresh')"
              @click="refresh"
            >
              <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
            </button>
            <button type="button" class="btn btn-primary" :disabled="busy" @click="handleCreate">
              <Icon name="plus" size="md" class="mr-1" />
              {{ t('admin.customModels.createCustomModel') }}
            </button>
          </div>
        </div>
      </template>
      <template #table>
        <p v-if="loadError" role="alert" class="mb-3 text-sm text-red-600 dark:text-red-400">{{ loadError }}</p>
        <DataTable :columns="columns" :data="customModels" :loading="loading">
          <template #empty>{{ t('admin.customModels.noData') }}</template>
          <template #cell-model_id="{ value }">
            <code class="font-mono text-sm text-gray-900 dark:text-gray-100">{{ value }}</code>
          </template>
          <template #cell-upstream_group_id="{ value }">{{ getGroupName(value) }}</template>
          <template #cell-upstream_model="{ value }"><code class="font-mono text-sm">{{ value }}</code></template>
          <template #cell-downstream_groups="{ value }">
            <div class="flex flex-wrap gap-1">
              <span v-for="groupId in value || []" :key="groupId" class="badge badge-gray">{{ getGroupName(groupId) }}</span>
              <span v-if="!value?.length" class="text-sm text-gray-500">{{ t('admin.customModels.notExposed') }}</span>
            </div>
          </template>
          <template #cell-enabled="{ value }">
            <span class="badge" :class="value ? 'badge-success' : 'badge-gray'">
              {{ t(value ? 'admin.customModels.status.enabled' : 'admin.customModels.status.disabled') }}
            </span>
          </template>
          <template #cell-description="{ value }">
            <span class="block max-w-xs truncate" :title="value || ''">{{ value || '—' }}</span>
          </template>
          <template #cell-created_at="{ value }">{{ formatDateTime(value) }}</template>
          <template #cell-actions="{ row }">
            <div class="flex items-center gap-1">
              <button type="button" class="btn btn-secondary p-2" :disabled="busy" :title="t('common.edit')" :aria-label="t('common.edit')" @click="handleEdit(row)">
                <Icon name="edit" size="sm" />
              </button>
              <button
                type="button"
                class="btn btn-secondary p-2"
                :disabled="busy"
                :title="t(row.enabled ? 'admin.customModels.actions.disable' : 'admin.customModels.actions.enable')"
                :aria-label="t(row.enabled ? 'admin.customModels.actions.disable' : 'admin.customModels.actions.enable')"
                @click="handleToggleEnabled(row)"
              >
                <LoadingSpinner v-if="togglingId === row.id" size="sm" />
                <Icon v-else :name="row.enabled ? 'ban' : 'play'" size="sm" />
              </button>
              <button type="button" class="btn btn-danger p-2" :disabled="busy" :title="t('common.delete')" :aria-label="t('common.delete')" @click="deletingModel = row">
                <Icon name="trash" size="sm" />
              </button>
            </div>
          </template>
        </DataTable>
      </template>
      <template #pagination>
        <Pagination
          v-if="pagination.total > 0"
          :page="pagination.page"
          :total="pagination.total"
          :page-size="pagination.page_size"
          @update:page="handlePageChange"
          @update:pageSize="handlePageSizeChange"
        />
      </template>
    </TablePageLayout>

    <BaseDialog
      :show="showFormDialog"
      :title="t(editingModel ? 'admin.customModels.editCustomModel' : 'admin.customModels.createCustomModel')"
      width="wide"
      :close-on-escape="!submitting"
      :show-close-button="!submitting"
      @close="closeFormDialog"
    >
      <form id="custom-model-form" @submit.prevent="handleSubmit">
        <fieldset :disabled="submitting" class="space-y-4">
          <div>
            <label for="custom-model-id" class="input-label">{{ t('admin.customModels.form.modelId') }}</label>
            <input id="custom-model-id" v-model="form.model_id" class="input font-mono" :placeholder="t('admin.customModels.form.modelIdPlaceholder')" :disabled="!!editingModel" required aria-describedby="custom-model-id-help" />
            <p id="custom-model-id-help" class="mt-1 text-xs text-gray-500">{{ t('admin.customModels.form.modelIdHelp') }}</p>
            <p v-if="formErrors.model_id" role="alert" class="mt-1 text-xs text-red-600">{{ formErrors.model_id }}</p>
          </div>
          <div v-if="groupsError" role="alert" class="flex items-center gap-2 text-sm text-red-600">
            {{ groupsError }}
            <button type="button" class="btn btn-secondary" :disabled="groupsLoading" @click="loadGroups">{{ t('common.refresh') }}</button>
          </div>
          <div>
            <label for="custom-model-upstream-group" class="input-label">{{ t('admin.customModels.form.upstreamGroup') }}</label>
            <Select
              id="custom-model-upstream-group"
              v-model="form.upstream_group_id"
              :options="groupOptions"
              :placeholder="t('admin.customModels.form.upstreamGroupPlaceholder')"
              :aria-label="t('admin.customModels.form.upstreamGroup')"
              :disabled="submitting || groupsLoading || !!groupsError"
              :loading="groupsLoading"
              :error="!!formErrors.upstream_group_id"
              aria-describedby="custom-model-upstream-group-help"
              searchable
            />
            <p id="custom-model-upstream-group-help" class="mt-1 text-xs text-gray-500">{{ t('admin.customModels.form.upstreamGroupHelp') }}</p>
            <p v-if="formErrors.upstream_group_id" role="alert" class="mt-1 text-xs text-red-600">{{ formErrors.upstream_group_id }}</p>
          </div>
          <div>
            <label for="custom-model-upstream-model" class="input-label">{{ t('admin.customModels.form.upstreamModel') }}</label>
            <input id="custom-model-upstream-model" v-model="form.upstream_model" class="input font-mono" :placeholder="t('admin.customModels.form.upstreamModelPlaceholder')" required aria-describedby="custom-model-upstream-model-help" />
            <p id="custom-model-upstream-model-help" class="mt-1 text-xs text-gray-500">{{ t('admin.customModels.form.upstreamModelHelp') }}</p>
            <p v-if="formErrors.upstream_model" role="alert" class="mt-1 text-xs text-red-600">{{ formErrors.upstream_model }}</p>
          </div>
          <div>
            <label for="custom-model-prompt" class="input-label">{{ t('admin.customModels.form.systemPrompt') }}</label>
            <textarea id="custom-model-prompt" v-model="form.system_prompt" class="input font-mono text-sm" :placeholder="t('admin.customModels.form.systemPromptPlaceholder')" rows="6" aria-describedby="custom-model-prompt-help" />
            <p id="custom-model-prompt-help" class="mt-1 text-xs text-gray-500">{{ t('admin.customModels.form.systemPromptHelp') }}</p>
          </div>
          <div>
            <label for="custom-model-injection-mode" class="input-label">{{ t('admin.customModels.form.injectionMode') }}</label>
            <select id="custom-model-injection-mode" v-model="form.injection_mode" class="input" aria-describedby="custom-model-injection-mode-help">
              <option value="prepend">{{ t('admin.customModels.form.injectionModePrepend') }}</option>
              <option value="append">{{ t('admin.customModels.form.injectionModeAppend') }}</option>
              <option value="replace">{{ t('admin.customModels.form.injectionModeReplace') }}</option>
            </select>
            <p id="custom-model-injection-mode-help" class="mt-1 text-xs text-gray-500">{{ t('admin.customModels.form.injectionModeHelp') }}</p>
          </div>
          <div>
            <label for="custom-model-description" class="input-label">{{ t('admin.customModels.form.description') }}</label>
            <textarea id="custom-model-description" v-model="form.description" class="input" :placeholder="t('admin.customModels.form.descriptionPlaceholder')" rows="3" />
          </div>
          <fieldset aria-describedby="custom-model-downstream-help">
            <legend class="input-label">
              {{ t('admin.customModels.form.downstreamGroups') }}
              <span class="font-normal text-gray-400">{{ t('common.selectedCount', { count: form.downstream_groups.length }) }}</span>
            </legend>
            <label for="custom-model-group-search" class="sr-only">{{ t('admin.customModels.form.groupSearch') }}</label>
            <div class="flex items-center gap-2 rounded-t-lg border border-gray-200 bg-gray-50 px-3 py-2 dark:border-dark-600 dark:bg-dark-800">
              <Icon name="search" size="sm" class="text-gray-400" />
              <input id="custom-model-group-search" v-model="groupSearch" type="search" class="min-w-0 flex-1 bg-transparent text-sm focus:outline-none" :placeholder="t('admin.customModels.form.groupSearch')" />
              <button v-if="form.downstream_groups.length" type="button" class="text-sm text-primary-600" @click="form.downstream_groups = []">{{ t('admin.customModels.actions.clearSelection') }}</button>
            </div>
            <div class="grid max-h-56 gap-1 overflow-y-auto rounded-b-lg border border-t-0 border-gray-200 bg-gray-50 p-2 dark:border-dark-600 dark:bg-dark-800 sm:grid-cols-2">
              <label v-for="group in filteredGroups" :key="group.id" class="flex cursor-pointer items-center gap-2 rounded px-2 py-2 hover:bg-white dark:hover:bg-dark-700">
                <input v-model="form.downstream_groups" type="checkbox" :value="group.id" :disabled="groupsLoading || !!groupsError" class="h-4 w-4 shrink-0 rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="min-w-0 flex-1">
                  <GroupBadge :name="group.name" :platform="group.platform" />
                  <span class="mt-1 block text-xs text-gray-500">{{ group.platform }} · {{ groupStatus(group) }}</span>
                </span>
              </label>
              <p v-if="!filteredGroups.length" class="py-2 text-center text-sm text-gray-500 sm:col-span-2">{{ t(groupsLoading ? 'common.loading' : 'common.noGroupsAvailable') }}</p>
            </div>
            <p id="custom-model-downstream-help" class="mt-1 text-xs text-gray-500">{{ t('admin.customModels.form.downstreamGroupsHelp') }}</p>
          </fieldset>
          <div class="flex items-center gap-3">
            <Toggle id="custom-model-enabled" v-model="form.enabled" :disabled="submitting" :aria-label="t('admin.customModels.form.enabled')" />
            <label for="custom-model-enabled" class="text-sm font-medium">{{ t('admin.customModels.form.enabled') }}</label>
          </div>
          <p v-if="saveError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ saveError }}</p>
        </fieldset>
      </form>
      <template #footer>
        <div class="flex justify-end gap-3">
          <button type="button" class="btn btn-secondary" :disabled="submitting" @click="closeFormDialog">{{ t('common.cancel') }}</button>
          <button type="submit" form="custom-model-form" class="btn btn-primary" :disabled="submitting || groupsLoading || !!groupsError">
            <LoadingSpinner v-if="submitting" size="sm" class="mr-2" />
            {{ t(editingModel ? 'admin.customModels.actions.save' : 'admin.customModels.actions.create') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <BaseDialog :show="!!deletingModel" :title="t('admin.customModels.deleteCustomModel')" width="narrow" :close-on-escape="!deleting" :show-close-button="!deleting" @close="closeDeleteDialog">
      <p class="text-sm text-gray-600 dark:text-gray-400">{{ t('admin.customModels.confirmDelete', { modelId: deletingModel?.model_id }) }}</p>
      <p v-if="deleteError" role="alert" class="mt-3 text-sm text-red-600">{{ deleteError }}</p>
      <template #footer>
        <div class="flex justify-end gap-3">
          <button type="button" class="btn btn-secondary" :disabled="deleting" @click="closeDeleteDialog">{{ t('common.cancel') }}</button>
          <button type="button" class="btn btn-danger" :disabled="deleting" @click="confirmDelete">
            <LoadingSpinner v-if="deleting" size="sm" class="mr-2" />
            {{ t('common.delete') }}
          </button>
        </div>
      </template>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import type { Column } from '@/components/common/types'
import Pagination from '@/components/common/Pagination.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import GroupBadge from '@/components/common/GroupBadge.vue'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore } from '@/stores/app'
import { formatDateTime } from '@/utils/format'
import { extractApiErrorMessage } from '@/utils/apiError'
import { getAllIncludingInactive } from '@/api/admin/groups'
import { listCustomModels, createCustomModel, updateCustomModel, deleteCustomModel, type CustomModel, type CreateCustomModelRequest } from '@/api/admin/customModels'
import type { AdminGroup } from '@/types'

const { t } = useI18n()
const appStore = useAppStore()
const customModels = ref<CustomModel[]>([])
const groups = ref<AdminGroup[]>([])
const loading = ref(false)
const groupsLoading = ref(false)
const loadError = ref('')
const groupsError = ref('')
const saveError = ref('')
const deleteError = ref('')
const searchQuery = ref('')
const statusFilter = ref<boolean | null>(null)
const pagination = ref({ page: 1, page_size: 20, total: 0 })
const showFormDialog = ref(false)
const editingModel = ref<CustomModel | null>(null)
const deletingModel = ref<CustomModel | null>(null)
const submitting = ref(false)
const deleting = ref(false)
const togglingId = ref<number | null>(null)
const busy = computed(() => submitting.value || deleting.value || togglingId.value !== null)
const groupSearch = ref('')
const formErrors = ref<Record<string, string>>({})
const emptyForm = () => ({
  model_id: '', upstream_group_id: null as number | null, upstream_model: '',
  system_prompt: '', injection_mode: 'prepend', description: '', enabled: true,
  downstream_groups: [] as number[]
})
const form = ref(emptyForm())
let requestId = 0
let disposed = false
let searchTimer: ReturnType<typeof setTimeout> | undefined

const columns = computed<Column[]>(() => [
  { key: 'model_id', label: t('admin.customModels.columns.modelId') },
  { key: 'upstream_group_id', label: t('admin.customModels.columns.upstreamGroup') },
  { key: 'upstream_model', label: t('admin.customModels.columns.upstreamModel') },
  { key: 'downstream_groups', label: t('admin.customModels.columns.downstreamGroups') },
  { key: 'enabled', label: t('admin.customModels.columns.enabled') },
  { key: 'description', label: t('admin.customModels.columns.description') },
  { key: 'created_at', label: t('admin.customModels.columns.createdAt') },
  { key: 'actions', label: t('admin.customModels.columns.actions') }
])
const filterStatusOptions = computed<SelectOption[]>(() => [
  { value: null, label: t('admin.customModels.filters.allStatus') },
  { value: true, label: t('admin.customModels.filters.enabledOnly') },
  { value: false, label: t('admin.customModels.filters.disabledOnly') }
])
function groupStatus(group: AdminGroup) {
  return t(group.status === 'active' ? 'admin.customModels.status.active' : 'admin.customModels.status.inactive')
}
const groupOptions = computed<SelectOption[]>(() => groups.value.map(group => ({
  value: group.id, label: `${group.name} (${group.platform} · ${groupStatus(group)})`
})))
const filteredGroups = computed(() => {
  const query = groupSearch.value.trim().toLowerCase()
  return groups.value.filter(group => `${group.name} ${group.platform} ${group.status} ${groupStatus(group)}`.toLowerCase().includes(query))
})
function getGroupName(id: number) {
  return groups.value.find(group => group.id === id)?.name ?? t('admin.customModels.unknownGroup', { id })
}

async function loadCustomModels() {
  clearTimeout(searchTimer)
  const currentRequest = ++requestId
  loading.value = true
  loadError.value = ''
  try {
    const response = await listCustomModels({
      page: pagination.value.page,
      page_size: pagination.value.page_size,
      search: searchQuery.value.trim() || undefined,
      enabled: statusFilter.value ?? undefined
    })
    if (disposed || currentRequest !== requestId) return
    const lastPage = Math.max(1, Math.ceil(response.total / response.page_size))
    if (response.page > lastPage) {
      pagination.value.page = lastPage
      await loadCustomModels()
      return
    }
    customModels.value = response.items
    pagination.value = { page: response.page, page_size: response.page_size, total: response.total }
  } catch (error) {
    if (disposed || currentRequest !== requestId) return
    loadError.value = extractApiErrorMessage(error, t('admin.customModels.messages.loadError'))
    customModels.value = []
  } finally {
    if (!disposed && currentRequest === requestId) loading.value = false
  }
}
async function loadGroups() {
  if (groupsLoading.value) return
  groupsLoading.value = true
  groupsError.value = ''
  try {
    const result = await getAllIncludingInactive()
    if (!disposed) groups.value = result
  } catch (error) {
    if (!disposed) groupsError.value = extractApiErrorMessage(error, t('admin.customModels.messages.groupsError'))
  } finally {
    if (!disposed) groupsLoading.value = false
  }
}
function refresh() {
  void loadCustomModels()
  void loadGroups()
}
function handleSearch() {
  clearTimeout(searchTimer)
  ++requestId // Invalidate older responses immediately, including the debounce interval.
  loading.value = true
  pagination.value.page = 1
  searchTimer = setTimeout(() => { void loadCustomModels() }, 300)
}
function handleFilterChange() {
  pagination.value.page = 1
  void loadCustomModels()
}
function handlePageChange(page: number) {
  pagination.value.page = page
  void loadCustomModels()
}
function handlePageSizeChange(pageSize: number) {
  pagination.value.page_size = pageSize
  pagination.value.page = 1
  void loadCustomModels()
}
function handleCreate() {
  if (busy.value) return
  editingModel.value = null
  form.value = emptyForm()
  openFormDialog()
}
function handleEdit(model: CustomModel) {
  if (busy.value) return
  editingModel.value = model
  form.value = {
    model_id: model.model_id, upstream_group_id: model.upstream_group_id,
    upstream_model: model.upstream_model, system_prompt: model.system_prompt ?? '',
    injection_mode: model.injection_mode ?? 'prepend',
    description: model.description ?? '', enabled: model.enabled,
    downstream_groups: [...(model.downstream_groups ?? [])]
  }
  openFormDialog()
}
function openFormDialog() {
  formErrors.value = {}
  saveError.value = ''
  groupSearch.value = ''
  showFormDialog.value = true
}
function closeFormDialog() {
  if (!submitting.value) showFormDialog.value = false
}
async function handleSubmit() {
  if (busy.value || groupsLoading.value || groupsError.value) return
  formErrors.value = {}
  if (!form.value.model_id.trim()) formErrors.value.model_id = t('admin.customModels.validation.modelIdRequired')
  if (!form.value.upstream_group_id) formErrors.value.upstream_group_id = t('admin.customModels.validation.upstreamGroupRequired')
  if (!form.value.upstream_model.trim()) formErrors.value.upstream_model = t('admin.customModels.validation.upstreamModelRequired')
  if (Object.keys(formErrors.value).length) return
  submitting.value = true
  saveError.value = ''
  const model = editingModel.value
  const payload: CreateCustomModelRequest = {
    model_id: form.value.model_id.trim(), upstream_group_id: form.value.upstream_group_id!,
    upstream_model: form.value.upstream_model.trim(), system_prompt: form.value.system_prompt,
    injection_mode: form.value.injection_mode,
    description: form.value.description, enabled: form.value.enabled,
    downstream_groups: [...form.value.downstream_groups]
  }
  try {
    if (model) {
      const { model_id: _modelId, ...updates } = payload
      await updateCustomModel(model.id, updates)
    } else {
      await createCustomModel(payload)
    }
    if (disposed) return
    appStore.showSuccess(t(model ? 'admin.customModels.messages.updateSuccess' : 'admin.customModels.messages.createSuccess'))
    showFormDialog.value = false
    await loadCustomModels()
  } catch (error) {
    if (!disposed) saveError.value = extractApiErrorMessage(error, t(model ? 'admin.customModels.messages.updateError' : 'admin.customModels.messages.createError'))
  } finally {
    submitting.value = false
  }
}
async function handleToggleEnabled(model: CustomModel) {
  if (busy.value) return
  togglingId.value = model.id
  try {
    await updateCustomModel(model.id, { enabled: !model.enabled })
    if (disposed) return
    appStore.showSuccess(t('admin.customModels.messages.updateSuccess'))
    await loadCustomModels()
  } catch (error) {
    if (!disposed) appStore.showError(extractApiErrorMessage(error, t('admin.customModels.messages.updateError')))
  } finally {
    togglingId.value = null
  }
}
function closeDeleteDialog() {
  if (deleting.value) return
  deletingModel.value = null
  deleteError.value = ''
}
async function confirmDelete() {
  if (!deletingModel.value || busy.value) return
  deleting.value = true
  deleteError.value = ''
  try {
    await deleteCustomModel(deletingModel.value.id)
    if (disposed) return
    appStore.showSuccess(t('admin.customModels.messages.deleteSuccess'))
    deletingModel.value = null
    await loadCustomModels()
  } catch (error) {
    if (!disposed) deleteError.value = extractApiErrorMessage(error, t('admin.customModels.messages.deleteError'))
  } finally {
    deleting.value = false
  }
}
onMounted(refresh)
onUnmounted(() => {
  disposed = true
  ++requestId
  clearTimeout(searchTimer)
})
</script>
