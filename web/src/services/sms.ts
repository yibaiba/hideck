import { api } from '../stores/auth'
import { callService } from './http'
import type { ServiceResult } from '../types/domain'
import type { DeviceMgmtListItem } from '../types/api'
import type { SMSContactDTO, SMSMessageDTO, SmsThreadVM } from '../types/view-model'
import { normalizeSmsUnreadCount } from '../utils/smsPresentation'

export type SmsThreadQueryParams = {
  peer: string
  limit: number
  iccid?: string
  device_id?: string
  imsi?: string
  before_ts?: string
  before_id?: number
}

export type SmsSendPayload = {
  device_id?: string
  imsi?: string
  iccid?: string
  phone: string
  message: string
}

export type SmsDeleteThreadPayload = {
  device_id?: string
  imsi?: string
  iccid?: string
  peer: string
}

export type SmsMarkThreadReadPayload = {
  iccid: string
  peer: string
} & ({ through_id: number; message_ids?: never } | { message_ids: readonly number[]; through_id?: never })

export type SmsMarkThreadReadResult = {
  marked: number
  unread_count: number
  through_id?: number
  message_ids?: number[]
}

function parseTs(s: string) {
  const ms = new Date(s).getTime()
  return Number.isFinite(ms) ? ms : 0
}

export function normalizeThread(contact: SMSContactDTO): SmsThreadVM {
  const iccid = String(contact.iccid || '')
  return {
    key: `${iccid || contact.imsi}|${contact.peer}`,
    imsi: contact.imsi,
    iccid,
    peer: contact.peer,
    deviceId: contact.device_id,
    lastTs: parseTs(contact.last_timestamp),
    lastSmsId: contact.last_sms_id || 0,
    lastMessage: String(contact.last_content || '').slice(0, 80),
    lastDeviceName: contact.device_name,
    localPhone: contact.local_phone || '',
    unreadCount: normalizeSmsUnreadCount(contact.unread_count),
    peerLower: String(contact.peer || '').toLowerCase(),
    lastMessageLower: String(contact.last_content || '').toLowerCase()
  }
}

export function resolveSmsThreadKey(requestedKey: string, threads: readonly SmsThreadVM[]): string {
  const requested = String(requestedKey || '').trim()
  if (!requested) return ''
  if (threads.some(thread => thread.key === requested)) return requested

  const separator = requested.indexOf('|')
  if (separator <= 0) return ''
  const imsi = requested.slice(0, separator)
  const peer = requested.slice(separator + 1)
  const matches = threads.filter(thread => thread.imsi === imsi && thread.peer === peer)
  return matches.length === 1 ? matches[0].key : ''
}

const inflightRequests = new Map<string, Promise<ServiceResult<unknown>>>()

function reuseInflight<T>(key: string, task: () => Promise<T>): Promise<ServiceResult<T>> {
  const existing = inflightRequests.get(key)
  if (existing) return existing as Promise<ServiceResult<T>>

  const promise = callService(task).finally(() => {
    if (inflightRequests.get(key) === promise) inflightRequests.delete(key)
  })
  inflightRequests.set(key, promise as Promise<ServiceResult<unknown>>)
  return promise
}

export const smsService = {
  listDevices() {
    return reuseInflight('sms:listDevices', async () => {
      const res = await api.get('/devices')
      const list = (res.data?.devices || []) as DeviceMgmtListItem[]
      // SMS 已是系统不变量（恒开），不再按 sms_enabled 过滤；仅展示运行中设备。
      return list.filter(d => d.running)
    })
  },
  listContacts(deviceId?: string) {
    const key = `sms:listContacts:${deviceId && deviceId !== 'all' ? deviceId : 'all'}`
    return reuseInflight(key, async () => {
      const params: Record<string, string> = { limit: '200' }
      if (deviceId && deviceId !== 'all') params.device_id = deviceId
      const res = await api.get('/sms/contacts', { params })
      const list = (res.data || []) as SMSContactDTO[]
      return list.map(normalizeThread).sort((a, b) => b.lastTs - a.lastTs)
    })
  },
  getThread(params: SmsThreadQueryParams) {
    const key = `sms:getThread:${params.iccid || ''}:${params.device_id || ''}:${params.imsi || ''}:${params.peer}:${params.limit}:${params.before_ts || ''}:${params.before_id || 0}`
    return reuseInflight(key, async () => {
      const res = await api.get('/sms/thread', { params })
      const list = (res.data || []) as SMSMessageDTO[]
      return list.slice().sort((a, b) => parseTs(a.timestamp) - parseTs(b.timestamp) || a.id - b.id)
    })
  },
  send(payload: SmsSendPayload) {
    return callService(async () => {
      const res = await api.post<{ parts_total?: number; destination: string; delivery_state?: string; message?: string }>('/sms/send', payload)
      const { delivery_state, message } = res.data
      const uncertain = res.status === 202 && (delivery_state === 'unknown' || delivery_state === 'partial_ack')
      return {
        partsTotal: Number(res.data?.parts_total || 0),
        destination: res.data.destination,
        submissionWarning: uncertain ? (message || '短信提交未完全确认，请勿重发整条') : ''
      }
    })
  },
  deleteMessage(id: number) {
    return callService(async () => {
      const res = await api.delete('/sms/messages/' + id)
      return res.data as { thread_empty: boolean; imsi: string; peer: string }
    })
  },
  markThreadRead(payload: SmsMarkThreadReadPayload) {
    return callService(async () => {
      const { iccid, peer, ...body } = payload
      const params = { iccid, peer }
      const res = await api.patch<SmsMarkThreadReadResult>('/sms/thread', body, { params })
      return res.data
    })
  },
  deleteThread(payload: SmsDeleteThreadPayload) {
    return callService(async () => {
      const params: Record<string, string> = { peer: payload.peer }
      if (payload.device_id) params.device_id = payload.device_id
      if (payload.imsi) params.imsi = payload.imsi
      if (payload.iccid) params.iccid = payload.iccid
      const res = await api.delete('/sms/thread', { params })
      return res.data as { deleted: number; iccid: string; peer: string }
    })
  }
}
