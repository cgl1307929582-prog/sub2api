<template>
  <div class="space-y-1.5" data-test="droid-quota">
    <div v-if="loading && !quota" class="text-xs text-gray-400">{{ t('common.loading') }}</div>
    <template v-if="quota">
      <div class="flex flex-wrap gap-x-4 gap-y-2">
        <div v-for="pool in pools" :key="pool.key" class="space-y-1" :data-test="`droid-${pool.key}`">
          <div class="text-[10px] font-medium text-gray-600 dark:text-gray-300">
            {{ pool.label }} · {{ t('admin.accounts.factoryQuota.used') }}
          </div>
          <div v-for="period in periods" :key="period.key">
            <div v-if="isInactive(pool.value?.[period.key])" class="text-[10px] text-gray-400" :data-test="`droid-${pool.key}-${period.key}-inactive`">
              {{ period.label }} · {{ t('admin.accounts.factoryQuota.inactive') }}
            </div>
            <UsageProgressBar
              v-else-if="hasUsage(pool.value?.[period.key])"
              :label="period.label"
              :utilization="pool.value![period.key]!.used_percent!"
              :resets-at="pool.value![period.key]!.resets_at"
              :color="pool.key === 'standard' ? 'indigo' : 'emerald'"
              :title="windowHint(pool.value![period.key]!)"
            />
            <div v-else class="text-[10px] text-gray-400">
              {{ period.label }} · {{ t('admin.accounts.factoryQuota.unknown') }}
            </div>
          </div>
        </div>
      </div>
      <div class="text-[10px] text-gray-600 dark:text-gray-300" data-test="droid-extra-balance">
        {{ t('admin.accounts.factoryQuota.extraBalance') }}: {{ balance }}
      </div>
      <div v-if="quota.stale || error" class="text-[10px] text-amber-600 dark:text-amber-400" data-test="droid-stale">
        {{ t('admin.accounts.factoryQuota.stale') }}
      </div>
      <div class="text-[9px] text-gray-400" data-test="droid-fetched-at">
        {{ t('admin.accounts.factoryQuota.updated', { time: fetchedAt }) }}
      </div>
    </template>
    <div v-if="error || usage?.error_code" class="max-w-[260px] text-xs text-amber-600 dark:text-amber-400" role="status">
      {{ error || quotaError }}
    </div>
    <button type="button" class="rounded px-1.5 py-0.5 text-[10px] font-medium text-blue-600 hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30" :disabled="loading" data-test="droid-refresh" @click="$emit('refresh')">
      {{ t('admin.accounts.factoryQuota.refresh') }}
    </button>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { AccountUsageInfo, DroidQuotaWindow } from '@/types'
import UsageProgressBar from './UsageProgressBar.vue'

const props = defineProps<{ usage: AccountUsageInfo | null; loading: boolean; error: string | null }>()
defineEmits<{ refresh: [] }>()
const { t } = useI18n()
const quota = computed(() => props.usage?.droid_quota)
const pools = computed(() => [
  { key: 'standard', label: 'Standard', value: quota.value?.standard },
  { key: 'core', label: 'Core', value: quota.value?.core }
])
const periods = [
  { key: 'five_hour', label: '5h' },
  { key: 'weekly', label: '7d' },
  { key: 'monthly', label: '30d' }
] as const
const hasUsage = (window?: DroidQuotaWindow | null) => window?.used_percent != null && Number.isFinite(window.used_percent)
const isInactive = (window?: DroidQuotaWindow | null) => window?.used_percent === 0 && window.resets_at == null
const formatDate = (value?: string | null) => value && Number.isFinite(Date.parse(value))
  ? new Date(value).toLocaleString()
  : t('admin.accounts.factoryQuota.unknown')
const fetchedAt = computed(() => formatDate(quota.value?.fetched_at))
const balance = computed(() => {
  const cents = quota.value?.extra_usage_balance_cents
  return cents != null && Number.isFinite(cents) ? `$${(cents / 100).toFixed(2)}` : t('admin.accounts.factoryQuota.unknown')
})
const windowHint = (window: DroidQuotaWindow) => t('admin.accounts.factoryQuota.windowHint', {
  percent: Number(Math.max(0, 100 - (window.used_percent ?? 0)).toFixed(2)),
  time: formatDate(window.resets_at)
})
const quotaError = computed(() => {
  const key = ({ unauthenticated: 'unauthenticated', forbidden: 'forbidden', rate_limited: 'rateLimited', quota_unknown: 'unknown' } as Record<string, string>)[props.usage?.error_code ?? ''] ?? 'failed'
  return t(`admin.accounts.factoryQuota.${key}`)
})
</script>
