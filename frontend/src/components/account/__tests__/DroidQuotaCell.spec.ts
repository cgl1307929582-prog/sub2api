import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import type { AccountUsageInfo, DroidQuotaInfo } from '@/types'
import DroidQuotaCell from '../DroidQuotaCell.vue'
import UsageProgressBar from '../UsageProgressBar.vue'

vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string, params?: object) => params ? `${key} ${JSON.stringify(params)}` : key })
}))

const snapshot = (): DroidQuotaInfo => ({
  standard: {
    five_hour: { used_percent: 12, resets_at: '2026-11-01T12:00:00Z' },
    weekly: { used_percent: 25, resets_at: '2026-11-07T12:00:00Z' },
    monthly: { used_percent: 30, resets_at: '2026-11-30T12:00:00Z' }
  },
  core: {
    five_hour: { used_percent: 0, resets_at: null },
    weekly: null,
    monthly: { used_percent: 1, resets_at: '2026-11-30T12:00:00Z' }
  },
  extra_usage_balance_cents: 0,
  extra_usage_allowed: true,
  uses_token_rate_limits_billing: true,
  fetched_at: '2026-10-16T02:39:00Z',
  stale: false
})
const usage = (quota: DroidQuotaInfo | null, errorCode?: string): AccountUsageInfo => ({
  updated_at: quota?.fetched_at ?? null,
  five_hour: null, seven_day: null, seven_day_sonnet: null,
  droid_quota: quota, error_code: errorCode
})

describe('DroidQuotaCell', () => {
  it('shows separate Standard/Core windows, genuine zero balance, and the observation time', () => {
    const wrapper = mount(DroidQuotaCell, { props: { usage: usage(snapshot()), loading: false, error: null } })
    expect(wrapper.findAllComponents(UsageProgressBar).map(bar => bar.props('utilization'))).toEqual([12, 25, 30, 1])
    expect(wrapper.get('[data-test="droid-extra-balance"]').text()).toContain('$0.00')
    expect(wrapper.get('[data-test="droid-core-five_hour-inactive"]').text()).toContain('factoryQuota.inactive')
    expect(wrapper.get('[data-test="droid-core"]').text()).toContain('factoryQuota.unknown')
    expect(wrapper.get('[data-test="droid-fetched-at"]').text()).toContain('2026')
    expect(wrapper.findAllComponents(UsageProgressBar)[2].attributes('title')).toContain('70')
    wrapper.unmount()
  })

  it('keeps missing balances and windows unknown rather than inventing zero', () => {
    const quota = snapshot()
    quota.extra_usage_balance_cents = null
    quota.standard = null
    quota.core = null
    const wrapper = mount(DroidQuotaCell, { props: { usage: usage(quota), loading: false, error: null } })
    expect(wrapper.findAllComponents(UsageProgressBar)).toHaveLength(0)
    expect(wrapper.get('[data-test="droid-extra-balance"]').text()).toContain('factoryQuota.unknown')
    expect(wrapper.text()).not.toContain('$0.00')
    wrapper.unmount()
  })

  it('formats cents as USD and labels a retained snapshot after a query failure', () => {
    const quota = snapshot()
    quota.extra_usage_balance_cents = 1234
    quota.stale = true
    const wrapper = mount(DroidQuotaCell, { props: { usage: usage(quota, 'forbidden'), loading: false, error: null } })
    expect(wrapper.get('[data-test="droid-extra-balance"]').text()).toContain('$12.34')
    expect(wrapper.get('[data-test="droid-stale"]').text()).toContain('factoryQuota.stale')
    expect(wrapper.get('[role="status"]').text()).toContain('factoryQuota.forbidden')
    wrapper.unmount()
  })

  it('offers retry without displaying a fake balance on the first failed query', async () => {
    const wrapper = mount(DroidQuotaCell, { props: { usage: usage(null, 'unauthenticated'), loading: false, error: null } })
    expect(wrapper.find('[data-test="droid-extra-balance"]').exists()).toBe(false)
    expect(wrapper.get('[role="status"]').text()).toContain('factoryQuota.unauthenticated')
    await wrapper.get('[data-test="droid-refresh"]').trigger('click')
    expect(wrapper.emitted('refresh')).toHaveLength(1)
    await wrapper.setProps({ loading: true })
    expect(wrapper.get('[data-test="droid-refresh"]').attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })
})
