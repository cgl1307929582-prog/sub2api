import { beforeEach, describe, expect, it, vi } from 'vitest'
import { loadUsage, safeAuthURL, snapshot, startInstance, stopInstance, updateSettings, type OAuthFlow, type PoolSettings } from '../colabPool'
import * as accounts from '../accounts'
import * as usage from '../usage'
import { apiClient } from '../../client'
vi.mock('../../client', () => ({ apiClient: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() } }))
vi.mock('../accounts', () => ({ list: vi.fn() }))
vi.mock('../usage', () => ({ getStats: vi.fn(), list: vi.fn() }))
const flow = (url: string): OAuthFlow => ({ url, pid: 'a'.repeat(32), expires_at: 9999999999, phase: 'pending' })
describe('Colab admin API', () => {
  beforeEach(() => vi.clearAllMocks())
  it('accepts only Google authorization endpoints', () => {
    expect(safeAuthURL(flow('https://accounts.google.com/o/oauth2/v2/auth?state=abc&code_challenge=test'))).toContain('code_challenge=test')
    for (const url of ['javascript:alert(1)', 'https://accounts.google.com.evil.test/o/oauth2/auth', 'https://evil@accounts.google.com/o/oauth2/auth', 'http://accounts.google.com/o/oauth2/auth', 'https://accounts.google.com:444/o/oauth2/auth', 'https://accounts.google.com/delete']) expect(safeAuthURL(flow(url))).toBeNull()
  })
  it('uses only authenticated Sub2API admin paths for read and lifecycle operations', async () => {
    vi.mocked(apiClient.get).mockResolvedValue({ data: {} }); vi.mocked(apiClient.post).mockResolvedValue({ data: {} }); vi.mocked(apiClient.put).mockResolvedValue({ data: {} })
    await snapshot(); await startInstance(7); await stopInstance(4, true); await updateSettings({ auto_start: true } as PoolSettings)
    expect(apiClient.get).toHaveBeenCalledWith('/admin/colab-pool/snapshot', expect.any(Object))
    expect(apiClient.post).toHaveBeenCalledWith('/admin/colab-pool/instances', { account_id: 7 })
    expect(apiClient.post).toHaveBeenCalledWith('/admin/colab-pool/instances/4/stop', { drain: true })
    expect(apiClient.put).toHaveBeenCalledWith('/admin/colab-pool/settings', { auto_start: true })
  })
  it('aggregates only Colab channels and weights request duration', async () => {
    vi.mocked(accounts.list).mockResolvedValue({ items: [{ id: 3, name: 'Pool A' }, { id: 8, name: 'Pool B' }], pages: 1 } as Awaited<ReturnType<typeof accounts.list>>)
    vi.mocked(usage.getStats).mockImplementation(async ({ account_id }) => ({ total_requests: account_id === 3 ? 2 : 1, total_input_tokens: 100, total_output_tokens: 10, total_actual_cost: 0.01, average_duration_ms: account_id === 3 ? 1000 : 4000 }) as Awaited<ReturnType<typeof usage.getStats>>)
    vi.mocked(usage.list).mockResolvedValue({ items: [], pages: 1 } as Awaited<ReturnType<typeof usage.list>>)
    const result = await loadUsage()
    expect(accounts.list).toHaveBeenCalledWith(1, 100, { platform: 'colab', lite: 'true' })
    expect(vi.mocked(usage.getStats).mock.calls.map(([args]) => args.account_id)).toEqual([3, 8])
    expect(result).toMatchObject({ requests: 3, averageMs: 2000, input: 200, output: 20, cost: 0.02 })
  })
  it('does not turn a failed usage read into zero usage', async () => {
    vi.mocked(accounts.list).mockRejectedValue(new Error('offline'))
    await expect(loadUsage()).rejects.toThrow('offline')
  })
})
