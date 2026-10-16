<template>
  <AppLayout>
    <div class="mx-auto max-w-7xl space-y-6">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <nav class="flex max-w-full gap-1 overflow-x-auto rounded-xl bg-gray-100 p-1 dark:bg-dark-800" :aria-label="t('colabPool.title')">
          <button v-for="item in tabs" :key="item" class="whitespace-nowrap rounded-lg px-4 py-2.5 text-sm font-medium transition" :class="tab === item ? 'bg-white text-primary-700 shadow-sm dark:bg-dark-700 dark:text-primary-300' : 'text-gray-500 hover:text-gray-900 dark:hover:text-gray-200'" :aria-current="tab === item ? 'page' : undefined" @click="selectTab(item)">{{ t(`colabPool.${item}`) }}</button>
        </nav>
        <div class="flex items-center gap-3"><span class="hidden text-xs text-gray-400 xl:block">{{ t('colabPool.autoRefresh') }}</span><button class="btn btn-secondary" :disabled="loading" @click="refreshAll"><Icon name="refresh" size="sm" :class="{ 'animate-spin': loading }" /><span>{{ t('colabPool.refresh') }}</span></button></div>
      </div>
      <div v-if="error" role="alert" class="rounded-xl border border-amber-200 bg-amber-50 p-4 text-sm text-amber-800 dark:border-amber-800 dark:bg-amber-900/20 dark:text-amber-300">{{ state ? t('colabPool.stale') : t('colabPool.unavailable') }}<p class="mt-1 text-xs">{{ error }}</p></div>
      <div v-if="!state && loading" class="pool-card py-16 text-center text-gray-500">{{ t('colabPool.loading') }}</div>
      <template v-if="state">
        <div v-if="tab === 'overview'" class="space-y-6">
          <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
            <div class="pool-card"><p class="metric-label">{{ t('colabPool.balance') }}</p><p class="metric-value">{{ num(state.summary.balance, 2) }} <small>CU</small></p><p class="metric-note">{{ t('colabPool.usable') }} {{ eligibleAccounts.length }} / {{ state.accounts.length }}</p></div>
            <div class="pool-card"><p class="metric-label">{{ t('colabPool.hours') }}</p><p class="metric-value">{{ num(state.summary.hours) }} <small>h</small></p><p class="metric-note">{{ t('colabPool.hoursNote') }}</p></div>
            <div class="pool-card"><p class="metric-label">{{ t('colabPool.running') }}</p><p class="metric-value">{{ state.summary.ready }} <small>/ {{ state.settings.max_instances }}</small></p><p class="metric-note">{{ t('colabPool.starting') }} {{ state.summary.starting }}</p></div>
            <div class="pool-card"><p class="metric-label">{{ t('colabPool.burn') }}</p><p class="metric-value">{{ num(state.summary.burn, 2) }} <small>CU/h</small></p><p class="metric-note">{{ t('colabPool.used24') }} {{ num(state.summary.used_24h, 2) }} CU</p></div>
          </div>
          <section class="pool-card flex flex-wrap items-center justify-between gap-5 border-primary-200 bg-primary-50/40 dark:border-primary-900 dark:bg-primary-950/20"><div><h2 class="font-semibold">{{ activeInstances.length ? `${t('colabPool.instances')} · ${activeInstances.length}` : t('colabPool.noActive') }}</h2><p class="mt-2 text-sm text-gray-500">{{ state.settings.auto_start ? t('colabPool.autoOn') : t('colabPool.autoOff') }} · {{ t('colabPool.coldStart') }}</p></div><button class="btn btn-primary" @click="selectTab('instances')">{{ t('colabPool.instances') }} →</button></section>
          <div v-if="activeInstances.length" class="grid gap-5 xl:grid-cols-2"><ColabInstanceCard v-for="instance in activeInstances" :key="instance.id" v-bind="instanceProps(instance)" @stop="confirmStop" /></div>
          <div class="pool-card"><div class="mb-4 flex items-center justify-between"><h2 class="font-semibold">{{ t('colabPool.accounts') }}</h2><button class="text-sm font-medium text-primary-600" @click="selectTab('accounts')">{{ t('colabPool.accounts') }} →</button></div><div v-for="account in state.accounts" :key="account.id" class="flex items-center gap-4 border-t border-gray-100 py-3 dark:border-dark-700"><div class="min-w-0 flex-1"><p class="truncate text-sm">{{ account.email }}</p><div class="mt-2 h-1.5 rounded-full bg-gray-100 dark:bg-dark-700"><div class="h-full rounded-full bg-primary-500" :style="{ width: `${Math.max(0, (account.balance || 0) / maxBalance * 100)}%` }" /></div></div><span class="text-sm font-semibold tabular-nums">{{ num(account.balance, 2) }} CU</span><span class="hidden text-xs text-gray-500 sm:block">{{ accountStatus(account) }}</span></div><p v-if="!state.accounts.length" class="text-sm text-gray-500">{{ t('colabPool.noAccounts') }}</p></div>
        </div>
        <div v-else-if="tab === 'accounts'" class="space-y-5">
          <div class="flex flex-wrap items-center justify-between gap-4"><p class="max-w-3xl text-sm text-gray-500">{{ t('colabPool.accountHint') }}</p><button class="btn btn-primary" :disabled="busy" @click="showAuth = true"><Icon name="plus" size="sm" />{{ t('colabPool.addAccount') }}</button></div>
          <div class="grid gap-5 lg:grid-cols-2 xl:grid-cols-3">
            <article v-for="account in state.accounts" :key="account.id" class="pool-card" :class="{ 'opacity-65': !account.enabled }">
              <div class="flex items-start justify-between gap-3"><div class="min-w-0"><h3 class="truncate text-sm font-semibold" :title="account.email">{{ account.email }}</h3><p class="mt-1 text-xs text-gray-400">{{ account.name || `#${account.id}` }}</p></div><span class="rounded-full bg-gray-100 px-2 py-1 text-xs dark:bg-dark-700">{{ accountStatus(account) }}</span></div>
              <div class="mt-5 flex items-end justify-between"><p class="text-3xl font-semibold tabular-nums">{{ num(account.balance, 2) }} <small class="text-sm font-normal text-gray-400">CU</small></p><svg v-if="account.history.length > 1" class="h-9 w-24 text-primary-500" viewBox="0 0 100 32" role="img" :aria-label="t('colabPool.balance')"><polyline :points="spark(account.history)" fill="none" stroke="currentColor" stroke-width="2" /></svg></div>
              <div class="mt-4 grid grid-cols-2 gap-2 text-sm"><div><p class="text-xs text-gray-400">{{ t('colabPool.used24') }}</p><p class="mt-1 tabular-nums">{{ num(account.used_24h, 2) }} CU</p></div><div><p class="text-xs text-gray-400">{{ t('colabPool.used7') }}</p><p class="mt-1 tabular-nums">{{ num(account.used_7d, 2) }} CU</p></div></div>
              <p class="mt-3 text-xs text-gray-500">{{ account.gpus.join(' · ') || '—' }}</p><p v-if="account.error" class="mt-2 break-words text-xs text-amber-600">{{ account.error }}</p>
              <div class="mt-5 flex flex-wrap items-center justify-between gap-3 border-t border-gray-100 pt-4 dark:border-dark-700"><label class="flex items-center gap-2 text-sm"><input type="checkbox" :checked="!!account.enabled" :disabled="busy" @change="toggleAccount(account, $event)">{{ t('colabPool.enabled') }}</label><div class="flex items-center gap-2 text-xs"><span>{{ t('colabPool.priority') }}</span><button class="step-button" :disabled="busy" :aria-label="`${account.email} ${t('colabPool.priority')} −1`" @click="act(() => api.updateAccount(account.id, { priority: account.priority - 1 }))">−</button><span class="tabular-nums">{{ account.priority }}</span><button class="step-button" :disabled="busy" :aria-label="`${account.email} ${t('colabPool.priority')} +1`" @click="act(() => api.updateAccount(account.id, { priority: account.priority + 1 }))">+</button></div></div>
              <div class="mt-4 flex flex-wrap gap-x-4 gap-y-2 text-xs font-medium"><button class="text-primary-600" :disabled="busy" @click="act(() => api.updateAccount(account.id, { refresh: true }))">{{ t('colabPool.refreshQuota') }}</button><button v-if="account.status === 'auth_error'" class="text-primary-600" @click="showAuth = true">{{ t('colabPool.reauthorize') }}</button><button class="text-gray-500" :disabled="busy" @click="editNotes(account)">{{ t('colabPool.editNotes') }}</button><button class="ml-auto text-red-600" :disabled="busy || !!account.instance" @click="confirmDelete(account)">{{ t('colabPool.delete') }}</button></div>
              <p v-if="account.note" class="mt-3 break-words text-xs text-gray-500">{{ account.note }}</p><p class="mt-3 text-xs text-gray-400">{{ t('colabPool.quotaSample') }} · {{ date(account.checked_at) }}</p>
            </article>
          </div><p v-if="!state.accounts.length" class="pool-card py-10 text-center text-gray-500">{{ t('colabPool.noAccounts') }}</p>
        </div>
        <div v-else-if="tab === 'instances'" class="space-y-5">
          <div class="pool-card flex flex-wrap items-center justify-between gap-4"><p class="max-w-2xl text-sm text-gray-500">{{ t('colabPool.coldStart') }}</p><div class="flex flex-wrap gap-3"><select v-model="startAccount" class="input w-auto max-w-xs" :aria-label="t('colabPool.chooseAccount')"><option value="">{{ t('colabPool.autoAccount') }}</option><option v-for="account in eligibleAccounts" :key="account.id" :value="String(account.id)">{{ account.email }} · {{ num(account.balance, 1) }} CU</option></select><button class="btn btn-primary" :disabled="busy || !eligibleAccounts.length || activeInstances.length >= state.settings.max_instances" @click="confirmStart">{{ t('colabPool.start') }}</button></div></div>
          <div v-if="activeInstances.length" class="grid gap-5 xl:grid-cols-2"><ColabInstanceCard v-for="instance in activeInstances" :key="instance.id" v-bind="instanceProps(instance)" @stop="confirmStop" /></div><div v-else class="pool-card py-10 text-center text-gray-500">{{ t('colabPool.noActive') }}</div>
          <h2 class="font-semibold">{{ t('colabPool.recentInstances') }}</h2><div class="grid gap-5 xl:grid-cols-2"><ColabInstanceCard v-for="instance in endedInstances" :key="instance.id" v-bind="instanceProps(instance)" @stop="confirmStop" /></div><p v-if="!endedInstances.length" class="text-sm text-gray-500">{{ t('colabPool.noRecords') }}</p>
        </div>
        <div v-else-if="tab === 'monitoring'" class="space-y-5">
          <h2 class="font-semibold">{{ t('colabPool.usageTitle') }}</h2><p class="text-sm text-gray-500">{{ t('colabPool.usageHint') }}</p><p v-if="usageError" role="alert" class="text-sm text-amber-600">{{ t('colabPool.usageUnavailable') }}</p>
          <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-5"><div v-for="metric in usageMetrics" :key="metric.label" class="pool-card"><p class="metric-label">{{ metric.label }}</p><p class="mt-3 text-2xl font-semibold tabular-nums">{{ metric.value }}</p></div></div>
          <p v-if="!snapshot?.telemetry_available" class="text-sm text-amber-600">{{ t('colabPool.telemetryUnavailable') }}</p><div class="grid gap-5 xl:grid-cols-2"><ColabInstanceCard v-for="instance in activeInstances" :key="instance.id" v-bind="instanceProps(instance)" @stop="confirmStop" /></div>
          <div class="pool-card overflow-x-auto"><h3 class="mb-4 font-semibold">{{ t('colabPool.recentCalls') }}</h3><table class="w-full whitespace-nowrap text-left text-sm"><thead class="text-xs text-gray-500"><tr><th>{{ t('colabPool.time') }}</th><th>{{ t('colabPool.model') }}</th><th>{{ t('colabPool.protocol') }}</th><th>{{ t('colabPool.input') }}</th><th>{{ t('colabPool.output') }}</th><th>{{ t('colabPool.duration') }}</th></tr></thead><tbody><tr v-for="log in usage?.recent || []" :key="log.id"><td>{{ new Date(log.created_at).toLocaleString() }}</td><td>{{ log.model }}</td><td>{{ log.inbound_endpoint || '—' }} {{ log.stream ? `· ${t('colabPool.stream')}` : '' }}</td><td>{{ num(log.input_tokens, 0) }}</td><td>{{ num(log.output_tokens, 0) }}</td><td>{{ log.duration_ms == null ? '—' : num(log.duration_ms / 1000, 2) + ' s' }}</td></tr></tbody></table><p v-if="!usage?.recent.length" class="py-5 text-sm text-gray-500">{{ t('colabPool.noRecords') }}</p></div>
        </div>
        <div v-else-if="tab === 'events'" class="space-y-4"><label class="flex items-center gap-2 text-sm"><input v-model="errorsOnly" type="checkbox">{{ t('colabPool.errorsOnly') }}</label><div class="pool-card"><div v-for="event in filteredEvents" :key="event.id" class="flex flex-wrap items-start justify-between gap-2 border-b border-gray-100 py-4 last:border-0 dark:border-dark-700"><p class="min-w-0 flex-1 break-words text-sm" :class="event.level === 'error' ? 'text-red-600' : ''">{{ event.msg }}</p><time class="text-xs text-gray-400">{{ date(event.ts) }}</time></div><p v-if="!filteredEvents.length" class="text-sm text-gray-500">{{ t('colabPool.noRecords') }}</p></div></div>
        <form v-else-if="tab === 'settings' && settingsDraft" class="pool-card max-w-4xl space-y-6" @submit.prevent="saveSettings">
          <div class="flex items-center justify-between gap-4"><div><h2 class="font-semibold">{{ t('colabPool.settings') }}</h2><p class="mt-2 text-sm text-gray-500">{{ t('colabPool.settingsHint') }}</p></div><button type="submit" class="btn btn-primary" :disabled="busy || !settingsDirty">{{ t('colabPool.save') }}</button></div><p v-if="settingsDirty" role="status" class="text-sm text-amber-600">{{ t('colabPool.unsaved') }}</p>
          <div class="grid gap-4 sm:grid-cols-3"><label v-for="key in toggleSettings" :key="key" class="flex items-center gap-3 rounded-xl bg-gray-50 p-4 text-sm dark:bg-dark-900"><input v-model="settingsDraft[key]" type="checkbox" :disabled="busy" @change="settingsDirty = true">{{ t(`colabPool.${key}`) }}</label></div>
          <div class="grid gap-5 sm:grid-cols-2"><label v-for="field in numericSettings" :key="field.key" class="block text-sm"><span class="mb-2 block text-gray-600 dark:text-gray-300">{{ t(`colabPool.${field.key}`) }}</span><input v-model.number="settingsDraft[field.key]" class="input" type="number" :min="field.min" :max="field.max" :step="field.step || 1" :disabled="busy" required @input="settingsDirty = true"></label><label class="block text-sm"><span class="mb-2 block">{{ t('colabPool.gpu') }}</span><select v-model="settingsDraft.gpu" class="input" :disabled="busy" @change="settingsDirty = true"><option v-for="gpu in ['G4','A100','L4','T4']" :key="gpu">{{ gpu }}</option></select></label></div><p class="text-xs text-gray-500">{{ t('colabPool.gpuNote') }}</p>
        </form>
        <p class="text-right text-xs text-gray-400">{{ t('colabPool.updated') }} · {{ date(state.now) }}</p>
      </template>
    </div>
    <BaseDialog :show="showAuth" :title="t('colabPool.addAccount')" @close="closeAuth">
      <div class="space-y-4"><p class="text-sm text-gray-500">{{ t('colabPool.authHelp') }}</p><button class="btn btn-secondary" :disabled="busy" @click="generateAuth">{{ t('colabPool.generate') }}</button><template v-if="authFlow"><div v-if="authURL" class="flex flex-wrap gap-3"><a :href="authURL" target="_blank" rel="noopener noreferrer" class="btn btn-primary">{{ t('colabPool.openGoogle') }}</a><button class="btn btn-secondary" @click="copyAuth">{{ t('colabPool.copyLink') }}</button></div><p class="text-xs text-gray-500">{{ t('colabPool.authExpiry') }} {{ date(authFlow.expires_at) }}</p><p v-if="authFlow.phase === 'exchanged'" class="text-sm text-primary-600">{{ t('colabPool.authExchanged') }}</p><label class="block text-sm">{{ t('colabPool.authCode') }}<input v-model="authCode" class="input mt-2" type="password" autocomplete="off" :spellcheck="false" maxlength="2048" :disabled="busy || authFlow.phase === 'exchanged'"></label></template><p v-if="authError" role="alert" class="break-words text-sm text-red-600">{{ authError }}</p></div>
      <template #footer><div class="flex justify-end gap-3"><button class="btn btn-secondary" :disabled="busy" @click="closeAuth">{{ t('colabPool.cancel') }}</button><button class="btn btn-primary" :disabled="busy || !authFlow || (!authCode.trim() && authFlow.phase !== 'exchanged')" @click="finishAuth">{{ t('colabPool.authFinish') }}</button></div></template>
    </BaseDialog>
    <BaseDialog :show="notesAccount !== null" :title="t('colabPool.editNotes')" @close="notesAccount = null"><label class="block text-sm">{{ t('colabPool.notes') }}<textarea v-model="notesDraft" class="input mt-2" maxlength="2000" rows="4" /></label><template #footer><button class="btn btn-primary" :disabled="busy" @click="saveNotes">{{ t('colabPool.save') }}</button></template></BaseDialog>
    <ConfirmDialog :show="!!confirmation" :title="confirmation?.title || ''" :message="confirmation?.message || ''" :danger="confirmation?.danger" @confirm="runConfirmed" @cancel="confirmation = null" />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import ColabInstanceCard from '@/components/colab/ColabInstanceCard.vue'
