import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import type { AccountUsageInfo, ColabPoolInfo } from '@/types'
import ColabPoolCell from '../ColabPoolCell.vue'
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string, params?: object) => params ? `${key} ${JSON.stringify(params)}` : key })
}))
const snapshot = (): ColabPoolInfo => ({
  version: 1, model_id: 'local-model', balance_cu: 560.77, burn_cu_per_hour: 0, used_cu_24h: 18.51,
  accounts: 5, eligible_accounts: 4, ready: 0, starting: 0, auto_start: true, max_instances: 2,
  idle_stop_minutes: 60, observed_at: '2026-10-16T05:00:00Z', quota_updated_at: '2026-10-16T04:59:00Z',
  quota_stale: false, fetched_at: '2026-10-16T05:00:01Z', stale: false,
  instances: [{ id: 24, state: 'stopped', gpu: 'G4', running: 0, waiting: 0, tokens_per_second: 0, created_at: null, last_active_at: null }]
})
const usage = (pool: ColabPoolInfo | null, code?: string): AccountUsageInfo => ({
  five_hour: null, seven_day: null, seven_day_sonnet: null, updated_at: null, colab_pool: pool, error_code: code
})
describe('ColabPoolCell', () => {
  it('shows CU, offline state, historical instances and on-demand configuration', () => {
    const wrapper = mount(ColabPoolCell, { props: { usage: usage(snapshot()), loading: false, error: null } })
    expect(wrapper.get('[data-test="colab-balance"]').text()).toContain('560.77 CU')
    expect(wrapper.text()).toContain('0.00 CU/h')
    expect(wrapper.get('[data-test="colab-offline"]').text()).toContain('colab.offline')
    expect(wrapper.text()).toContain('colab.states.stopped')
    expect(wrapper.text()).toContain('colab.coldStart')
    expect(wrapper.text()).not.toContain('$')
  })
  it('keeps unknown credits distinct from zero and labels old data', () => {
    const pool = snapshot()
    pool.balance_cu = null
    pool.quota_stale = true
    pool.stale = true
    const wrapper = mount(ColabPoolCell, { props: { usage: usage(pool), loading: false, error: null } })
    expect(wrapper.get('[data-test="colab-balance"]').text()).toContain('colab.unknown')
    expect(wrapper.find('[data-test="colab-quota-stale"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="colab-stale"]').exists()).toBe(true)
  })
  it('refreshes metadata without an instance lifecycle control', async () => {
    const wrapper = mount(ColabPoolCell, { props: { usage: usage(null, 'unauthenticated'), loading: false, error: null } })
    expect(wrapper.find('[data-test="colab-balance"]').exists()).toBe(false)
    expect(wrapper.get('[role="status"]').text()).toContain('colab.unauthenticated')
    expect(wrapper.findAll('button')).toHaveLength(1)
    await wrapper.get('[data-test="colab-refresh"]').trigger('click')
    expect(wrapper.emitted('refresh')).toHaveLength(1)
    await wrapper.setProps({ loading: true })
    expect(wrapper.get('button').attributes('disabled')).toBeDefined()
  })
})
