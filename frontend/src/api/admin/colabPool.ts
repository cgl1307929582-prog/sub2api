import { apiClient } from '../client'
import * as accounts from './accounts'
import * as usage from './usage'
import type { AdminUsageLog } from '@/types'

export interface PoolSettings {
  auto_start: boolean; auto_rotate: boolean; autoscale: boolean
  max_instances: number; idle_stop_min: number; switch_below_cu: number; min_start_cu: number
  scale_up_running: number; scale_up_sustain_sec: number; scale_down_min: number; gpu: string
}
export interface PoolAccount {
  id: number; email: string; name: string | null; enabled: number | boolean; priority: number; note: string
  balance: number | null; rate: number | null; assignments: number | null; gpus: string[]; status: string
  error: string | null; checked_at: number | null; cooldown: boolean; instance: number | null
  used_24h: number; used_7d: number; history: number[]
}
export interface PoolInstance {
  id: number; account_id: number; email: string; state: string; phase: string; reason: string
  created_at: number; ended_at: number | null; last_active: number | null
  running: number; waiting: number; tps: number; gen_tokens: number
}
export interface PoolEvent { id: number; ts: number; level: string; msg: string }
export interface PoolState {
  now: number
  summary: { balance: number; hours: number; accounts: number; usable: number; ready: number; starting: number; burn: number; used_24h: number; last_demand: number }
  settings: PoolSettings; accounts: PoolAccount[]; instances: PoolInstance[]; events: PoolEvent[]
}
export interface PoolResource {
  cpu?: { percent: number; model: string; logical_cores: number; load_average: number[] }
  ram?: { used_bytes: number; total_bytes: number }; disk?: { used_bytes: number; total_bytes: number }
  gpus?: { name: string; percent: number; memory_used_bytes: number; memory_total_bytes: number; temperature_c: number; power_w: number; power_limit_w: number }[]
  engine?: { context: number; parallel: number }; received_at?: number; stale?: boolean; error?: string
}
export interface PoolTelemetry {
  resources: Record<string, PoolResource>
  calls: { instances: Record<string, { requests: number; errors: number; rpm: number }>; since?: number }
  updated_at?: number; error?: string
}
export interface PoolSnapshot { state: PoolState; telemetry: PoolTelemetry | null; telemetry_available: boolean }
export interface OAuthFlow { pid: string; url: string | null; expires_at: number; phase: string }
const base = '/admin/colab-pool'
export const snapshot = async () => (await apiClient.get<PoolSnapshot>(base + '/snapshot', { timeout: 20000 })).data
export const beginAuth = async () => (await apiClient.post<OAuthFlow>(base + '/auth/start')).data
export const pendingAuth = async (pid: string) => (await apiClient.get<OAuthFlow>(base + '/auth/pending/' + encodeURIComponent(pid))).data
export const finishAuth = async (pid: string, code: string) => (await apiClient.post<PoolAccount>(base + '/auth/finish', { pid, code }, { timeout: 90000 })).data
export const updateAccount = async (id: number, patch: { enabled?: boolean; priority?: number; note?: string; refresh?: boolean }) => (await apiClient.patch<PoolAccount>(`${base}/accounts/${id}`, patch, { timeout: 90000 })).data
export const deleteAccount = async (id: number) => (await apiClient.delete(`${base}/accounts/${id}`)).data
export const startInstance = async (accountID?: number) => (await apiClient.post<{ id: number }>(base + '/instances', accountID ? { account_id: accountID } : {})).data
export const stopInstance = async (id: number, drain = false) => (await apiClient.post(`${base}/instances/${id}/stop`, { drain })).data
export const updateSettings = async (settings: PoolSettings) => (await apiClient.put<PoolSettings>(base + '/settings', settings)).data
export function safeAuthURL(flow: OAuthFlow | null): string | null {
  if (!flow?.url) return null
  try { const u = new URL(flow.url); return u.protocol === 'https:' && u.hostname === 'accounts.google.com' && !u.username && !u.password && !u.port && ['/o/oauth2/auth', '/o/oauth2/v2/auth'].includes(u.pathname) ? u.href : null } catch { return null }
}
export interface PoolUsage { requests: number; input: number; output: number; cost: number; averageMs: number; recent: AdminUsageLog[]; channels: { id: number; name: string }[] }
export async function loadUsage(): Promise<PoolUsage> {
  const channels: { id: number; name: string }[] = []
  for (let page = 1; ; page++) {
    const result = await accounts.list(page, 100, { platform: 'colab', lite: 'true' })
    channels.push(...result.items.map(a => ({ id: a.id, name: a.name })))
    if (page >= result.pages || result.items.length === 0) break
  }
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone
  const result: PoolUsage = { requests: 0, input: 0, output: 0, cost: 0, averageMs: 0, recent: [], channels }
  // Bounded concurrency also keeps a large multi-pool installation from flooding the API.
  for (let offset = 0; offset < channels.length; offset += 4) {
    const batch = await Promise.all(channels.slice(offset, offset + 4).map(async a => {
      const [stats, logs] = await Promise.all([usage.getStats({ account_id: a.id, period: 'today', timezone }), usage.list({ account_id: a.id, page: 1, page_size: 20, sort_order: 'desc' })])
      return { stats, logs }
    }))
    for (const { stats, logs } of batch) {
      result.requests += stats.total_requests; result.input += stats.total_input_tokens; result.output += stats.total_output_tokens
      result.cost += stats.total_actual_cost; result.averageMs += stats.average_duration_ms * stats.total_requests
      result.recent.push(...logs.items)
    }
  }
  result.averageMs = result.requests ? result.averageMs / result.requests : 0
  result.recent.sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at)); result.recent = result.recent.slice(0, 20)
  return result
}
