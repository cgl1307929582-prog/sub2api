<template>
  <div class="card flex flex-wrap items-center justify-between gap-3 px-4 py-3">
    <div>
      <h3 class="text-sm font-medium text-gray-900 dark:text-white">{{ t('admin.usage.conversation.title') }}</h3>
      <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ statusText }}</p>
      <p v-if="config?.stats && (config.stats.dropped || config.stats.write_failures)" class="mt-1 text-xs text-amber-700 dark:text-amber-300">{{ t('admin.usage.conversation.writeWarning', config.stats) }}</p>
    </div>
    <button class="btn btn-secondary" @click="open">{{ t('admin.usage.conversation.settings') }}</button>
    <BaseDialog :show="show" :title="t('admin.usage.conversation.settings')" @close="show = false">
      <p class="mb-5 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.usage.conversation.description') }}</p>
      <label class="mb-5 flex items-center gap-3 text-sm text-gray-900 dark:text-white">
        <input v-model="draft.enabled" type="checkbox" class="rounded border-gray-300 text-primary-600" />
        {{ t('admin.usage.conversation.enable') }}
      </label>
      <label class="block text-sm font-medium text-gray-900 dark:text-white">
        {{ t('admin.usage.conversation.retention') }}
        <select v-model.number="draft.retention_days" class="input mt-2 w-full">
          <option v-for="days in [7, 30, 90]" :key="days" :value="days">{{ t('admin.usage.conversation.days', { days }) }}</option>
          <option v-if="![0, 7, 30, 90].includes(draft.retention_days)" :value="draft.retention_days">{{ t('admin.usage.conversation.days', { days: draft.retention_days }) }}</option>
          <option :value="0">{{ t('admin.usage.conversation.forever') }}</option>
        </select>
      </label>
      <p class="mt-4 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.usage.conversation.retentionHint') }}</p>
      <template #footer>
        <button class="btn btn-secondary" :disabled="saving" @click="show = false">{{ t('common.cancel') }}</button>
        <button class="btn btn-primary" :disabled="saving || !config" @click="save">{{ t('common.save') }}</button>
      </template>
    </BaseDialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { getConversationConfig, updateConversationConfig, type ConversationConfig } from '@/api/admin/conversations'
import { useAppStore } from '@/stores'

const { t } = useI18n()
const app = useAppStore()
const config = ref<ConversationConfig | null>(null)
const draft = ref<ConversationConfig>({ enabled: false, retention_days: 7 })
const show = ref(false)
const saving = ref(false)
const loadFailed = ref(false)
const statusText = computed(() => {
  if (loadFailed.value) return t('admin.usage.conversation.loadFailed')
  if (!config.value) return t('common.loading')
  if (!config.value.enabled) return t('admin.usage.conversation.disabled')
  return t('admin.usage.conversation.enabledStatus', { retention: config.value.retention_days === 0 ? t('admin.usage.conversation.forever') : t('admin.usage.conversation.days', { days: config.value.retention_days }) })
})
async function load() {
  try { config.value = await getConversationConfig(); loadFailed.value = false }
  catch { loadFailed.value = true }
}
async function open() {
  await load()
  if (!config.value || loadFailed.value) { app.showError(t('admin.usage.conversation.loadFailed')); return }
  draft.value = { ...config.value }
  show.value = true
}
async function save() {
  saving.value = true
  try { config.value = await updateConversationConfig(draft.value); show.value = false; app.showSuccess(t('admin.usage.conversation.saved')) }
  catch { app.showError(t('admin.usage.conversation.saveFailed')) }
  finally { saving.value = false }
}
onMounted(load)
</script>