import { useAppStore } from '@/stores/app'
import * as api from '@/api/admin/colabPool'
import type { PoolAccount, PoolInstance, PoolSettings, PoolSnapshot, PoolUsage, OAuthFlow } from '@/api/admin/colabPool'

const { t } = useI18n(), route = useRoute(), router = useRouter(), appStore = useAppStore()
const tabs = ['overview', 'accounts', 'instances', 'monitoring', 'events', 'settings'] as const
type Tab = typeof tabs[number]
const tab = computed<Tab>(() => tabs.includes(route.query.tab as Tab) ? route.query.tab as Tab : 'overview')
const selectTab = (value: Tab) => { void router.replace({ query: { tab: value } }); if (value === 'monitoring') void refreshUsage() }
const snapshot = ref<PoolSnapshot | null>(null), state = computed(() => snapshot.value?.state)
const loading = ref(false), busy = ref(false), error = ref(''), errorsOnly = ref(false)
const usage = ref<PoolUsage | null>(null), usageError = ref(false)
let usageLoading = false, poll: ReturnType<typeof setInterval> | undefined, usagePoll: ReturnType<typeof setInterval> | undefined
const settingsDraft = ref<PoolSettings | null>(null), settingsDirty = ref(false)
const startAccount = ref(''), showAuth = ref(false), authFlow = ref<OAuthFlow | null>(null), authCode = ref(''), authError = ref('')
const authURL = computed(() => api.safeAuthURL(authFlow.value))
const notesAccount = ref<PoolAccount | null>(null), notesDraft = ref('')
const confirmation = ref<{ title: string; message: string; danger: boolean; run: () => Promise<unknown> } | null>(null)
const liveStates = ['starting', 'ready', 'draining', 'stopping']
const activeInstances = computed(() => state.value?.instances.filter(i => liveStates.includes(i.state)) || [])
const endedInstances = computed(() => state.value?.instances.filter(i => !liveStates.includes(i.state)) || [])
const eligibleAccounts = computed(() => state.value?.accounts.filter(a => a.enabled && a.status === 'ok' && !a.instance && !a.cooldown && !a.assignments && a.balance != null && a.balance >= state.value!.settings.min_start_cu && a.gpus.includes(state.value!.settings.gpu)) || [])
const maxBalance = computed(() => Math.max(1, ...(state.value?.accounts.map(a => a.balance || 0) || [])))
const filteredEvents = computed(() => (state.value?.events || []).filter(e => !errorsOnly.value || e.level === 'error'))
const num = (n: number | null | undefined, d = 1) => typeof n === 'number' && Number.isFinite(n) ? n.toLocaleString(undefined, { maximumFractionDigits: d }) : '—'
const date = (n: number | null | undefined) => n ? new Date(n * 1000).toLocaleString() : '—'
const message = (e: unknown) => { const v = e as { response?: { data?: { message?: string } }; message?: string }; return v.response?.data?.message || v.message || t('colabPool.unavailable') }
const spark = (values: number[]) => { const min = Math.min(...values), span = Math.max(...values) - min || 1; return values.map((v, i) => `${i / (values.length - 1) * 100},${30 - (v - min) / span * 28}`).join(' ') }
const accountStatus = (a: PoolAccount) => t('colabPool.' + (!a.enabled ? 'disabled' : a.status === 'auth_error' ? 'authError' : a.status === 'error' ? 'accountError' : a.instance ? 'assigned' : a.cooldown ? 'cooldown' : eligibleAccounts.value.some(x => x.id === a.id) ? 'available' : 'insufficient'))
const instanceProps = (instance: PoolInstance) => {
  const telemetry = snapshot.value?.telemetry
  return { instance, resource: telemetry?.resources?.[instance.id], now: state.value?.now || Date.now()/1000, busy: busy.value, telemetryAvailable: !!snapshot.value?.telemetry_available && !error.value, calls: telemetry?.calls?.instances?.[instance.id], callsFresh: !!snapshot.value?.telemetry_available && !error.value && !telemetry?.error && Date.now()/1000 - (telemetry?.updated_at || 0) < 30 }
}
const usageMetrics = computed(() => [
  { label: t('colabPool.requests'), value: num(usageError.value ? undefined : usage.value?.requests, 0) },
  { label: t('colabPool.input'), value: num(usageError.value ? undefined : usage.value?.input, 0) },
  { label: t('colabPool.output'), value: num(usageError.value ? undefined : usage.value?.output, 0) },
  { label: t('colabPool.duration'), value: num(!usageError.value && usage.value ? usage.value.averageMs / 1000 : undefined, 2) + ' s' },
  { label: t('colabPool.cost'), value: '$' + num(usageError.value ? undefined : usage.value?.cost, 4) }
])
const toggleSettings = ['auto_start', 'auto_rotate', 'autoscale'] as const
const numericSettings: { key: Exclude<keyof PoolSettings, typeof toggleSettings[number] | 'gpu'>; min: number; max: number; step?: number }[] = [
  { key: 'max_instances', min: 1, max: 16 }, { key: 'idle_stop_min', min: 1, max: 1440 }, { key: 'switch_below_cu', min: 0, max: 100000, step: 0.5 }, { key: 'min_start_cu', min: 0, max: 100000, step: 0.5 }, { key: 'scale_up_running', min: 1, max: 10000 }, { key: 'scale_up_sustain_sec', min: 15, max: 3600 }, { key: 'scale_down_min', min: 1, max: 1440 }
]
async function load() {
  if (loading.value) return
  loading.value = true
  try { snapshot.value = await api.snapshot(); error.value = ''; if (!settingsDirty.value) settingsDraft.value = { ...snapshot.value.state.settings } }
  catch (e) { error.value = message(e) }
  finally { loading.value = false }
}
async function refreshUsage() {
  if (usageLoading) return
  usageLoading = true
  try { usage.value = await api.loadUsage(); usageError.value = false } catch { usageError.value = true } finally { usageLoading = false }
}
function refreshAll() { void load(); void refreshUsage() }
async function act(action: () => Promise<unknown>, success = t('colabPool.operationDone')) {
  if (busy.value) return
  busy.value = true
  try { await action(); appStore.showSuccess(success); await load() } catch (e) { appStore.showError(message(e)) } finally { busy.value = false }
}
function toggleAccount(a: PoolAccount, e: Event) { const enabled = (e.target as HTMLInputElement).checked; (e.target as HTMLInputElement).checked = !!a.enabled; void act(() => api.updateAccount(a.id, { enabled })) }
function confirmStart() { confirmation.value = { title: t('colabPool.startConfirm'), message: t('colabPool.startMessage'), danger: false, run: () => api.startInstance(startAccount.value ? Number(startAccount.value) : undefined) } }
function confirmStop(id: number, drain: boolean) { confirmation.value = { title: t('colabPool.stopConfirm'), message: t(drain ? 'colabPool.drainMessage' : 'colabPool.stopMessage'), danger: !drain, run: () => api.stopInstance(id, drain) } }
function confirmDelete(a: PoolAccount) { confirmation.value = { title: t('colabPool.deleteConfirm'), message: a.email + ' — ' + t('colabPool.deleteMessage'), danger: true, run: () => api.deleteAccount(a.id) } }
function runConfirmed() { const item = confirmation.value; confirmation.value = null; if (item) void act(item.run) }
function editNotes(a: PoolAccount) { notesAccount.value = a; notesDraft.value = a.note || '' }
async function saveNotes() { const a = notesAccount.value; if (!a) return; await act(async () => { await api.updateAccount(a.id, { note: notesDraft.value }); notesAccount.value = null }, t('colabPool.saved')) }
async function saveSettings() { if (!settingsDraft.value) return; const next = { ...settingsDraft.value }; await act(async () => { await api.updateSettings(next); settingsDirty.value = false }, t('colabPool.saved')) }
const authStorage = 'sub2api-colab-authorization-pid'
function rememberAuth(flow: OAuthFlow | null) { authFlow.value = flow; try { if (flow) sessionStorage.setItem(authStorage, flow.pid); else sessionStorage.removeItem(authStorage) } catch { /* storage is optional */ } }
function closeAuth() { if (!busy.value) { showAuth.value = false; authCode.value = ''; authError.value = '' } }
async function generateAuth() { if (busy.value) return; busy.value = true; authError.value = ''; authCode.value = ''; try { rememberAuth(await api.beginAuth()); if (!authURL.value) throw new Error(t('colabPool.authExpired')) } catch (e) { authError.value = message(e) } finally { busy.value = false } }
async function copyAuth() { if (authURL.value) { try { await navigator.clipboard.writeText(authURL.value); appStore.showSuccess(t('colabPool.copied')) } catch (e) { authError.value = message(e) } } }
async function finishAuth() {
  if (busy.value || !authFlow.value) return
  if (authFlow.value.expires_at <= Date.now()/1000) { authError.value = t('colabPool.authExpired'); return }
  busy.value = true; authError.value = ''
  try { await api.finishAuth(authFlow.value.pid, authCode.value.trim()); rememberAuth(null); authCode.value = ''; showAuth.value = false; appStore.showSuccess(t('colabPool.authSaved')); await load() }
  catch (e) { authError.value = message(e); try { rememberAuth(await api.pendingAuth(authFlow.value!.pid)) } catch { rememberAuth(null) } }
  finally { busy.value = false }
}
onMounted(() => {
  refreshAll()
  poll = setInterval(() => { if (!document.hidden) void load() }, 5000)
  usagePoll = setInterval(() => { if (!document.hidden && tab.value === 'monitoring') void refreshUsage() }, 30000)
  try { const pid = sessionStorage.getItem(authStorage); if (pid && /^[a-f0-9]{32}$/.test(pid)) void api.pendingAuth(pid).then(rememberAuth).catch(() => rememberAuth(null)) } catch { /* storage is optional */ }
})
onUnmounted(() => { clearInterval(poll); clearInterval(usagePoll); authCode.value = '' })
</script>

<style scoped>
.pool-card { @apply rounded-2xl border border-gray-200 bg-white p-5 shadow-sm dark:border-dark-700 dark:bg-dark-800; }
.metric-label { @apply text-sm text-gray-500 dark:text-gray-400; }
.metric-value { @apply mt-3 text-3xl font-semibold tabular-nums tracking-tight; }
.metric-value small { @apply text-sm font-normal text-gray-400; }
.metric-note { @apply mt-3 text-xs leading-5 text-gray-400; }
.step-button { @apply h-7 w-7 rounded-lg border border-gray-200 text-sm hover:bg-gray-50 disabled:opacity-50 dark:border-dark-600 dark:hover:bg-dark-700; }
th, td { @apply border-b border-gray-100 px-3 py-3 dark:border-dark-700; }
</style>
