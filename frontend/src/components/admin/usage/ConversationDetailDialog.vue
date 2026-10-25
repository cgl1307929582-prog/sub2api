<template>
  <BaseDialog :show="show" :title="t('admin.usage.conversation.title')" width="wide" :close-on-click-outside="true" @close="$emit('close')">
    <div v-if="loading" class="py-12 text-center text-gray-500" role="status">{{ t('common.loading') }}</div>
    <div v-else-if="error" class="space-y-3 py-8 text-center text-gray-500" role="status">
      <p>{{ error }}</p>
      <button class="btn btn-secondary" @click="load">{{ t('common.refresh') }}</button>
    </div>
    <div v-else-if="detail" class="space-y-5">
      <div class="flex flex-wrap items-center gap-2 text-xs text-gray-500 dark:text-gray-400">
        <span>{{ detail.model }}</span><span>·</span><span>{{ formatDateTime(detail.created_at) }}</span>
        <span>·</span><span>{{ detail.endpoint }}</span>
        <span v-if="!detail.response_complete" class="rounded bg-amber-100 px-2 py-1 text-amber-800 dark:bg-amber-900/30 dark:text-amber-300">{{ t('admin.usage.conversation.incomplete') }}</span>
      </div>
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.usage.conversation.scope') }}</p>
      <section v-for="block in blocks" :key="block.key" class="overflow-hidden rounded-xl border border-gray-200 dark:border-dark-600">
        <div class="flex items-center justify-between gap-2 bg-gray-50 px-4 py-3 dark:bg-dark-800">
          <h4 class="text-sm font-medium text-gray-900 dark:text-white">{{ block.label }}</h4>
          <button v-if="block.text" type="button" class="text-xs text-primary-600 dark:text-primary-400" @click="copy(block.text)">{{ t('keys.copyToClipboard') }}</button>
        </div>
        <!-- Plain text interpolation intentionally avoids executing model-supplied HTML. -->
        <pre class="max-h-[45vh] overflow-auto whitespace-pre-wrap break-words px-4 py-4 font-sans text-sm leading-7 text-gray-800 dark:text-gray-200">{{ block.text || t('admin.usage.conversation.noText') }}</pre>
        <p v-if="block.truncated" class="px-4 pb-3 text-xs text-amber-700 dark:text-amber-300">{{ t('admin.usage.conversation.truncated') }}</p>
      </section>
      <p v-if="detail.error_text" class="whitespace-pre-wrap break-words rounded-lg bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">{{ detail.error_text }}</p>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { getConversation, type ConversationDetail } from '@/api/admin/conversations'
import { formatDateTime } from '@/utils/format'
import { useAppStore } from '@/stores'

const props = withDefaults(defineProps<{ show: boolean; recordId: number | null; kind?: 'usage' | 'error' }>(), { kind: 'usage' })
defineEmits<{ close: [] }>()
const { t } = useI18n()
const app = useAppStore()
const detail = ref<ConversationDetail | null>(null)
const loading = ref(false)
const error = ref('')
let controller: AbortController | undefined
const blocks = computed(() => detail.value ? [
  { key: 'user', label: t('admin.usage.conversation.user'), text: detail.value.user_text, truncated: detail.value.input_truncated },
  { key: 'assistant', label: t('admin.usage.conversation.assistant'), text: detail.value.assistant_text, truncated: detail.value.output_truncated }
] : [])

async function load() {
  controller?.abort()
  detail.value = null
  error.value = ''
  if (!props.show || !props.recordId) { loading.value = false; return }
  const current = new AbortController()
  controller = current
  loading.value = true
  try {
    const result = await getConversation(props.recordId, props.kind, current.signal)
    if (!current.signal.aborted) detail.value = result
  } catch (e: unknown) {
    if (current.signal.aborted) return
    const status = (e as { response?: { status?: number } })?.response?.status
    error.value = t(status === 404 ? 'admin.usage.conversation.unavailable' : 'admin.usage.conversation.loadFailed')
  } finally {
    if (!current.signal.aborted) loading.value = false
  }
}

async function copy(text: string) {
  try { await navigator.clipboard.writeText(text); app.showSuccess(t('keys.copied')) }
  catch { app.showError(t('admin.usage.conversation.copyFailed')) }
}

watch(() => [props.show, props.recordId, props.kind], load, { immediate: true })
onBeforeUnmount(() => controller?.abort())
</script>
