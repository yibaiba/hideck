<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { storeToRefs } from 'pinia'
import { ElMessage, ElMessageBox } from 'element-plus'
import ErrorState from '../components/ErrorState.vue'
import PageHeader from '../components/PageHeader.vue'
import { t } from '../i18n'
import ProxyCountryRuleDrawer from '../components/proxy/ProxyCountryRuleDrawer.vue'
import ProxyInstanceEditorDrawer from '../components/proxy/ProxyInstanceEditorDrawer.vue'
import ProxyModeSwitch from '../components/proxy/ProxyModeSwitch.vue'
import ProxyOutboundInventory from '../components/proxy/ProxyOutboundInventory.vue'
import ProxyUpstreamEditorDrawer from '../components/proxy/ProxyUpstreamEditorDrawer.vue'
import ProxyUpstreamInventory from '../components/proxy/ProxyUpstreamInventory.vue'
import { usePollingScheduler } from '../composables/usePollingScheduler'
import { useProxyStore } from '../stores/proxy'
import { useUpstreamProxyStore } from '../stores/upstream-proxy'
import type {
  ProxyDevice,
  ProxyInstance,
  ProxyMode,
  UpstreamProxy,
  UpstreamProxyProbeResponse
} from '../types/api'
import { toAppError } from '../services/http'
import {
  createOutboundProxyPresentation,
  createUpstreamProxyPresentation
} from '../utils/proxyPresentation'
import { createLatestRequestGate } from '../utils/latestRequestGate'
import { upstreamProxyAddressWarning } from '../utils/upstreamProxyAddress'

// ── Tab 控制 ──
const activeTab = ref<'outbound' | 'upstream'>('upstream')

// ══════════════════════════════════════════════════════
// 出站代理（原有逻辑，不动）
// ══════════════════════════════════════════════════════
const proxyStore = useProxyStore()
const { statusMap } = storeToRefs(proxyStore)

const initialLoading = ref(true)
const refreshing = ref(false)
const loadError = ref<{ message: string; status?: number } | null>(null)
const instances = ref<ProxyInstance[]>([])
const devices = ref<ProxyDevice[]>([])
const saving = ref(false)

const drawerOpen = ref(false)
const editingInstance = ref<ProxyInstance | null>(null)
const instanceForm = ref<ProxyInstance>({
  id: '',
  name: '',
  device_id: '',
  enabled: true,
  mode: 'socks5',
  listen_addr: '0.0.0.0',
  listen_port: 1080,
  auth_enabled: false,
  username: '',
  password: ''
})

const modeOptions: Array<{ label: string; value: ProxyMode }> = [
  { label: 'SOCKS5', value: 'socks5' },
  { label: 'HTTP', value: 'http' }
]

const outboundRows = computed(() => instances.value.map(instance => (
  createOutboundProxyPresentation({
    instance,
    status: statusMap.value[instance.id],
    device: devices.value.find(device => device.id === instance.device_id)
  })
)))

watch(
  () => instanceForm.value.auth_enabled,
  (enabled) => {
    if (!enabled) {
      instanceForm.value.username = ''
      instanceForm.value.password = ''
    }
  }
)

async function fetchOverview(opts: { silent?: boolean; initial?: boolean } = {}) {
  const isInitial = opts.initial === true
  const silent = opts.silent === true
  if (isInitial) {
    initialLoading.value = true
  } else if (!silent) {
    refreshing.value = true
  }
  loadError.value = null

  try {
    const result = await proxyStore.fetchOverview()
    if (!result.ok) throw new Error(result.error.message)
    instances.value = proxyStore.instances.map((inst) => ({
      ...inst,
      mode: inst.mode || 'socks5'
    }))
    devices.value = proxyStore.devices
  } catch (e: unknown) {
    const err = toAppError(e)
    loadError.value = {
      message: err.message || '加载代理配置失败',
      status: err.status
    }
  } finally {
    if (isInitial) {
      initialLoading.value = false
    } else if (!silent) {
      refreshing.value = false
    }
  }
}

