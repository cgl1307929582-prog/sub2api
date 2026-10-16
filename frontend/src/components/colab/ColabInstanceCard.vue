<template>
  <article class="rounded-2xl border border-gray-200 bg-white p-5 shadow-sm dark:border-dark-700 dark:bg-dark-800">
    <div class="flex items-center justify-between gap-3"><h3 class="font-semibold">#{{ instance.id }} · {{ t('colabPool.instances') }}</h3><span class="rounded-full px-3 py-1 text-xs font-medium" :class="instance.state === 'ready' ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-300' : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'">{{ stateLabel }}</span></div>
    <p class="mt-2 truncate text-sm text-gray-500">{{ instance.email }}</p>
    <p class="mt-1 text-xs text-gray-500">{{ t('colabPool.uptime') }} · {{ num(Math.max(0, (instance.ended_at || now) - instance.created_at) / 60, 0) }} min</p>
    <div v-if="live" class="mt-5 grid grid-cols-3 gap-3 text-center">
      <div><strong class="block text-xl tabular-nums">{{ num(instance.running, 0) }}<small v-if="resource?.engine?.parallel" class="text-sm text-gray-400"> / {{ resource.engine.parallel }}</small></strong><span class="text-xs text-gray-500">{{ t('colabPool.processing') }}</span></div>
      <div><strong class="block text-xl tabular-nums">{{ num(instance.waiting, 0) }}</strong><span class="text-xs text-gray-500">{{ t('colabPool.waiting') }}</span></div>
      <div><strong class="block text-xl tabular-nums">{{ num(instance.tps) }}</strong><span class="text-xs text-gray-500">{{ t('colabPool.throughput') }}</span></div>
    </div>
    <p v-if="instance.phase" class="mt-4 break-words text-sm text-gray-500">{{ t('colabPool.phase') }} · {{ instance.phase }}</p>
    <div v-if="resource?.cpu" class="mt-5 space-y-3 border-t border-gray-100 pt-4 dark:border-dark-700">
      <div v-for="meter in meters" :key="meter.label"><div class="mb-1 flex justify-between text-xs"><span class="text-gray-500">{{ meter.label }}</span><span class="tabular-nums">{{ meter.value }}</span></div><div class="h-1.5 overflow-hidden rounded-full bg-gray-100 dark:bg-dark-700"><div class="h-full rounded-full bg-primary-500" :style="{ width: `${Math.min(100, Math.max(0, meter.percent || 0))}%` }" /></div></div>
      <p class="text-xs leading-5 text-gray-500">{{ resource.cpu.model }} · {{ resource.cpu.logical_cores }} CPU<br>{{ gpu?.name }}<br v-if="gpu">{{ gpu ? `${num(gpu.temperature_c, 0)} °C · ${num(gpu.power_w, 0)} / ${num(gpu.power_limit_w, 0)} W` : '' }}</p>
      <p class="text-xs" :class="stale ? 'text-amber-600 dark:text-amber-400' : 'text-gray-400'">{{ stale ? t('colabPool.resourceStale') : t('colabPool.hardwareSample') }} · {{ resource.received_at ? new Date(resource.received_at * 1000).toLocaleString() : '—' }}</p>
    </div>
    <p v-else-if="live" class="mt-4 text-xs text-gray-500">{{ t('colabPool.sampling') }}</p>
    <p v-if="live" class="mt-3 text-xs text-gray-500">{{ t('colabPool.instanceCalls') }} · {{ callsFresh ? `${calls?.requests ?? 0} / ${calls?.errors ?? 0}` : '—' }}</p>
    <div v-if="live" class="mt-5 flex justify-end gap-2"><button v-if="instance.state === 'ready'" class="btn btn-secondary text-sm" :disabled="busy" @click="$emit('stop', instance.id, true)">{{ t('colabPool.drain') }}</button><button class="btn btn-danger text-sm" :disabled="busy || instance.state === 'stopping'" @click="$emit('stop', instance.id, false)">{{ t('colabPool.stop') }}</button></div>
  </article>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { PoolInstance, PoolResource } from '@/api/admin/colabPool'
const props = defineProps<{ instance: PoolInstance; resource?: PoolResource; now: number; busy: boolean; telemetryAvailable: boolean; callsFresh: boolean; calls?: { requests: number; errors: number; rpm: number } }>()
defineEmits<{ stop: [id: number, drain: boolean] }>()
const { t, te } = useI18n()
const num = (n: number | null | undefined, d = 1) => typeof n === 'number' && Number.isFinite(n) ? n.toLocaleString(undefined, { maximumFractionDigits: d }) : '—'
const stateLabel = computed(() => te('colabPool.states.' + props.instance.state) ? t('colabPool.states.' + props.instance.state) : props.instance.state)
const live = computed(() => ['ready', 'starting', 'draining', 'stopping'].includes(props.instance.state))
const gpu = computed(() => props.resource?.gpus?.[0])
const stale = computed(() => !props.telemetryAvailable || props.resource?.stale || props.now - (props.resource?.received_at || 0) > 75)
const bytes = (n?: number) => n == null ? '—' : num(n / 1073741824)
const meters = computed(() => {
  const r = props.resource, g = gpu.value
  return [
    { label: t('colabPool.cpu'), value: num(r?.cpu?.percent) + '%', percent: r?.cpu?.percent },
    { label: t('colabPool.gpuUsage'), value: num(g?.percent) + '%', percent: g?.percent },
    ...[{ label: t('colabPool.ram'), used: r?.ram?.used_bytes, total: r?.ram?.total_bytes }, { label: t('colabPool.vram'), used: g?.memory_used_bytes, total: g?.memory_total_bytes }, { label: t('colabPool.disk'), used: r?.disk?.used_bytes, total: r?.disk?.total_bytes }].map(x => ({ label: x.label, value: `${bytes(x.used)} / ${bytes(x.total)} GiB`, percent: x.total ? (x.used || 0) / x.total * 100 : 0 }))
  ]
})
</script>
