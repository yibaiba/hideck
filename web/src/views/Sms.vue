<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Loading } from '@element-plus/icons-vue'
import { useSMSStore } from '../stores/sms'
import { usePollingScheduler } from '../composables/usePollingScheduler'
import { toAppError } from '../services/http'
import { resolveSmsThreadKey, type SmsThreadQueryParams } from '../services/sms'
import { fetchSmsMessageTarget, type SmsMessageTarget } from '../services/sms-message-target'
import { includeSmsTargetThread } from '../utils/smsMessageTarget'
import EmptyState from '../components/EmptyState.vue'
import ErrorState from '../components/ErrorState.vue'
import SmsDeviceRail from '../components/sms/SmsDeviceRail.vue'
import SmsThreadListPane from '../components/sms/SmsThreadListPane.vue'
import SmsConversationHeader from '../components/sms/SmsConversationHeader.vue'
import SmsMessageTimeline from '../components/sms/SmsMessageTimeline.vue'
import SmsComposer from '../components/sms/SmsComposer.vue'
import PhoneContactsDrawer from '../components/PhoneContactsDrawer.vue'
import type { DeviceMgmtListItem, SMSMessage } from '../types/api'
import { Delete24Regular, Person24Regular, Send24Regular } from '@vicons/fluent'
import 'vue-virtual-scroller/dist/vue-virtual-scroller.css'
import { formatDeviceDate } from '../utils/deviceTime'
import { createSmsConversationContext, createSmsDeviceChannels } from '../utils/smsPresentation'
import { isNearTimelineEnd } from '../utils/timelineFollow'
import {
  SMS_THREAD_PAGE_SIZE,
  mergeSmsThreadPages,
  threadAlreadyHasLatest
} from '../utils/smsThreadMessages'

type SmsThread = {
  key: string
  imsi: string
  iccid: string
  peer: string
  deviceId?: string
  lastTs: number
  lastSmsId: number
  lastMessage: string
  lastDeviceName?: string
  localPhone?: string
  unreadCount: number
  peerLower: string
  lastMessageLower: string
}

const route = useRoute()
const router = useRouter()
const smsStore = useSMSStore()

const devices = ref<DeviceMgmtListItem[]>([])
const devicesLastOkAt = ref<number | null>(null)
const devicesError = ref<{ message: string; status?: number; method?: string; url?: string } | null>(null)

const loading = ref(false)
const threads = ref<SmsThread[]>([])
const messagesLastOkAt = ref<number | null>(null)
const messagesError = ref<{ message: string; status?: number; method?: string; url?: string } | null>(null)

const selectedDevice = ref<string>(typeof route.query.device === 'string' ? route.query.device : 'all')
const selectedThreadKey = ref<string>(typeof route.query.contact === 'string' ? route.query.contact : '')
const targetMessageQuery = ref<unknown>(route.query.message)
const messageTarget = ref<SmsMessageTarget | null>(null)
const viewingTarget = computed(() => !!messageTarget.value && route.query.latest !== '1')
const searchQuery = ref('')

const smsPageRef = ref<HTMLElement | null>(null)
const smsPageWidth = ref(0)
const SMS_NARROW_BREAKPOINT = 980
let smsPageResizeObserver: ResizeObserver | null = null

function syncSmsPageWidth() {
  smsPageWidth.value = smsPageRef.value?.clientWidth || 0
}

function dateKey(timestamp: string) {
  return formatDeviceDate(timestamp, { fallback: '未知日期' })
}

const filteredThreads = computed(() => {
  const q = String(searchQuery.value || '').trim().toLowerCase()
  if (!q) return threads.value
  return threads.value.filter(t => {
    if (t.peerLower.includes(q)) return true
    return t.lastMessageLower.includes(q)
  })
})

const presentedThreads = computed(() => {
  return filteredThreads.value
})

const selectedThread = computed(() => {
  return threads.value.find(t => t.key === selectedThreadKey.value) || null
})

const loadingHistoryMore = ref(false)
const threadLoading = ref(false)
const threadMessages = ref<SMSMessage[]>([])
const threadHasMore = ref(false)

const canLoadMoreHistory = computed(() => {
  return !!selectedThread.value && threadHasMore.value
})

const selectedThreadGroups = computed(() => {
  if (!selectedThread.value) return []
  const out: Array<{ date: string; items: SMSMessage[] }> = []
  let last = ''
  for (const m of threadMessages.value) {
    const key = dateKey(m.timestamp)
    if (!out.length || key !== last) {
      out.push({ date: key, items: [m] })
      last = key
    } else {
      out[out.length - 1].items.push(m)
    }
  }
  return out
})

const isNarrowLayout = computed(() => smsPageWidth.value > 0 && smsPageWidth.value < SMS_NARROW_BREAKPOINT)