async function saveConfig() {
  saving.value = true
  try {
    const result = await proxyStore.saveConfig(instances.value)
    if (!result.ok) throw new Error(result.error.message || '保存失败')
    ElMessage.success('配置已保存')
    await fetchOverview()
  } catch (e: unknown) {
    const err = toAppError(e)
    ElMessage.error(err.message || '保存失败')
  } finally {
    saving.value = false
  }
}

async function startInstance(id: string) {
  try {
    const result = await proxyStore.startInstance(id)
    if (!result.ok) throw new Error(result.error.message || '启动失败')
    ElMessage.success('已启动')
    await fetchOverview()
  } catch (e: unknown) {
    const err = toAppError(e)
    ElMessage.error(err.message || '启动失败')
  }
}

async function stopInstance(id: string) {
  try {
    const result = await proxyStore.stopInstance(id)
    if (!result.ok) throw new Error(result.error.message || '停止失败')
    ElMessage.success('已停止')
    await fetchOverview()
  } catch (e: unknown) {
    const err = toAppError(e)
    ElMessage.error(err.message || '停止失败')
  }
}

async function restartInstance(id: string) {
  try {
    const result = await proxyStore.restartInstance(id)
    if (!result.ok) throw new Error(result.error.message || '重启失败')
    ElMessage.success('已重启')
    await fetchOverview()
  } catch (e: unknown) {
    const err = toAppError(e)
    ElMessage.error(err.message || '重启失败')
  }
}

function resetForm() {
  instanceForm.value = {
    id: '',
    name: '',
    device_id: devices.value[0]?.id || '',
    enabled: true,
    mode: 'socks5',
    listen_addr: '0.0.0.0',
    listen_port: 1080,
    auth_enabled: false,
    username: '',
    password: ''
  }
}

async function openDrawer(inst?: ProxyInstance) {
  if (!inst) {
    editingInstance.value = null
    resetForm()
    instanceForm.value.id = `proxy-${Date.now()}`
    instanceForm.value.listen_port = 10800 + instances.value.length
    drawerOpen.value = true
    return
  }

  try {
    const result = await proxyStore.fetchInstance(inst.id)
    if (!result.ok) throw result.error
    editingInstance.value = inst
    instanceForm.value = { ...result.data, mode: result.data.mode || 'socks5' }
    drawerOpen.value = true
  } catch (e: unknown) {
    const err = toAppError(e)
    ElMessage.error(err.message || '读取完整实例配置失败')
  }
}

function saveForm() {
  const form = { ...instanceForm.value }

  if (!form.id) {
    ElMessage.warning('实例 ID 不能为空')
    return
  }
  if (!form.device_id) {
    ElMessage.warning('必须绑定设备')
    return
  }
  if (form.mode !== 'socks5' && form.mode !== 'http') {
    ElMessage.warning('代理模式仅支持 SOCKS5 或 HTTP')
    return
  }
  if (form.listen_port <= 0 || form.listen_port > 65535) {
    ElMessage.warning('监听端口无效')
    return
  }
  if (!form.listen_addr) {
    form.listen_addr = '0.0.0.0'
  }

  if (form.auth_enabled) {
    form.username = (form.username || '').trim()
    form.password = (form.password || '').trim()
    if (!form.username || !form.password) {
      ElMessage.warning('启用认证时必须填写用户名和密码')
      return
    }
  } else {
    form.username = ''
    form.password = ''
  }

  if (editingInstance.value) {
    const idx = instances.value.findIndex((i) => i.id === editingInstance.value!.id)
    if (idx >= 0) {
      instances.value[idx] = form
    }
  } else {
    if (instances.value.some((i) => i.id === form.id)) {
      ElMessage.warning('实例 ID 已存在')
      return
    }
    instances.value.push(form)
  }

  drawerOpen.value = false
  saveConfig()
}

