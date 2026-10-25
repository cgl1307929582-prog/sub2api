import { apiClient } from '../client'

export interface ConversationConfig {
  enabled: boolean
  retention_days: number
  max_text_bytes?: number
  stats?: { dropped: number; write_failures: number }
}

export interface ConversationDetail {
  id: number
  created_at: string
  request_id: string
  user_id: number
  api_key_id: number
  model: string
  endpoint: string
  status_code: number
  stream: boolean
  user_text: string
  assistant_text: string
  error_text: string
  input_truncated: boolean
  output_truncated: boolean
  response_complete: boolean
}

export async function getConversationConfig(): Promise<ConversationConfig> {
  return (await apiClient.get<ConversationConfig>('/admin/usage/conversation-config')).data
}

export async function updateConversationConfig(config: ConversationConfig): Promise<ConversationConfig> {
  return (await apiClient.put<ConversationConfig>('/admin/usage/conversation-config', {
    enabled: config.enabled, retention_days: config.retention_days
  })).data
}

export async function getConversation(id: number, kind: 'usage' | 'error' = 'usage', signal?: AbortSignal): Promise<ConversationDetail> {
  const path = kind === 'error' ? `/admin/usage/conversation-errors/${id}` : `/admin/usage/${id}/conversation`
  return (await apiClient.get<ConversationDetail>(path, { signal })).data
}