const showSendModal = ref(false)
const showContactsPicker = ref(false)
const sending = ref(false)
const deletingMessageId = ref<number | null>(null)
const deletingThreadKey = ref<string | null>(null)
const showActionSheet = ref(false)
const actionSheetTarget = ref<{ type: 'thread'; thread: SmsThread } | { type: 'message'; message: SMSMessage } | null>(null)
const composer = ref('')
const GSM7_BASIC_CHARS = new Set(Array.from(`@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞ !"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà`))
const GSM7_EXT_CHARS = new Set(Array.from(`^{}\\[~]|€`))

function estimateSegments(text: string) {
  const raw = String(text || '')
  let gsm7Units = 0
  let isGSM7 = true
  for (const ch of Array.from(raw)) {
    if (GSM7_BASIC_CHARS.has(ch)) {
      gsm7Units += 1
      continue
    }
    if (GSM7_EXT_CHARS.has(ch)) {
      gsm7Units += 2
      continue
    }
    isGSM7 = false
    break
  }
  if (isGSM7) {
    const single = 160
    const multi = 153
    const parts = gsm7Units <= single ? 1 : Math.ceil(gsm7Units / multi)
    return { encoding: 'GSM7', parts, units: gsm7Units, unitName: 'septets' }
  }
  const ucs2Units = Array.from(raw).length
  const single = 70
  const multi = 67
  const parts = ucs2Units <= single ? 1 : Math.ceil(ucs2Units / multi)
  return { encoding: 'UCS2', parts, units: ucs2Units, unitName: 'chars' }
}

const composerLen = computed(() => Array.from(String(composer.value || '')).length)
const composerEstimate = computed(() => estimateSegments(String(composer.value || '')))
const detailScrollbar = ref<HTMLElement | null>(null)
const followingLatest = ref(true)
let followLatestTimer = 0

const sendForm = ref({
  device_id: '',
  phone: '',
  message: ''
})
const sendEstimate = computed(() => estimateSegments(String(sendForm.value.message || '')))

const selectedSendDeviceId = ref('')
const sendDeviceOptions = computed(() => devices.value.map(d => ({ label: `${d.name || d.id}`, value: d.id })))

const deviceSidebarItems = computed(() => createSmsDeviceChannels(devices.value))
const conversationContext = computed(() => createSmsConversationContext({
  selectedDeviceId: selectedDevice.value,
  thread: selectedThread.value,
  devices: devices.value
}))

function normalizeQueryDevice(device: string) {
  const v = String(device || '').trim()
  if (!v || v === 'all') return undefined
  return v
}

function buildSmsQuery(device: string, contact?: string) {
  const nextContact = String(contact || '').trim()
  return {
    ...route.query,
    message: undefined,
    latest: undefined,
    device: normalizeQueryDevice(device),
    contact: nextContact || undefined
  }
}

async function markThreadSeen(t: SmsThread | null) {
  if (!t || threadMessages.value.length === 0) return true
  const hasDisplayedUnread = threadMessages.value.some(message => message.type === 1 && message.status === 0)
  if (t.unreadCount <= 0 && !hasDisplayedUnread) return true
  const messageIDs = threadMessages.value.map(message => message.id)
  const scope = viewingTarget.value
    ? { message_ids: messageIDs }
    : { through_id: Math.max(...messageIDs) }
  if (!t.iccid) {
    messagesError.value = { message: '短信会话缺少 ICCID，无法保存已读状态' }
    return false
  }
  const result = await smsStore.markThreadRead({ iccid: t.iccid, peer: t.peer, ...scope })
  if (!result.ok) {
    messagesError.value = result.error
    return false
  }
  threads.value = threads.value.map(thread => (
    thread.key === t.key ? { ...thread, unreadCount: result.data.unread_count } : thread
  ))
  return true
}

function scrollThreadToBottom() {
  followingLatest.value = true
  const snap = () => {
    const wrap = detailScrollbar.value
    if (!wrap) return
    wrap.scrollTop = wrap.scrollHeight
  }
  nextTick(() => {
    snap()
    requestAnimationFrame(() => {
      snap()
      requestAnimationFrame(snap)
    })
    window.clearTimeout(followLatestTimer)
    followLatestTimer = window.setTimeout(snap, 80)
  })
}

function getDetailWrap() {
  return detailScrollbar.value
}