async function deleteInstance(id: string) {
  const confirmed = await ElMessageBox.confirm(
    `确定删除实例 ${id}？`,
    '确认删除',
    { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' }
  ).then(() => true).catch(() => false)

  if (!confirmed) return
  instances.value = instances.value.filter((i) => i.id !== id)
  saveConfig()
}

function editOutboundInstance(id: string) {
  const instance = instances.value.find(item => item.id === id)
  if (instance) void openDrawer(instance)
}

const pollEnabled = computed(() => !initialLoading.value && instances.value.length > 0)
usePollingScheduler(() => fetchOverview({ silent: true }), 5000, {
  enabled: pollEnabled,
  maxIntervalMs: 60000,
  backgroundIntervalMs: 15000
})


// ══════════════════════════════════════════════════════
// 前置代理（新增逻辑）
// ══════════════════════════════════════════════════════
const upstreamStore = useUpstreamProxyStore()
const enabledUpstreamCount = computed(() => upstreamStore.proxies.filter((proxy) => proxy.enabled).length)
const runningOutboundCount = computed(() => outboundRows.value.filter((instance) => instance.running).length)

const upstreamLoading = ref(true)
const upstreamRefreshing = ref(false)
const upstreamError = ref<{ message: string; status?: number } | null>(null)
const upstreamRequestGate = createLatestRequestGate()

// ── 编辑 Drawer ──
const upstreamDrawerOpen = ref(false)
const editingUpstream = ref<UpstreamProxy | null>(null)
const upstreamForm = ref<UpstreamProxy>({
  id: '',
  name: '',
  addr: '',
  username: '',
  password: '',
  enabled: true
})

// ── 国家规则管理 Drawer ──
const countryRuleDrawerOpen = ref(false)
const countryRuleTargetProxy = ref<UpstreamProxy | null>(null)
const selectedCountryCode = ref('')

const availableCountries = computed(() => {
  if (!countryRuleTargetProxy.value) return []
  const bound = new Set(
    upstreamStore.getRulesForProxy(countryRuleTargetProxy.value.id).map((rule) => rule.country_code)
  )
  return upstreamStore.countries.filter((country) => !bound.has(country.country_code))
})

const currentProxyCountryRules = computed(() => {
  if (!countryRuleTargetProxy.value) return []
  return upstreamStore.getRulesForProxy(countryRuleTargetProxy.value.id)
})

// 前置代理列表（带国家规则数量）
const upstreamRows = computed(() => upstreamStore.proxies.map(proxy => (
  createUpstreamProxyPresentation({
    proxy,
    health: upstreamStore.probeStatusMap[proxy.id],
    ruleCount: upstreamStore.getRulesForProxy(proxy.id).length
  })
)))

const countryRuleTargetPresentation = computed(() => {
  const targetId = countryRuleTargetProxy.value?.id
  if (!targetId) return null
  return upstreamRows.value.find(row => row.id === targetId) ?? null
})

async function fetchUpstream(opts: { silent?: boolean; initial?: boolean } = {}) {
  const request = upstreamRequestGate.begin()
  const isInitial = opts.initial === true
  const silent = opts.silent === true
  upstreamLoading.value = isInitial
  upstreamRefreshing.value = !isInitial && !silent
  upstreamError.value = null

  try {
    const result = await upstreamStore.fetchAll()
    if (!request.isCurrent()) return
    const error = result.ok ? upstreamStore.error : result.error
    if (error) throw error
  } catch (e: unknown) {
    if (!request.isCurrent()) return
    const err = toAppError(e)
    upstreamError.value = {
      message: err.message || '加载前置代理失败',
      status: err.status
    }
  } finally {
    if (request.isCurrent()) {
      upstreamLoading.value = false
      upstreamRefreshing.value = false
    }
  }
}

function openUpstreamDrawer(proxy?: UpstreamProxy) {
  if (!proxy) {
    editingUpstream.value = null
    upstreamForm.value = {
      id: '',
      name: '',
      addr: '',
      username: '',
      password: '',
      enabled: true
    }
  } else {
    editingUpstream.value = proxy
    upstreamForm.value = { ...proxy }
    // 密码脱敏时清空，让用户重新输入
    if (upstreamForm.value.password === '****') {
      upstreamForm.value.password = ''
    }
  }
  upstreamDrawerOpen.value = true
}

async function saveUpstreamForm() {
  const form = { ...upstreamForm.value }
  form.id = (form.id || '').trim()
  form.name = (form.name || '').trim()
  form.addr = (form.addr || '').trim()

  if (!form.id) {
    ElMessage.warning('ID 不能为空')
    return
  }
  if (!form.addr) {
    ElMessage.warning('Socks5 地址不能为空')
    return
  }
  const addrWarning = upstreamProxyAddressWarning(form.addr)
  if (addrWarning) {
    ElMessage.warning(addrWarning)
    return
  }

  try {
    if (editingUpstream.value) {
      // 更新
      const result = await upstreamStore.updateProxy(form.id, form)
      if (!result.ok) throw new Error(result.error.message || '更新失败')
      showUpstreamSaveResult(result.data, '前置代理已更新')
    } else {
      // 新增
      const result = await upstreamStore.createProxy(form)
      if (!result.ok) throw new Error(result.error.message || '创建失败')
      showUpstreamSaveResult(result.data, '前置代理已创建')
    }
    upstreamDrawerOpen.value = false
    await fetchUpstream()
  } catch (e: unknown) {
    const err = toAppError(e)
    ElMessage.error(err.message || '保存失败')
  }
}

function showUpstreamSaveResult(result: UpstreamProxyProbeResponse, fallback: string) {
  const message = result.message?.trim() || fallback
  if (result.status === 'warning') {
    ElMessage.warning(message)
    return
  }
  ElMessage.success(message)
}

async function deleteUpstream(proxy: UpstreamProxy) {
  const confirmed = await ElMessageBox.confirm(
    `确定删除前置代理「${proxy.name || proxy.id}」？\n绑定到该代理的国家规则将自动删除，相关国家会恢复直连。`,
    '确认删除',
    { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' }
  ).then(() => true).catch(() => false)

  if (!confirmed) return

  try {
    const result = await upstreamStore.deleteProxy(proxy.id)
    if (!result.ok) throw new Error(result.error.message || '删除失败')
    ElMessage.success('前置代理已删除')
    await fetchUpstream()
  } catch (e: unknown) {
    const err = toAppError(e)
    ElMessage.error(err.message || '删除失败')
  }
}

function openCountryRuleDrawer(proxy: UpstreamProxy) {
  countryRuleTargetProxy.value = proxy
  selectedCountryCode.value = ''
  countryRuleDrawerOpen.value = true
}

function findUpstreamProxy(id: string): UpstreamProxy | undefined {
  return upstreamStore.proxies.find(proxy => proxy.id === id)
}

function editUpstreamProxy(id: string) {
  const proxy = findUpstreamProxy(id)
  if (proxy) openUpstreamDrawer(proxy)
}

function removeUpstreamProxy(id: string) {
  const proxy = findUpstreamProxy(id)
  if (proxy) void deleteUpstream(proxy)
}

function manageUpstreamRules(id: string) {
  const proxy = findUpstreamProxy(id)
  if (proxy) openCountryRuleDrawer(proxy)
}

async function doUpsertCountryRule() {
  if (!countryRuleTargetProxy.value || !selectedCountryCode.value) {
    ElMessage.warning('请选择国家')
    return
  }

  try {
    const result = await upstreamStore.upsertCountryRule(selectedCountryCode.value, {
      upstream_proxy_id: countryRuleTargetProxy.value.id,
      enabled: true
    })
    if (!result.ok) throw new Error(result.error.message || '保存规则失败')
    ElMessage.success('国家规则已保存。同一国家绑多条时，开 VoWiFi 会优先选 UDP 正常的节点')
    selectedCountryCode.value = ''
    await fetchUpstream()
  } catch (e: unknown) {
    const err = toAppError(e)
    ElMessage.error(err.message || '保存规则失败')
  }
}

async function doDeleteCountryRule(countryCode: string) {
  try {
    const result = await upstreamStore.deleteCountryRule(countryCode, countryRuleTargetProxy.value?.id || '')
    if (!result.ok) throw new Error(result.error.message || '删除规则失败')
    ElMessage.success('已从该代理移除这个国家')
    await fetchUpstream()
  } catch (e: unknown) {
    const err = toAppError(e)
    ElMessage.error(err.message || '删除规则失败')
  }
}

// ── 统一初始化 ──
onMounted(() => {
  fetchOverview({ initial: true })
  fetchUpstream({ initial: true })
})

// 前置代理轮询
const upPollEnabled = computed(() => !upstreamLoading.value && activeTab.value === 'upstream')
usePollingScheduler(() => fetchUpstream({ silent: true }), 10000, {
  enabled: upPollEnabled,
  maxIntervalMs: 60000,
  backgroundIntervalMs: 30000
})
</script>

<template>
  <div class="app-page proxy-page">
    <PageHeader :title="t('proxy.title')" :subtitle="t('proxy.subtitle')" />

    <section class="proxy-workspace ui-card ui-workspace-glow">
      <ProxyModeSwitch
        v-model="activeTab"
        :enabled-upstream-count="enabledUpstreamCount"
        :outbound-count="instances.length"
        :rule-count="upstreamStore.countryRules.length"
        :running-outbound-count="runningOutboundCount"
        :upstream-count="upstreamStore.proxies.length"
      />

      <Transition name="proxy-mode" mode="out-in">
        <div v-if="activeTab === 'upstream'" key="upstream" class="proxy-workspace-content">
          <ErrorState
            v-if="upstreamError"
            class="m-4"
            title="加载前置代理失败"
            :message="upstreamError.message"
            :status-code="upstreamError.status"
            retry-text="重试"
            @retry="fetchUpstream"
          />

          <ProxyUpstreamInventory
            :loading="upstreamLoading"
            :refreshing="upstreamRefreshing"
            :rows="upstreamRows"
            @add="openUpstreamDrawer()"
            @delete="removeUpstreamProxy"
            @edit="editUpstreamProxy"
            @refresh="fetchUpstream"
            @rules="manageUpstreamRules"
          />
        </div>

        <div v-else key="outbound" class="proxy-workspace-content">
          <ErrorState
            v-if="loadError"
            class="m-4"
            title="加载代理配置失败"
            :message="loadError.message"
            :status-code="loadError.status"
            retry-text="重试"
            @retry="fetchOverview"
          />

          <ProxyOutboundInventory
            :loading="initialLoading"
            :refreshing="refreshing"
            :rows="outboundRows"
            @add="openDrawer()"
            @delete="deleteInstance"
            @edit="editOutboundInstance"
            @refresh="fetchOverview"
            @restart="restartInstance"
            @start="startInstance"
            @stop="stopInstance"
          />
        </div>
      </Transition>
    </section>

    <ProxyInstanceEditorDrawer
      v-model="drawerOpen"
      v-model:form="instanceForm"
      :devices="devices"
      :editing="!!editingInstance"
      :mode-options="modeOptions"
      :saving="saving"
      @save="saveForm"
    />

    <ProxyUpstreamEditorDrawer
      v-model="upstreamDrawerOpen"
      v-model:form="upstreamForm"
      :editing="!!editingUpstream"
      @save="saveUpstreamForm"
    />

    <ProxyCountryRuleDrawer
      v-model="countryRuleDrawerOpen"
      v-model:selected-country-code="selectedCountryCode"
      :available-countries="availableCountries"
      :rules="currentProxyCountryRules"
      :target="countryRuleTargetPresentation"
      @delete="doDeleteCountryRule"
      @save="doUpsertCountryRule"
    />
  </div>
</template>

<style scoped>
.proxy-workspace { min-width: 0; overflow: hidden; }
.proxy-workspace-content { min-width: 0; }

.proxy-page :deep(.ui-panel-muted) {
  border-radius: var(--ui-radius-xl);
}

.proxy-mode-enter-active {
  transition: transform 180ms var(--ui-ease-out), opacity 180ms var(--ui-ease-out);
}

.proxy-mode-leave-active {
  transition: transform 120ms var(--ui-ease-out), opacity 120ms var(--ui-ease-out);
}

.proxy-mode-enter-from {
  opacity: 0;
  transform: translateY(4px);
}

.proxy-mode-leave-to {
  opacity: 0;
  transform: translateY(-2px);
}

@media (prefers-reduced-motion: reduce) {
  .proxy-mode-enter-active,
  .proxy-mode-leave-active {
    transition-property: opacity;
  }

  .proxy-mode-enter-from,
  .proxy-mode-leave-to {
    transform: none;
  }
}
</style>
