import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ConversationDetailDialog from '../ConversationDetailDialog.vue'
import ConversationLogSettings from '../ConversationLogSettings.vue'

const api = vi.hoisted(() => ({
  getConversation: vi.fn(), getConversationConfig: vi.fn(), updateConversationConfig: vi.fn(),
}))
vi.mock('@/api/admin/conversations', () => api)
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/stores', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
vi.mock('@/utils/format', () => ({ formatDateTime: (value: string) => value }))

const global = { stubs: { BaseDialog: {
  props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>',
} } }
const reply = (text = 'Answer') => ({
  id: 1, created_at: '2026-10-25', model: 'test-model', endpoint: '/v1/messages',
  user_text: '<img src=x onerror="alert(1)">', assistant_text: text, error_text: '',
  response_complete: true, input_truncated: false, output_truncated: false,
})
const wrappers: ReturnType<typeof mount>[] = []
afterEach(() => { wrappers.splice(0).forEach(w => w.unmount()) })
beforeEach(() => {
  vi.resetAllMocks()
  api.getConversation.mockResolvedValue(reply())
  api.getConversationConfig.mockResolvedValue({ enabled: false, retention_days: 7 })
})

describe('conversation content administration', () => {
  it('renders caller and model content as literal text, never HTML', async () => {
    const wrapper = mount(ConversationDetailDialog, { props: { show: true, recordId: 1 }, global })
    wrappers.push(wrapper)
    await flushPromises()
    expect(wrapper.find('img').exists()).toBe(false)
    expect(wrapper.findAll('pre').map(p => p.text())).toEqual([reply().user_text, 'Answer'])
  })

  it('explains missing or expired content and clears previous content', async () => {
    const wrapper = mount(ConversationDetailDialog, { props: { show: true, recordId: 1 }, global })
    wrappers.push(wrapper)
    await flushPromises()
    api.getConversation.mockRejectedValueOnce({ response: { status: 404 } })
    await wrapper.setProps({ recordId: 2 })
    await flushPromises()
    expect(wrapper.text()).toContain('admin.usage.conversation.unavailable')
    expect(wrapper.find('pre').exists()).toBe(false)
  })

  it('aborts superseded requests and cannot display another selected record', async () => {
    let resolveOld!: (value: unknown) => void
    api.getConversation.mockReturnValueOnce(new Promise(resolve => { resolveOld = resolve }))
    const wrapper = mount(ConversationDetailDialog, { props: { show: true, recordId: 1 }, global })
    wrappers.push(wrapper)
    const oldSignal = api.getConversation.mock.calls[0][2] as AbortSignal
    await wrapper.setProps({ recordId: 2 })
    await flushPromises()
    resolveOld(reply('Wrong record'))
    await flushPromises()
    expect(oldSignal.aborted).toBe(true)
    expect(wrapper.text()).toContain('Answer')
    expect(wrapper.text()).not.toContain('Wrong record')
    await wrapper.setProps({ show: false })
    expect((api.getConversation.mock.calls[1][2] as AbortSignal).aborted).toBe(true)
  })

  it('saves recording and retention together, including indefinite retention', async () => {
    api.updateConversationConfig.mockResolvedValue({ enabled: true, retention_days: 0 })
    const wrapper = mount(ConversationLogSettings, { global })
    wrappers.push(wrapper)
    await flushPromises()
    await wrapper.find('button').trigger('click')
    await flushPromises()
    await wrapper.find('input[type="checkbox"]').setValue(true)
    await wrapper.find('select').setValue('0')
    const save = wrapper.findAll('button').find(button => button.text() === 'common.save')!
    await save.trigger('click')
    await flushPromises()
    expect(api.updateConversationConfig).toHaveBeenCalledWith({ enabled: true, retention_days: 0 })
    expect(wrapper.find('select').exists()).toBe(false)
    expect(wrapper.text()).toContain('admin.usage.conversation.enabledStatus')
  })
})