async function loadMoreHistory() {
  if (!canLoadMoreHistory.value) return
  if (loadingHistoryMore.value) return
  if (!selectedThread.value) return
  if (threadMessages.value.length === 0) return
  const wrap = getDetailWrap()
  const prevTop = wrap?.scrollTop || 0
  const prevHeight = wrap?.scrollHeight || 0
  const requestThreadKey = selectedThreadKey.value
  loadingHistoryMore.value = true
  try {
    const oldest = threadMessages.value[0]
    const params: SmsThreadQueryParams = {
      peer: selectedThread.value.peer,
      limit: SMS_THREAD_PAGE_SIZE,
      before_ts: oldest.timestamp,
      before_id: oldest.id
    }
    if (selectedThread.value.iccid) {
      params.iccid = selectedThread.value.iccid
    } else if (selectedDevice.value && selectedDevice.value !== 'all') {
      params.device_id = selectedDevice.value
    } else {
      params.device_id = 'all'
      params.imsi = selectedThread.value.imsi
    }
    const result = await smsStore.fetchThread(params)
    if (selectedThreadKey.value !== requestThreadKey) return
    if (!result.ok) {
      messagesError.value = result.error
      return
    }
    const list = (result.data || []) as SMSMessage[]
    threadMessages.value = mergeSmsThreadPages(list, threadMessages.value)
    threadHasMore.value = list.length === params.limit
    await nextTick()
    requestAnimationFrame(() => {
      const w = getDetailWrap()
      if (!w) return
      const nextHeight = w.scrollHeight
      const delta = Math.max(0, nextHeight - prevHeight)
      w.scrollTop = prevTop + delta
    })
    messagesError.value = null
    if (viewingTarget.value) await markThreadSeen(selectedThread.value)
  } catch (error: unknown) {
    if (selectedThreadKey.value !== requestThreadKey) return
    messagesError.value = toAppError(error)
  } finally {
    loadingHistoryMore.value = false
  }
}

function onDetailScroll(e: Event) {
  const target = e.target as HTMLElement
  if (!target) return
  followingLatest.value = isNearTimelineEnd({
    scrollTop: target.scrollTop,
    clientHeight: target.clientHeight,
    scrollHeight: target.scrollHeight
  }, 160)
  if (target.scrollTop <= 80) {
    loadMoreHistory()
  }
}

let devicesFetchSeq = 0
let messagesFetchSeq = 0
let threadFetchSeq = 0
function showThreadActionSheet(thread: SmsThread) {
  actionSheetTarget.value = { type: 'thread', thread }
  showActionSheet.value = true
}

function showMessageActionSheet(message: SMSMessage) {
  actionSheetTarget.value = { type: 'message', message }
  showActionSheet.value = true
}

function closeActionSheet() {
  showActionSheet.value = false
  actionSheetTarget.value = null
}

async function onActionSheetDelete() {
  const target = actionSheetTarget.value
  closeActionSheet()
  if (!target) return
  if (target.type === 'thread') {
    await confirmDeleteThread(target.thread)
    return
  }
  await confirmDeleteMessage(target.message)
}

function clearSelectedThread(syncRoute = false) {
  targetMessageQuery.value = undefined
  messageTarget.value = null
  messagesFetchSeq += 1
  threadFetchSeq += 1
  selectedThreadKey.value = ''
  threadMessages.value = []
  threadHasMore.value = false
  threadLoading.value = false
  if (syncRoute) {
    void router.replace({ query: buildSmsQuery(selectedDevice.value) })
  }
}

async function fetchDevices() {
  const seq = ++devicesFetchSeq
  devicesError.value = null
  const result = await smsStore.fetchDevices()
  if (seq !== devicesFetchSeq) return false
  if (result.ok) {
    devices.value = (result.data || []) as DeviceMgmtListItem[]
    devicesLastOkAt.value = Date.now()
    if (selectedDevice.value !== 'all' && !devices.value.some(d => d.id === selectedDevice.value)) {
      selectedDevice.value = 'all'
      clearSelectedThread(false)
      void router.replace({ query: buildSmsQuery('all') })
    }
    return true
  }
  devicesError.value = result.error
  return false
}

async function fetchMessages(silent = false) {
  const seq = ++messagesFetchSeq
  if (!silent) loading.value = true
  messagesError.value = null
  const result = await smsStore.fetchThreads(selectedDevice.value)
  if (seq !== messagesFetchSeq) return false
  if (targetMessageQuery.value !== undefined) {
    const target = await fetchSmsMessageTarget(targetMessageQuery.value)
    if (seq !== messagesFetchSeq) return false
    if (!target.ok) {
      messagesError.value = target.error
      loading.value = false
      return false
    }
    messageTarget.value = target.data
    selectedThreadKey.value = target.data.thread.key
  }
  if (result.ok) {
    threads.value = messageTarget.value ? includeSmsTargetThread(result.data, messageTarget.value.thread) : result.data
    messagesLastOkAt.value = Date.now()
  } else {
    messagesError.value = result.error
  }
  if (!silent) loading.value = false
  return result.ok
}

