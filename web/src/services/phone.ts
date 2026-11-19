import { api } from '../stores/auth'
import { readPhoneEvents } from './phone-events'

export type PhoneDevice = {
  id: string
  name: string
  iccid: string
  voice: { ready?: boolean; registered?: boolean; active_call?: boolean; phase?: string; last_error?: string }
  phone_mode?: string      // "wifi" | "cellular" | "volte"
  native_volte?: {
    phase?: string
    ims_registered?: boolean
    voice_indications_ready?: boolean
    ims_enabled?: boolean
    volte_enabled?: boolean
    voice_available?: boolean
    reboot_required?: boolean
    last_error?: string
    uac_enabled?: boolean
    uac_unusable?: boolean
    provision_stage?: string
    plmn?: string
    mbn_name?: string
    lte_registered?: boolean
    ims_pdn_active?: boolean
  }
  data_strategy?: string   // "always" | "on_demand"
  network_enabled?: boolean
  vowifi_enabled?: boolean
  vowifi_active?: boolean
  software_ims_blocked?: boolean
  native_voice_unavailable_reason?: string
  cellular_ims_unavailable_reason?: string
  phone_region?: string
  phone_country_code?: number
  rf_lock?: string
  lebara_identity_status?: string
  lebara_identity_message?: string
  lebara_identity_iccid?: string
}

export type PhoneCall = {
  call_id: string
  device_id: string
  direction: 'inbound' | 'outbound'
  peer: string
  status: string
  media_id?: string
  started_at: string
  answered_at?: string
  ended_at?: string
  end_reason?: string
  codec?: string
  recording_error?: string
  read_only: boolean
  held?: boolean
}

export type PhoneRecord = {
  id: number
  call_id: string
  device_id: string
  direction: 'inbound' | 'outbound'
  peer: string
  status: string
  started_at: string
  answered_at?: string
  ended_at?: string
  duration_seconds: number
  end_reason?: string
  codec?: string
  recording_name?: string
  recording_error?: string
}

export type PhoneEvent = {
  id: number
  type: string
  call: PhoneCall
  time: string
}

function leaseHeaders(lease: string) {
  return lease ? { 'X-Phone-Lease': lease } : undefined
}

const MEDIA_RELEASE_TIMEOUT_MS = 5_000

export const phoneService = {
  async devices(signal?: AbortSignal) {
    return (await api.get<{ devices: PhoneDevice[] }>('/phone/devices', { signal })).data.devices
  },

  async createMedia(sdp: string) {
    return (await api.post<{ media_id: string; lease: string; sdp: string }>('/phone/media', { sdp })).data
  },

  async releaseMedia(mediaId: string, lease: string) {
    await api.delete(`/phone/media/${encodeURIComponent(mediaId)}`, {
      headers: leaseHeaders(lease),
      timeout: MEDIA_RELEASE_TIMEOUT_MS
    })
  },

  async active(lease: string) {
    return (await api.get<{ calls: PhoneCall[] }>('/phone/calls/active', { headers: leaseHeaders(lease) })).data.calls
  },

  async history(limit = 50) {
    return (await api.get<{ records: PhoneRecord[] }>('/phone/history', { params: { limit } })).data.records
  },

  async deleteHistory(ids: number[]) {
    return (await api.post<{ deleted: number }>('/phone/history/delete', { ids })).data.deleted
  },

  async startCall(deviceId: string, callee: string, mediaId: string, lease: string) {
    return (await api.post<PhoneCall>('/phone/calls', {
      device_id: deviceId,
      callee,
      media_id: mediaId
    }, { headers: leaseHeaders(lease) })).data
  },

  async answer(callId: string, mediaId: string, lease: string) {
    return (await api.post<PhoneCall>(`/phone/calls/${encodeURIComponent(callId)}/answer`, {
      media_id: mediaId
    }, { headers: leaseHeaders(lease) })).data
  },

  async reject(callId: string, mediaId: string, lease: string) {
    await api.post(`/phone/calls/${encodeURIComponent(callId)}/reject`, {
      media_id: mediaId
    }, { headers: leaseHeaders(lease) })
  },

  async hangup(callId: string, lease: string) {
    await api.delete(`/phone/calls/${encodeURIComponent(callId)}`, { headers: leaseHeaders(lease) })
  },

  async dtmf(callId: string, digit: string, lease: string) {
    await api.post(`/phone/calls/${encodeURIComponent(callId)}/dtmf`, { digit }, { headers: leaseHeaders(lease) })
  },

  async hold(callId: string, lease: string) {
    await api.post(`/phone/calls/${encodeURIComponent(callId)}/hold`, {}, { headers: leaseHeaders(lease) })
  },

  async resume(callId: string, lease: string) {
    await api.post(`/phone/calls/${encodeURIComponent(callId)}/resume`, {}, { headers: leaseHeaders(lease) })
  },

  async refreshMedia(callId: string, mediaId: string, lease: string, takeover = false) {
    return (await api.put<{ call: PhoneCall; lease: string }>(
      `/phone/calls/${encodeURIComponent(callId)}/media`,
      { media_id: mediaId, takeover },
      { headers: leaseHeaders(lease) }
    )).data
  },

  async recording(name: string) {
    return (await api.get<Blob>(`/phone/recordings/${encodeURIComponent(name)}`, { responseType: 'blob' })).data
  },

  async events(
    afterId: number,
    signal: AbortSignal,
    onEvent: (event: PhoneEvent) => void,
    onOpen?: () => void | Promise<void>
  ) {
    const token = localStorage.getItem('token') || ''
    const response = await fetch(`/api/phone/events?after_id=${afterId}`, {
      headers: {
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
        'Last-Event-ID': String(afterId)
      },
      signal
    })
    if (!response.ok || !response.body) {
      throw new Error(`电话事件流连接失败（HTTP ${response.status}）`)
    }
    await onOpen?.()
    await readPhoneEvents(response.body, onEvent)
  }
}
