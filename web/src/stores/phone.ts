import { defineStore } from 'pinia'
import { phoneService, type PhoneCall, type PhoneDevice, type PhoneEvent, type PhoneRecord } from '../services/phone'
import { PhoneEventListener } from '../services/phone-event-listener'
import { PhoneMediaController, type PhoneMediaState } from '../services/phone-media'
import {
  readPhoneControl,
  savePhoneControl,
  type SavedPhoneControl
} from '../services/phone-session'
import { normalizeCallOwnership, phoneErrorMessage, shouldRefreshCallMedia } from '../utils/phone'

const ACTIVE_STATUSES = new Set(['calling', 'ringing', 'waiting', 'connected'])
export type PhoneMediaMode = 'none' | 'listen-only' | 'two-way'

type PhoneState = {
  initialized: boolean
  loading: boolean
  devices: PhoneDevice[]
  calls: PhoneCall[]
  endingCallIds: string[]
  history: PhoneRecord[]
  mediaId: string
  lease: string
  mediaState: PhoneMediaState
  mediaMode: PhoneMediaMode
  muted: boolean
  error: string
  eventError: string
  listening: boolean
  lastEventId: number
  now: number
}

let clockTimer: number | null = null
let mediaController: PhoneMediaController | null = null
let eventListener: PhoneEventListener | null = null