async function fetchThreadLatest(silent = false) {
  const t = selectedThread.value
  if (!t) {
    threadMessages.value = []
    threadHasMore.value = false
    return false
  }
  if (viewingTarget.value && messageTarget.value?.thread.key === t.key) {
    threadMessages.value = messageTarget.value.messages
    threadHasMore.value = messageTarget.value.hasMore
    return true
  }
  if (silent && threadAlreadyHasLatest(threadMessages.value, t.lastSmsId)) {
    return true
  }
  const seq = ++threadFetchSeq
  if (!silent) threadLoading.value = true
  const params: SmsThreadQueryParams = { peer: t.peer, limit: SMS_THREAD_PAGE_SIZE }
  if (t.iccid) {
    params.iccid = t.iccid
  } else if (selectedDevice.value && selectedDevice.value !== 'all') {
    params.device_id = selectedDevice.value
  } else {
    params.device_id = 'all'
    params.imsi = t.imsi
  }
  const result = await smsStore.fetchThread(params)
  if (seq !== threadFetchSeq) return false
  if (result.ok) {
    const list = (result.data || []) as SMSMessage[]
    if (silent && threadMessages.value.length > 0) {
      threadMessages.value = mergeSmsThreadPages(threadMessages.value, list)
      if (!threadHasMore.value) threadHasMore.value = list.length === params.limit
    } else {
      threadMessages.value = list
      threadHasMore.value = list.length === params.limit
    }
  } else {
    messagesError.value = result.error
  }
  if (!silent) threadLoading.value = false
  return result.ok
}

async function ensureThreadSelection(options: { syncRoute?: boolean; silent?: boolean; scrollToBottom?: boolean } = {}) {
  const syncRoute = options.syncRoute === true
  const silent = options.silent === true
  const scrollToBottom = options.scrollToBottom ?? !viewingTarget.value

  const requestedKey = selectedThreadKey.value
  let current = selectedThread.value
  if (!current && requestedKey) {
    const resolvedKey = resolveSmsThreadKey(requestedKey, threads.value)
    if (resolvedKey) {
      selectedThreadKey.value = resolvedKey
      current = selectedThread.value
      if (resolvedKey !== requestedKey) {
        void router.replace({ query: buildSmsQuery(selectedDevice.value, resolvedKey) })
      }
    }
  }
  if (current) {
    const previousCount = threadMessages.value.length
    const previousLastId = threadMessages.value.at(-1)?.id
    const ok = await fetchThreadLatest(silent)
    if (ok) {
      await applyThreadSeen(current)
      const changed = threadMessages.value.length !== previousCount || threadMessages.value.at(-1)?.id !== previousLastId
      if (!viewingTarget.value && (scrollToBottom || (silent && followingLatest.value && changed))) {
        scrollThreadToBottom()
      }
    }
    return
  }

  threadMessages.value = []
  threadHasMore.value = false
  if (requestedKey) {
    selectedThreadKey.value = ''
    void router.replace({ query: buildSmsQuery(selectedDevice.value) })
    return
  }
  if (isNarrowLayout.value || filteredThreads.value.length === 0) return
  await selectThread(filteredThreads.value[0].key, { syncRoute, silent, scrollToBottom })
}

async function selectThread(key: string, options: { syncRoute?: boolean; silent?: boolean; scrollToBottom?: boolean } = {}) {
  if (!key) return
  if (selectedThreadKey.value === key && threadMessages.value.length > 0) return
  targetMessageQuery.value = undefined
  messageTarget.value = null
  messagesFetchSeq += 1

  const syncRoute = options.syncRoute !== false
  const silent = options.silent === true
  const scrollToBottom = options.scrollToBottom !== false
  followingLatest.value = scrollToBottom

  selectedThreadKey.value = key
  threadFetchSeq += 1
  threadMessages.value = []
  threadHasMore.value = false
  threadLoading.value = false
  messagesError.value = null
  if (syncRoute) {
    void router.replace({ query: buildSmsQuery(selectedDevice.value, key) })
  }

  const t = threads.value.find(x => x.key === key) || null
  if (!t) {
    threadMessages.value = []
    threadHasMore.value = false
    return
  }

  const ok = await fetchThreadLatest(silent)
  if (!ok) return
  await applyThreadSeen(t)
  if (scrollToBottom) scrollThreadToBottom()
}

async function applyThreadSeen(thread: SmsThread) {
  await markThreadSeen(thread)
}

async function handleSelectDevice(deviceId: string, options: { syncRoute?: boolean; silent?: boolean } = {}) {
  const nextDevice = String(deviceId || 'all').trim() || 'all'
  const syncRoute = options.syncRoute !== false
  const silent = options.silent === true

  selectedDevice.value = nextDevice
  clearSelectedThread(false)
  if (syncRoute) {
    void router.replace({ query: buildSmsQuery(nextDevice) })
  }

  const ok = await fetchMessages(silent)
  if (!ok || selectedDevice.value !== nextDevice) return
  await ensureThreadSelection({ syncRoute, silent, scrollToBottom: !viewingTarget.value })
}

