import { apiClient } from '../client'

export type CodexTicketPlan = 'pro' | 'team'

// 账号可勾选的目标模型；每个模型独立持票。
export const CODEX_TICKET_MODELS = ['gpt-6-astra', 'gpt-5.6-sol'] as const
export type CodexTicketModel = (typeof CODEX_TICKET_MODELS)[number]

export type CodexAccountTicketState = 'disabled' | 'global_disabled' | 'waiting' | 'harvesting' | 'ready' | 'error'

export interface CodexAccountTicketWatchdog {
  enabled: boolean
  trigger_count: number
  last_reason?: 'model_mismatch' | 'state_312'
  last_triggered_at?: string
}

export interface CodexAccountTicketModelStatus {
  model: string
  state: CodexAccountTicketState
  ticket_usable: boolean
  refreshing?: boolean
  captured_at?: string
  expires_at?: string
  retry_after?: string
  remaining_seconds: number
  last_error: string
  attempts: number
}

export interface CodexAccountTicketStatus {
  enabled: boolean
  global_enabled: boolean
  model: string
  // Optional so older API responses and test fixtures remain valid; fall back to [model].
  models?: string[]
  model_statuses?: CodexAccountTicketModelStatus[]
  ticket_plan: CodexTicketPlan
  target_length: number
  proxy_configured: boolean
  proxy_display: string
  fixed_proxy_configured: boolean
  direct_route?: boolean
  state: CodexAccountTicketState
  remaining_seconds: number
  // These fields are optional so older API responses and test fixtures remain valid.
  ticket_usable?: boolean
  captured_at?: string
  expires_at?: string
  refreshing?: boolean
  retry_after?: string
  last_error: string
  attempts: number
  watchdog: CodexAccountTicketWatchdog
}

export interface CodexAccountTicketSettings {
  enabled: boolean
  model?: string
  models?: string[]
  ticket_plan?: CodexTicketPlan
}

export async function getCodexAccountTicket(accountId: number): Promise<CodexAccountTicketStatus> {
  const { data } = await apiClient.get<CodexAccountTicketStatus>(`/admin/accounts/${accountId}/codex-ticket`)
  return data
}

export async function saveCodexAccountTicket(accountId: number, settings: CodexAccountTicketSettings): Promise<CodexAccountTicketStatus> {
  const { data } = await apiClient.put<CodexAccountTicketStatus>(`/admin/accounts/${accountId}/codex-ticket`, settings)
  return data
}

export async function harvestCodexAccountTicket(accountId: number): Promise<CodexAccountTicketStatus> {
  const { data } = await apiClient.post<CodexAccountTicketStatus>(`/admin/accounts/${accountId}/codex-ticket/harvest`)
  return data
}