export const usePhoneStore = defineStore('phone', {
  state: (): PhoneState => ({
    initialized: false,
    loading: false,
    devices: [],
    calls: [],
    endingCallIds: [],
    history: [],
    mediaId: '',
    lease: '',
    mediaState: 'idle',
    mediaMode: 'none',
    muted: false,
    error: '',
    eventError: '',
    listening: false,
    lastEventId: 0,
    now: Date.now()
  }),
  getters: {
    currentCall(state): PhoneCall | undefined {
      return state.calls.find((call) => call.media_id === state.mediaId)
        || state.calls.find((call) => !call.read_only)
        || state.calls.find((call) => call.status === 'ringing')
        || state.calls[0]
    },
    isCallEnding(state) {
      return (callId: string) => state.endingCallIds.includes(callId)
    },
    canSendDTMF(): boolean {
      const call = this.currentCall
      return !!call && call.status === 'connected' && !call.held && !call.read_only
        && !this.isCallEnding(call.call_id)
    },
    mediaReady(state) {
      return state.mediaState === 'connected'
    },
    secureContext() {
      return typeof window !== 'undefined' && window.location.protocol === 'https:' && window.isSecureContext
    }
  },
  actions: {
    async initialize() {
      if (this.initialized) return
      this.initialized = true
      this.loading = true
      this.restoreControl()
      this.ensureMediaController()
      try {
        const [devices, calls, history] = await Promise.all([
          phoneService.devices(),
          phoneService.active(this.lease),
          phoneService.history()
        ])
        this.devices = devices
        this.calls = calls
        this.history = history
      } catch (error) {
        this.error = phoneErrorMessage(error, '电话服务加载失败')
      } finally {
        this.loading = false
      }
      this.startEventStream()
      this.startClock()
    },

    dispose() {
      this.listening = false
      eventListener?.stop()
      eventListener = null
      if (clockTimer !== null) window.clearInterval(clockTimer)
      clockTimer = null
      mediaController?.close()
      mediaController = null
      this.mediaState = 'idle'
      this.mediaMode = 'none'
      this.endingCallIds = []
      this.initialized = false
    },

    async refresh() {
      const [devices, calls, history] = await Promise.all([
        phoneService.devices(), phoneService.active(this.lease), phoneService.history()
      ])
      this.devices = devices
      this.mergeCalls(calls)
      this.endingCallIds = this.endingCallIds.filter((callId) => calls.some((call) => call.call_id === callId))
      this.history = history
    },

    // 把服务端的 active 快照合并进本地列表，而不是整体覆盖。
    //
    // 背景：refresh() 会在 SSE 断连重连（onOpen）时触发，与 SSE 事件流是两条
    // 并发数据源、无顺序保证。若 HTTP 快照恰好落在 call_answered 之前返回，
    // 整体覆盖会用一个还写着 ringing 的旧状态把前端已更新的 connected 顶回去，
    // 通话卡片就永远停在"响铃中"（媒体与 DB 都正常，所以对方能听到声音、只是
    // UI 不动）。这里按"状态只进不退"合并：本地已处于更靠后状态的通话，不被
    // 远端较旧的状态覆盖。
    mergeCalls(remote: PhoneCall[]) {
      const rank: Record<string, number> = { calling: 0, ringing: 1, waiting: 1, connected: 2 }
      const byId = new Map<string, PhoneCall>()
      for (const call of this.calls) {
        byId.set(call.call_id, normalizeCallOwnership(call, this.mediaId))
      }
      for (const incoming of remote) {
        const normalized = normalizeCallOwnership(incoming, this.mediaId)
        const existing = byId.get(normalized.call_id)
        if (!existing) {
          byId.set(normalized.call_id, normalized)
          continue
        }
        const remoteRank = rank[normalized.status] ?? -1
        const localRank = rank[existing.status] ?? -1
        // 状态只进不退：远端更靠后才覆盖，否则保留本地。
        if (remoteRank >= localRank) {
          byId.set(normalized.call_id, normalized)
        }
      }
      // active 快照里没有的通话 = 服务端已结束（terminal 不返回），删除。
      const remoteIds = new Set(remote.map((call) => call.call_id))
      this.calls = [...byId.values()].filter((call) => remoteIds.has(call.call_id))
    },

    async enableMedia() {
      return this.refreshCurrentMedia('two-way')
    },

    async enableListenOnlyMedia() {
      return this.refreshCurrentMedia('listen-only')
    },

    async refreshCurrentMedia(mode: Exclude<PhoneMediaMode, 'none'>) {
      const current = this.currentCall
      const previous: SavedPhoneControl = { mediaId: this.mediaId, lease: this.lease }
      const prepared = mode === 'two-way'
        ? await this.prepareMedia()
        : await this.prepareReceiveOnlyMedia()
      if (!current || !shouldRefreshCallMedia(current, prepared.mediaId)) return
      try {
        const result = await phoneService.refreshMedia(current.call_id, prepared.mediaId, previous.lease)
        this.lease = result.lease
        this.upsertCall(result.call)
        this.saveControl()
      } catch (error) {
        this.restoreSavedControl(previous)
        throw error
      }
    },

    async takeOver(call: PhoneCall, mode: Exclude<PhoneMediaMode, 'none'> = 'two-way') {
      const prepared = mode === 'listen-only'
        ? await this.prepareReceiveOnlyMedia()
        : await this.prepareMedia()
      try {
        const result = await phoneService.refreshMedia(call.call_id, prepared.mediaId, '', true)
        this.lease = result.lease
        this.upsertCall(result.call)
        this.saveControl()
      } catch (error) {
        this.releaseMedia()
        throw error
      }
    },

    async startCall(deviceId: string, callee: string) {
      return this.startCallWithMode(deviceId, callee, 'two-way')
    },

    async startListenOnlyCall(deviceId: string, callee: string) {
      return this.startCallWithMode(deviceId, callee, 'listen-only')
    },

    async startCallWithMode(
      deviceId: string,
      callee: string,
      mode: Exclude<PhoneMediaMode, 'none'>
    ) {
      this.error = ''
      if (this.calls.some((call) => ACTIVE_STATUSES.has(call.status) && !call.read_only)) {
        throw new Error('当前浏览器已有一通活动电话')
      }
      const media = mode === 'two-way'
        ? await this.prepareMedia()
        : await this.prepareReceiveOnlyMedia()
      try {
        const call = await phoneService.startCall(deviceId, callee, media.mediaId, media.lease)
        this.upsertCall(call)
        return call
      } catch (error) {
        this.releaseMedia()
        throw error
      }
    },

    async answer(call: PhoneCall) {
      const media = await this.prepareMedia()
      try {
        const answered = await phoneService.answer(call.call_id, media.mediaId, media.lease)
        this.upsertCall(answered)
      } catch (error) {
        this.releaseMedia()
        throw error
      }
    },

    async answerListenOnly(call: PhoneCall) {
      const media = await this.prepareReceiveOnlyMedia()
      try {
        const answered = await phoneService.answer(call.call_id, media.mediaId, media.lease)
        this.upsertCall(answered)
      } catch (error) {
        this.releaseMedia()
        throw error
      }
    },

    async reject(call: PhoneCall) {
      const media = await this.prepareControlMedia()
      await phoneService.reject(call.call_id, media.mediaId, media.lease)
      this.releaseMedia()
    },

    async hangup(call?: PhoneCall) {
      const target = call || this.currentCall
      if (!target || this.isCallEnding(target.call_id)) return
      this.endingCallIds = [...this.endingCallIds, target.call_id]
      try {
        await phoneService.hangup(target.call_id, this.lease)
        this.calls = this.calls.filter((item) => item.call_id !== target.call_id)
        this.clearEndingCall(target.call_id)
        if (target.media_id === this.mediaId) this.releaseMedia()
        void this.reloadHistory()
      } catch (error) {
        this.clearEndingCall(target.call_id)
        throw error
      }
    },

    async sendDTMF(digit: string) {
      const call = this.currentCall
      if (!call) throw new Error('当前没有活动电话')
      if (!this.canSendDTMF) throw new Error('当前通话不可发送按键，请确认通话已接通且由本页面控制')
      await phoneService.dtmf(call.call_id, digit, this.lease)
    },

    async toggleHold() {
      const call = this.currentCall
      if (!call || call.status !== 'connected' || call.read_only) return
      if (call.held) {
        await phoneService.resume(call.call_id, this.lease)
        this.upsertCall({ ...call, held: false })
        return
      }
      await phoneService.hold(call.call_id, this.lease)
      this.upsertCall({ ...call, held: true })
    },

    toggleMute() {
      if (this.mediaMode !== 'two-way') return
      this.muted = !this.muted
      mediaController?.setMuted(this.muted)
    },

    clearError() {
      this.error = ''
    },

    clearEndingCall(callId: string) {
      this.endingCallIds = this.endingCallIds.filter((value) => value !== callId)
    },

    async recordingURL(name: string) {
      const blob = await phoneService.recording(name)
      return URL.createObjectURL(blob)
    },

    ensureMediaController() {
      if (mediaController) return
      mediaController = new PhoneMediaController({
        onState: (state) => { this.mediaState = state },
        onError: (message) => { this.error = message }
      })
    },

    hasReusableMedia(mode: Exclude<PhoneMediaMode, 'none'>) {
      return this.mediaReady && !!this.mediaId && !!this.lease && this.mediaMode === mode
    },

    async prepareMedia() {
      if (this.hasReusableMedia('two-way')) {
        return { mediaId: this.mediaId, lease: this.lease }
      }
      this.ensureMediaController()
      this.error = ''
      try {
        const prepared = await mediaController!.prepare()
        this.mediaId = prepared.mediaId
        this.lease = prepared.lease
        this.mediaMode = 'two-way'
        this.muted = false
        this.saveControl()
        return prepared
      } catch (error) {
        this.error = phoneErrorMessage(error, '听筒和麦克风启用失败')
        throw error
      }
    },

    async prepareControlMedia() {
      return this.prepareReceiveOnlyMedia()
    },

    async prepareReceiveOnlyMedia() {
      if (this.hasReusableMedia('listen-only')) {
        return { mediaId: this.mediaId, lease: this.lease }
      }
      this.ensureMediaController()
      const prepared = await mediaController!.prepare({ microphone: false })
      this.mediaId = prepared.mediaId
      this.lease = prepared.lease
      this.mediaMode = 'listen-only'
      this.saveControl()
      return prepared
    },

    upsertCall(call: PhoneCall) {
      const normalized = normalizeCallOwnership(call, this.mediaId)
      const index = this.calls.findIndex((item) => item.call_id === call.call_id)
      if (index >= 0) this.calls = this.calls.map((item, position) => position === index ? normalized : item)
      else this.calls = [normalized, ...this.calls]
    },

    handleEvent(event: PhoneEvent) {
      if (event.id === this.lastEventId) return
      this.lastEventId = event.id
      const ended = event.type === 'call_ended' || !ACTIVE_STATUSES.has(event.call.status)
      if (ended) this.calls = this.calls.filter((call) => call.call_id !== event.call.call_id)
      else this.upsertCall(event.call)
      if (event.type === 'call_ended') {
        this.clearEndingCall(event.call.call_id)
        if (event.call.media_id === this.mediaId) this.releaseMedia()
        void this.reloadHistory()
      } else if (event.type === 'recording_ready' || event.type === 'recording_failed') {
        void this.reloadHistory()
      } else if (event.type === 'media_disconnected' && event.call.media_id === this.mediaId) {
        this.error = '浏览器媒体连接已断开，请在 15 秒内恢复听筒，否则电话将自动挂断'
      }
    },

    startEventStream() {
      if (this.listening) return
      this.listening = true
      eventListener = new PhoneEventListener({
        cursor: () => this.lastEventId,
        onEvent: (event) => this.handleEvent(event),
        onError: (message) => {
          this.eventError = message
          void this.refresh().catch((error) => {
            this.eventError = phoneErrorMessage(error, message)
          })
        },
        onOpen: async () => {
          this.eventError = ''
          try {
            await this.refresh()
          } catch (error) {
            this.eventError = phoneErrorMessage(error, '电话状态同步失败')
          }
        }
      })
      eventListener.start()
    },

    async reloadHistory() {
      try {
        this.history = await phoneService.history()
      } catch (error) {
        this.error = phoneErrorMessage(error, '最近通话加载失败')
      }
    },

    startClock() {
      if (clockTimer !== null) return
      clockTimer = window.setInterval(() => { this.now = Date.now() }, 1_000)
    },

    restoreControl() {
      const saved = readPhoneControl()
      this.mediaId = saved.mediaId
      this.lease = saved.lease
    },

    saveControl() {
      savePhoneControl({ mediaId: this.mediaId, lease: this.lease })
    },

    restoreSavedControl(control: SavedPhoneControl) {
      mediaController?.close()
      mediaController = null
      this.mediaId = control.mediaId
      this.lease = control.lease
      this.mediaState = 'idle'
      this.mediaMode = 'none'
      this.muted = false
      this.saveControl()
    },

    releaseMedia() {
      mediaController?.close()
      mediaController = null
      this.mediaId = ''
      this.lease = ''
      this.calls = this.calls.map((call) => normalizeCallOwnership(call, ''))
      this.mediaState = 'idle'
      this.mediaMode = 'none'
      this.muted = false
      savePhoneControl({ mediaId: '', lease: '' })
    }
  }
})