async function fetchMessagesAndThread(silent = false) {
  const ok = await fetchMessages(silent)
  if (!ok) return
  await ensureThreadSelection({ syncRoute: false, silent, scrollToBottom: !silent })
}

async function refreshAll() {
  await fetchDevices()
  await fetchMessagesAndThread()
}

async function showLatestMessages() {
  followingLatest.value = true
  if (viewingTarget.value) {
    targetMessageQuery.value = undefined
    messageTarget.value = null
    void router.replace({ query: { ...route.query, latest: '1' } })
    const ok = await fetchThreadLatest(false)
    if (ok) scrollThreadToBottom()
    return
  }
  scrollThreadToBottom()
}

async function pollRefresh() {
  if (loading.value || threadLoading.value || loadingHistoryMore.value) return
  try {
    await fetchMessagesAndThread(true)
  } catch {
    // Ignore polling failures; scheduler keeps retrying.
  }
}

usePollingScheduler(pollRefresh, 5000, {
  maxIntervalMs: 60000,
  backgroundIntervalMs: 15000
})

watch(
  () => isNarrowLayout.value,
  async (isNarrow) => {
    if (isNarrow) return
    if (selectedThreadKey.value) return
    if (filteredThreads.value.length === 0) return
    await selectThread(filteredThreads.value[0].key, { syncRoute: true, scrollToBottom: true })
  }
)

onMounted(async () => {
  syncSmsPageWidth()
  if (typeof ResizeObserver !== 'undefined') {
    smsPageResizeObserver = new ResizeObserver(() => {
      syncSmsPageWidth()
    })
    if (smsPageRef.value) {
      smsPageResizeObserver.observe(smsPageRef.value)
    }
  } else {
    window.addEventListener('resize', syncSmsPageWidth, { passive: true })
  }
  const initialDevice = selectedDevice.value
  let [, messagesOk] = await Promise.all([fetchDevices(), fetchMessages()])
  if (!messagesOk && selectedDevice.value !== initialDevice) {
    messagesOk = await fetchMessages()
  }
  if (messagesOk) await ensureThreadSelection({ syncRoute: false, silent: false, scrollToBottom: !viewingTarget.value })
})

onUnmounted(() => {
  messagesFetchSeq += 1
  devicesFetchSeq += 1
  threadFetchSeq += 1
  smsPageResizeObserver?.disconnect()
  smsPageResizeObserver = null
  window.removeEventListener('resize', syncSmsPageWidth)
  window.clearTimeout(followLatestTimer)
})

function openSendModal() {
  sendForm.value.phone = ''
  sendForm.value.message = ''
  selectedSendDeviceId.value = selectedDevice.value !== 'all' ? selectedDevice.value : (devices.value[0]?.id || '')
  showSendModal.value = true
}

function applyContactNumber(number: string) {
  sendForm.value.phone = number
  showContactsPicker.value = false
}

async function handleSendModal() {
  if (!selectedSendDeviceId.value || !sendForm.value.phone || !sendForm.value.message) {
    ElMessage.warning('请填写完整信息')
    return
  }
  sending.value = true
  try {
    const result = await smsStore.send({
      device_id: selectedSendDeviceId.value,
      phone: sendForm.value.phone,
      message: sendForm.value.message
    })
    if (!result.ok) throw new Error(result.error.message || '发送失败')
    if (result.data.submissionWarning) {
      ElMessage.warning(result.data.submissionWarning)
    } else {
      const parts = result.data.partsTotal
      ElMessage.success(`短信已提交${parts > 1 ? `（${parts}段）` : ''}`)
    }
    showSendModal.value = false
    setTimeout(async () => {
      await fetchMessagesAndThread()
    }, 800)
  } catch (e: unknown) {
    const err = toAppError(e)
    ElMessage.error('发送失败：' + (err.message || '未知错误'))
  } finally {
    sending.value = false
  }
}

async function sendToCurrentThread() {
  const t = selectedThread.value
  if (!t) return
  const text = String(composer.value || '').trim()
  if (!text) return
  if (!t.iccid) {
    ElMessage.error('短信会话缺少 ICCID，无法确定外呼 SIM 卡')
    return
  }
  sending.value = true
  try {
    const result = await smsStore.send({ iccid: t.iccid, phone: t.peer, message: text })
    if (!result.ok) throw new Error(result.error.message || '发送失败')
    if (result.data.submissionWarning) {
      ElMessage.warning(result.data.submissionWarning)
    }
    composer.value = ''
    if (viewingTarget.value && selectedThreadKey.value === t.key) {
      showLatestMessages()
      return
    }
    scrollThreadToBottom()
    setTimeout(async () => {
      await fetchMessagesAndThread()
    }, 800)
  } catch (e: unknown) {
    const err = toAppError(e)
    ElMessage.error('发送失败：' + (err.message || '未知错误'))
  } finally {
    sending.value = false
  }
}

