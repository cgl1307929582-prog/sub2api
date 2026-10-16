<template>
  <div class="max-w-[320px] space-y-1 text-xs" data-test="colab-pool">
    <div v-if="loading && !pool" class="text-gray-400">{{ t('common.loading') }}</div>
    <template v-if="pool">
      <div class="font-medium text-gray-800 dark:text-gray-200" data-test="colab-balance">
        {{ t('admin.accounts.colab.balance') }}: {{ numeric(pool.balance_cu) }} CU
      </div>
      <div class="text-[10px] text-gray-500">
        {{ t('admin.accounts.colab.rate') }}: {{ numeric(pool.burn_cu_per_hour) }} CU/h ·
        {{ t('admin.accounts.colab.used') }}: {{ numeric(pool.used_cu_24h) }} CU
      </div>
      <div data-test="colab-instance-count">{{ t('admin.accounts.colab.instances', { ready: pool.ready, starting: pool.starting, max: pool.max_instances }) }}</div>
      <div class="text-[10px] text-gray-500">{{ t('admin.accounts.colab.accounts', { total: pool.accounts, eligible: pool.eligible_accounts }) }}</div>
      <div v-if="pool.ready === 0 && pool.starting === 0" class="text-amber-600 dark:text-amber-400" data-test="colab-offline">
        {{ t('admin.accounts.colab.offline') }}
      </div>
      <div class="text-[10px] text-gray-500">
        {{ t(pool.auto_start ? 'admin.accounts.colab.autoStart' : 'admin.accounts.colab.manualStart') }} ·
        {{ t('admin.accounts.colab.idleStop', { minutes: pool.idle_stop_minutes }) }}
      </div>
      <div v-if="pool.ready === 0 && pool.auto_start" class="text-[10px] text-gray-500">{{ t('admin.accounts.colab.coldStart') }}</div>
      <details v-if="pool.instances.length" class="text-[10px]">
        <summary class="cursor-pointer text-blue-600 dark:text-blue-400">{{ t('admin.accounts.colab.details') }}</summary>
        <div v-for="instance in pool.instances" :key="instance.id" class="mt-1 border-l-2 border-gray-200 pl-2 dark:border-gray-600">
          #{{ instance.id }} · {{ stateLabel(instance.state) }} · {{ instance.gpu }}
          <div>{{ t('admin.accounts.colab.running', { running: instance.running, waiting: instance.waiting }) }} · {{ numeric(instance.tokens_per_second) }} tok/s</div>
        </div>
      </details>
      <div v-if="pool.stale || error" class="text-[10px] text-amber-600" data-test="colab-stale">{{ t('admin.accounts.colab.stale') }}</div>
      <div v-if="pool.quota_stale" class="text-[10px] text-amber-600" data-test="colab-quota-stale">{{ t('admin.accounts.colab.quotaStale') }}</div>
      <div class="text-[9px] text-gray-400">{{ t('admin.accounts.colab.updated', { time: date(pool.observed_at) }) }}</div>
      <div class="text-[9px] text-gray-400">{{ t('admin.accounts.colab.quotaUpdated', { time: date(pool.quota_updated_at) }) }}</div>
    </template>
    <div v-if="error || usage?.error_code" class="text-amber-600" role="status">{{ error || queryError }}</div>
    <button type="button" class="rounded px-1.5 py-0.5 text-[10px] font-medium text-blue-600 hover:bg-blue-50 disabled:opacity-50 dark:text-blue-400" :disabled="loading" data-test="colab-refresh" @click="$emit('refresh')">
      {{ t('admin.accounts.colab.refresh') }}
    </button>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { AccountUsageInfo } from '@/types'
const props = defineProps<{ usage: AccountUsageInfo | null; loading: boolean; error: string | null }>()
defineEmits<{ refresh: [] }>()
const { t } = useI18n()
const pool = computed(() => props.usage?.colab_pool)
const numeric = (value: number | null) => value != null && Number.isFinite(value) ? value.toFixed(2) : t('admin.accounts.colab.unknown')
const date = (value: string | null) => value && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString() : t('admin.accounts.colab.unknown')
const stateLabel = (state: string) => t(`admin.accounts.colab.states.${['starting', 'ready', 'draining', 'stopping', 'stopped', 'error', 'failed'].includes(state) ? state : 'unknown'}`)
const queryError = computed(() => t(`admin.accounts.colab.${['unauthenticated', 'forbidden'].includes(props.usage?.error_code ?? '') ? props.usage!.error_code : 'failed'}`))
</script>
