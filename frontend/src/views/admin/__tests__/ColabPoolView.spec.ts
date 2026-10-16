import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'
import ColabPoolView from '../ColabPoolView.vue'
import * as api from '@/api/admin/colabPool'
import type { PoolSnapshot } from '@/api/admin/colabPool'
const mocks = vi.hoisted(() => ({ route: null as null | { query: Record<string, string> }, success: vi.fn(), error: vi.fn() }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<div><slot /></div>' } }))
vi.mock('vue-router', async () => {
  const { reactive } = await import('vue'); const route = reactive({ query: {} as Record<string, string> }); mocks.route = route
  return { useRoute: () => route, useRouter: () => ({ replace: ({ query }: { query: Record<string, string> }) => { route.query = query; return Promise.resolve() } }) }
})
vi.mock('vue-i18n', async () => ({ ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'), useI18n: () => ({ t: (key: string) => key, te: () => true }) }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: mocks.success, showError: mocks.error }) }))
vi.mock('@/api/admin/colabPool', () => ({ snapshot: vi.fn(), loadUsage: vi.fn(), safeAuthURL: vi.fn(), beginAuth: vi.fn(), pendingAuth: vi.fn(), finishAuth: vi.fn(), updateAccount: vi.fn(), deleteAccount: vi.fn(), startInstance: vi.fn(), stopInstance: vi.fn(), updateSettings: vi.fn() }))
function fixture(): PoolSnapshot {
  return { telemetry: null, telemetry_available: false, state: { now: Date.now()/1000, summary: { balance: 123.4, hours: 13.9, accounts: 1, usable: 1, ready: 0, starting: 0, burn: 0, used_24h: 2, last_demand: 0 }, settings: { auto_start: true, auto_rotate: true, autoscale: true, max_instances: 2, idle_stop_min: 60, switch_below_cu: 4, min_start_cu: 8, scale_up_running: 8, scale_up_sustain_sec: 120, scale_down_min: 15, gpu: 'G4' }, accounts: [{ id: 1, email: 'pool@example.test', name: null, enabled: 1, priority: 0, note: '', balance: 123.4, rate: 0, assignments: 0, gpus: ['G4'], status: 'ok', error: null, checked_at: 1, cooldown: false, instance: null, used_24h: 2, used_7d: 3, history: [125, 123.4] }], instances: [], events: [] } }
}
let wrapper: VueWrapper
const button = (label: string) => { const b = wrapper.findAll('button').find(b => b.text() === label); if (!b) throw new Error('Missing button: ' + label); return b }
async function render(tab = 'overview') {
  mocks.route!.query = { tab }
  wrapper = mount(ColabPoolView, { global: { stubs: { AppLayout: { template: '<div><slot /></div>' }, Icon: true, BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' } } } }); await flushPromises()
}
beforeEach(() => {
  vi.useFakeTimers(); vi.clearAllMocks(); sessionStorage.clear()
  vi.mocked(api.snapshot).mockResolvedValue(fixture()); vi.mocked(api.loadUsage).mockResolvedValue({ requests: 0, input: 0, output: 0, cost: 0, averageMs: 0, recent: [], channels: [] })
})
afterEach(() => { wrapper?.unmount(); vi.useRealTimers() })
describe('native Colab management', () => {
  it('renders six native sections and reading/polling never starts a GPU', async () => {
    await render(); expect(wrapper.findAll('nav button')).toHaveLength(6); expect(wrapper.find('iframe').exists()).toBe(false)
    expect(wrapper.text()).toContain('123.4'); expect(wrapper.text()).toContain('colabPool.noActive')
    await vi.advanceTimersByTimeAsync(5000); expect(api.startInstance).not.toHaveBeenCalled(); expect(api.stopInstance).not.toHaveBeenCalled(); expect(api.updateSettings).not.toHaveBeenCalled()
  })
  it('requires an explicit confirmation before starting', async () => {
    await render('instances'); await button('colabPool.start').trigger('click'); expect(api.startInstance).not.toHaveBeenCalled()
    await button('common.confirm').trigger('click'); await flushPromises(); expect(api.startInstance).toHaveBeenCalledOnce()
  })
  it('preserves unsaved settings while new snapshots arrive and saves explicitly', async () => {
    await render('settings'); const input = wrapper.findAll('input[type="number"]')[0]; await input.setValue(3)
    await vi.advanceTimersByTimeAsync(5000); expect((input.element as HTMLInputElement).value).toBe('3'); expect(api.updateSettings).not.toHaveBeenCalled()
    await wrapper.get('form').trigger('submit'); await flushPromises(); expect(api.updateSettings).toHaveBeenCalledWith(expect.objectContaining({ max_instances: 3 }))
  })
  it('keeps the last balance visible and marks failed refreshes', async () => {
    await render(); vi.mocked(api.snapshot).mockRejectedValue(new Error('offline')); await button('colabPool.refresh').trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('123.4'); expect(wrapper.get('[role="alert"]').text()).toContain('colabPool.stale')
  })
  it('retains the last hardware sample when only telemetry becomes unavailable', async () => {
    const live = fixture()
    live.state.instances = [{ id: 26, account_id: 1, email: 'pool@example.test', state: 'ready', phase: '', reason: '', created_at: Date.now()/1000, ended_at: null, last_active: null, running: 0, waiting: 0, tps: 0, gen_tokens: 0 }]
    live.telemetry_available = true
    live.telemetry = { resources: { '26': { cpu: { percent: 44, model: 'Recorded CPU', logical_cores: 48, load_average: [] }, received_at: Date.now()/1000 } }, calls: { instances: {} } }
    vi.mocked(api.snapshot).mockResolvedValue(live)
    await render('instances'); expect(wrapper.text()).toContain('Recorded CPU')
    vi.mocked(api.snapshot).mockResolvedValue({ ...live, telemetry: null, telemetry_available: false })
    await button('colabPool.refresh').trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('Recorded CPU'); expect(wrapper.text()).toContain('colabPool.resourceStale')
  })
  it('opens account authorization without starting a flow or sending a credential', async () => {
    await render('accounts'); await button('colabPool.addAccount').trigger('click')
    expect(wrapper.text()).toContain('colabPool.authHelp'); expect(api.beginAuth).not.toHaveBeenCalled(); expect(api.finishAuth).not.toHaveBeenCalled()
  })
})