async function confirmDeleteMessage(message: SMSMessage) {
  if (!message.id || deletingMessageId.value === message.id) return
  try {
    await ElMessageBox.confirm(
      '删除后无法恢复。仅删除短信中心历史记录。',
      '删除这条短信？',
      {
        confirmButtonText: '删除',
        cancelButtonText: '取消',
        type: 'warning'
      }
    )
  } catch {
    return
  }

  deletingMessageId.value = message.id
  try {
    const result = await smsStore.deleteMessage(message.id)
    if (!result.ok) throw new Error(result.error.message || '删除失败')
    ElMessage.success('已删除短信')
    if (messageTarget.value?.message.id === message.id) {
      clearSelectedThread(true)
      await fetchMessages(false)
      return
    }
    await fetchMessagesAndThread()
    if (result.data.thread_empty) {
      clearSelectedThread(true)
    }
  } catch (e: unknown) {
    const err = toAppError(e)
    ElMessage.error('删除失败：' + (err.message || '未知错误'))
  } finally {
    deletingMessageId.value = null
  }
}

async function confirmDeleteThread(thread: SmsThread) {
  if (deletingThreadKey.value === thread.key) return
  if (!thread.iccid) {
    ElMessage.error('短信会话缺少 ICCID，无法确定要删除的会话')
    return
  }
  try {
    await ElMessageBox.confirm(
      `将删除与 ${thread.peer} 的全部短信历史，无法恢复。仅删除短信中心历史记录。`,
      '永久删除整个对话？',
      {
        confirmButtonText: '删除对话',
        cancelButtonText: '取消',
        type: 'warning'
      }
    )
  } catch {
    return
  }

  deletingThreadKey.value = thread.key
  try {
    const payload = { iccid: thread.iccid, peer: thread.peer }
    const result = await smsStore.deleteThread(payload)
    if (!result.ok) throw new Error(result.error.message || '删除失败')
    ElMessage.success('已删除对话')
    const deletingCurrent = selectedThreadKey.value === thread.key
    if (deletingCurrent) clearSelectedThread(true)
    await fetchMessages(false)
  } catch (e: unknown) {
    const err = toAppError(e)
    ElMessage.error('删除失败：' + (err.message || '未知错误'))
  } finally {
    deletingThreadKey.value = null
  }
}

</script>

<template>
  <div ref="smsPageRef" class="app-page sms-page h-[calc(100vh-144px)] flex flex-col">
    <ErrorState
      v-if="devicesError"
      class="mb-4"
      title="设备列表加载失败"
      :message="devicesError.message"
      :status-code="devicesError.status"
      :request-method="devicesError.method"
      :request-url="devicesError.url"
      :last-success-at="devicesLastOkAt"
      retry-text="重试"
      @retry="fetchDevices"
    />

    <ErrorState
      v-if="messagesError"
      class="mb-6"
      title="短信加载失败"
      :message="messagesError.message"
      :status-code="messagesError.status"
      :request-method="messagesError.method"
      :request-url="messagesError.url"
      :last-success-at="messagesLastOkAt"
      retry-text="重试"
      @retry="refreshAll"
    />

    <div class="flex-1 sms-workspace ui-card ui-workspace-glow overflow-hidden relative">
      <div v-if="loading && threads.length === 0" class="absolute inset-0 z-20 flex items-center justify-center bg-white/70 dark:bg-black/40">
        <el-icon class="is-loading" size="28"><Loading /></el-icon>
      </div>

      <div class="sms-main-layout">
        <SmsDeviceRail
          :items="deviceSidebarItems"
          :selected-id="selectedDevice"
          @select="(deviceId) => void handleSelectDevice(deviceId)"
        />

        <SmsThreadListPane
          v-model="searchQuery"
          :items="presentedThreads"
          :selected-key="selectedThreadKey"
          :loading="loading"
          :deleting-key="deletingThreadKey"
          @select="(key) => void selectThread(key)"
          @delete="(thread) => void confirmDeleteThread(thread)"
          @open-actions="showThreadActionSheet"
          @new-message="openSendModal"
        />

        <section class="sms-conversation-pane flex flex-col min-w-0 min-h-0">
          <SmsConversationHeader
            :peer="selectedThread?.peer || ''"
            :context="conversationContext"
            :loading="threadLoading"
            :show-back="false"
            @refresh="() => void fetchMessagesAndThread(false)"
            @latest="() => void showLatestMessages()"
            @delete="selectedThread && void confirmDeleteThread(selectedThread)"
          />

          <div v-if="viewingTarget" class="sms-target-notice" role="status">
            <span>正在查看提醒短信</span>
            <el-button text type="primary" @click="showLatestMessages">返回最新短信</el-button>
          </div>

          <div v-if="!selectedThread" class="flex-1 flex items-center justify-center p-6">
            <EmptyState title="请选择一个会话" subtitle="从左侧联系人列表进入短信明细" />
          </div>

          <div v-else ref="detailScrollbar" class="flex-1 min-h-0 overflow-y-auto sms-detail-scroll" @scroll="onDetailScroll">
            <SmsMessageTimeline
              :groups="selectedThreadGroups"
              :loading="threadLoading"
              :can-load-more="canLoadMoreHistory"
              :loading-more="loadingHistoryMore"
              :deleting-id="deletingMessageId"
              :highlighted-id="viewingTarget ? messageTarget?.message.id : undefined"
              @load-more="loadMoreHistory"
              @delete="(message) => void confirmDeleteMessage(message)"
              @open-actions="showMessageActionSheet"
            />
          </div>

          <SmsComposer
            v-if="selectedThread"
            v-model="composer"
            :encoding="composerEstimate.encoding"
            :parts="composerEstimate.parts"
            :length="composerLen"
            :sending="sending"
            @send="sendToCurrentThread"
          />
        </section>
      </div>
    </div>

    <transition name="sms-sheet-fade">
      <div v-if="showActionSheet && isNarrowLayout" class="sms-action-sheet-mask" @click="closeActionSheet">
        <div class="sms-action-sheet" @click.stop>
          <div class="sms-action-sheet-title">操作</div>
          <el-button class="sms-danger-ghost-btn !w-full !justify-center" @click="void onActionSheetDelete()">
            <el-icon><Delete24Regular /></el-icon>
            {{ actionSheetTarget?.type === 'thread' ? '删除对话' : '删除短信' }}
          </el-button>
          <el-button class="!w-full !justify-center" @click="closeActionSheet">取消</el-button>
        </div>
      </div>
    </transition>

    <!-- Send Modal -->
    <el-dialog v-model="showSendModal" title="发送短信" width="min(520px, 92vw)" class="glass-modal">
      <el-form label-position="top" class="mt-2">
        <el-form-item label="发送设备">
          <el-select v-model="selectedSendDeviceId" placeholder="选择设备">
            <el-option v-for="opt in sendDeviceOptions" :key="opt.value" :label="opt.label" :value="opt.value" />
          </el-select>
        </el-form-item>
        <el-form-item label="目标号码">
          <div class="sms-recipient-field">
            <el-input v-model="sendForm.phone" placeholder="+86138..." />
            <el-button class="sms-contact-picker-button" aria-label="从联系人选择" @click="showContactsPicker = true">
              <el-icon><Person24Regular /></el-icon>
              联系人
            </el-button>
          </div>
          <small class="text-xs text-[var(--ui-text-muted)]">号码会原样提交；发送国际号码时请明确输入 + 国家码。</small>
        </el-form-item>
        <el-form-item label="短信内容">
          <el-input
            v-model="sendForm.message"
            type="textarea"
            placeholder="输入短信内容..."
            :autosize="{ minRows: 4, maxRows: 10 }"
            resize="none"
          />
          <div class="mt-2 text-xs flex justify-end text-[var(--ui-muted)]">
            {{ sendEstimate.encoding }} · 预计 {{ sendEstimate.parts }} 段 · {{ Array.from(String(sendForm.message || '')).length }} 字
          </div>
        </el-form-item>
      </el-form>
      <template #footer>
        <div class="flex justify-end gap-3">
          <el-button @click="showSendModal = false">取消</el-button>
          <el-button type="primary" :loading="sending" @click="handleSendModal">
            <el-icon><Send24Regular /></el-icon>
            发送
          </el-button>
        </div>
      </template>
    </el-dialog>

    <PhoneContactsDrawer
      v-model="showContactsPicker"
      :device-id="selectedSendDeviceId"
      kind="sms"
      @select="applyContactNumber"
    />
  </div>
</template>

<style scoped>
.sms-target-notice { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 4px 16px; border-bottom: 1px solid var(--ui-border); background: var(--ui-selected); color: var(--ui-text); font-size: 13px; }
.sms-target-notice :deep(.el-button) { min-height: 44px; flex-shrink: 0; }
.sms-page {
  container-type: inline-size;
}

.sms-recipient-field {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 8px;
  width: 100%;
}

.sms-recipient-field :deep(.el-input) {
  min-width: 0;
}

.sms-contact-picker-button {
  min-width: 88px;
  margin: 0;
}

.sms-workspace {
  min-height: 0;
  animation: sms-workspace-enter 220ms var(--ui-ease-out) both;
}

.sms-main-layout {
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  gap: 0;
  height: 100%;
  min-height: 0;
}

.sms-conversation-pane {
  overflow: hidden;
  background: transparent;
}

.sms-workspace :deep(.sms-device-rail),
.sms-workspace :deep(.sms-thread-list),
.sms-workspace :deep(.sms-conversation-header) {
  background: transparent;
}

.sms-workspace :deep(.sms-composer) {
  background: color-mix(in srgb, var(--ui-surface) 82%, transparent);
}

@keyframes sms-workspace-enter {
  from { opacity: 0; transform: translateY(6px); }
  to { opacity: 1; transform: translateY(0); }
}

@media (prefers-reduced-motion: reduce) {
  .sms-workspace { animation-name: sms-workspace-fade; }
  @keyframes sms-workspace-fade { from { opacity: 0; } to { opacity: 1; } }
}

.sms-action-sheet-mask {
  position: fixed;
  inset: 0;
  z-index: 2200;
  background: color-mix(in srgb, var(--ui-primary-solid) 36%, transparent);
  display: flex;
  align-items: flex-end;
  justify-content: center;
}

.sms-action-sheet {
  width: min(520px, 100%);
  background: var(--ui-surface);
  border-top-left-radius: 8px;
  border-top-right-radius: 8px;
  border: 1px solid var(--ui-border);
  border-bottom: none;
  box-shadow: var(--ui-shadow-lg);
  padding: 12px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.sms-action-sheet-title {
  font-size: 12px;
  font-weight: 700;
  color: var(--ui-text-muted);
  text-align: center;
  letter-spacing: 0.03em;
}

.sms-sheet-fade-enter-active,
.sms-sheet-fade-leave-active {
  transition: opacity 0.18s ease;
}

.sms-sheet-fade-enter-from,
.sms-sheet-fade-leave-to {
  opacity: 0;
}

:deep(.sms-danger-ghost-btn.el-button) {
  color: var(--ui-danger);
  border-color: color-mix(in srgb, var(--ui-danger) 24%, var(--ui-border));
  background: color-mix(in srgb, var(--ui-danger) 9%, transparent);
}

:deep(.sms-danger-ghost-btn.el-button:hover) {
  color: var(--ui-danger);
  border-color: color-mix(in srgb, var(--ui-danger) 38%, var(--ui-border));
  background: color-mix(in srgb, var(--ui-danger) 15%, transparent);
}

:deep(.sms-danger-ghost-btn.el-button:focus-visible) {
  outline: 2px solid color-mix(in srgb, var(--ui-danger) 25%, transparent);
  outline-offset: 2px;
}

@container (min-width: 980px) {
  .sms-main-layout {
    grid-template-columns: 218px 310px minmax(0, 1fr);
  }
}

@container (max-width: 979px) {
  .sms-workspace {
    flex: none;
    min-height: 870px;
    overflow: hidden;
  }

  .sms-main-layout {
    min-height: 870px;
    grid-template-columns: 64px minmax(0, 1fr);
    grid-template-rows: 250px minmax(620px, 1fr);
  }

  .sms-main-layout > :deep(.sms-device-rail) {
    grid-column: 1;
    grid-row: 1 / span 2;
  }

  .sms-main-layout > :deep(.sms-thread-list) {
    grid-column: 2;
    grid-row: 1;
    border-right: 0;
    border-bottom: 1px solid var(--ui-border);
  }

  .sms-conversation-pane {
    min-height: 620px;
    grid-column: 2;
    grid-row: 2;
  }
}

@media (max-width: 1180px) {
  .sms-page {
    height: auto !important;
    min-height: calc(100dvh - 104px);
  }
}

@container (max-width: 560px) {
  .sms-main-layout {
    grid-template-columns: 52px minmax(0, 1fr);
  }

}

/* 消息详情区域原生滚动条样式 */
.sms-detail-scroll::-webkit-scrollbar {
  width: 6px;
}
.sms-detail-scroll::-webkit-scrollbar-track {
  background: transparent;
}
.sms-detail-scroll::-webkit-scrollbar-thumb {
  background: color-mix(in srgb, var(--ui-text-muted) 30%, transparent);
  border-radius: 3px;
}
.sms-detail-scroll::-webkit-scrollbar-thumb:hover {
  background: color-mix(in srgb, var(--ui-text-muted) 50%, transparent);
}
/* Firefox */
.sms-detail-scroll {
  scrollbar-width: thin;
  scrollbar-color: color-mix(in srgb, var(--ui-text-muted) 30%, transparent) transparent;
}
</style>
