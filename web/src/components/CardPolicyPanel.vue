<script setup lang="ts">
import { computed, onMounted, toRef } from 'vue'
import { Sim24Regular } from '@vicons/fluent'
import { Loading } from '@element-plus/icons-vue'
import type { CardPolicy } from '../types/api'
import { devicesService } from '../services/devices'
import { useCardPolicyToggles, type PolicyMirror } from '../composables/useCardPolicyToggles'
import { CARD_POLICY_SAVED_RESTART_FAILED, useCardPolicyFields } from '../composables/useCardPolicyFields'
import { cardsService } from '../services/cards'
import { phoneModeLabel } from '../utils/phoneMode'
import { useUpstreamProxyStore } from '../stores/upstream-proxy'

const props = defineProps<{
  deviceId: string | undefined
  iccid: string | undefined
  policy: CardPolicy | null
  deviceOnline: boolean
  rfLock?: string
}>()

const emit = defineEmits<{
  policyChanged: []
}>()

const canToggle = computed(() => props.deviceOnline && !!props.iccid)
const rfLocked = computed(() => !!props.rfLock)
const canEditPolicy = computed(() => !!props.iccid)

// 上游 policy → 三开关镜像（喂给 composable）
const mirror = computed<PolicyMirror | null>(() =>
  props.policy
    ? {
        network_enabled: props.policy.network_enabled,
        vowifi_enabled: props.policy.vowifi_enabled,
        airplane_enabled: props.policy.airplane_enabled,
        phone_mode: props.policy.phone_mode ?? 'wifi',
        data_strategy: props.policy.data_strategy ?? 'on_demand'
      }
    : null
)

const { ipVersion, apn, vowifiUpstreamProxyID, pending: fieldPending, error: fieldError, errorCode: fieldErrorCode, errorField, saveIPVersion, saveAPN, saveVowifiUpstreamProxy } =
  useCardPolicyFields(toRef(props, 'policy'), async (patch) => {
    if (!props.iccid) return { ok: false, error: { message: 'SIM 身份未就绪' } }
    return cardsService.putPolicy(props.iccid, patch)
  }, () => emit('policyChanged'))

const upstreamStore = useUpstreamProxyStore()
const upstreamOptions = computed(() => upstreamStore.proxies.filter((proxy) => proxy.enabled))

onMounted(() => {
  void upstreamStore.fetchAll().catch(() => {})
})

function onAPNEnter(event: KeyboardEvent) {
  const target = event.target as HTMLInputElement | null
  target?.blur()
}

// live 执行器：调设备动作端点，即时生效。network 携带本组件的 ip/apn。
const {
  local,
  networkPending,
  networkFailed,
  vowifiPending,
  vowifiFailed,
  airplanePending,
  airplaneFailed,
  onNetworkToggle,
  onVoWiFiToggle,
  onAirplaneToggle,
  wifiCallingLocksRadio,
  radioMode,
  phoneModePending,
  phoneModeFailed,
  onPhoneModeChange,
  onDataStrategyChange
} = useCardPolicyToggles(mirror, {
  async applyNetwork(enabled) {
    if (!props.deviceId) return { ok: false }
    const r = enabled
      ? await devicesService.startNetwork(props.deviceId, { ip_version: ipVersion.value, apn: apn.value })
      : await devicesService.stopNetwork(props.deviceId)
    return { ok: r.ok }
  },
  async applyVoWiFi(enabled, next) {
    if (!props.deviceId) return { ok: false }
    const r = enabled
      ? await devicesService.enableVoWiFi(props.deviceId, {
          mode: next.phone_mode,
          data_strategy: next.data_strategy
        })
      : await devicesService.disableVoWiFi(props.deviceId)
    return { ok: r.ok }
  },
  async applyPhoneMode(next) {
    if (!props.deviceId) return { ok: false }
    const r = await devicesService.enableVoWiFi(props.deviceId, {
      mode: next.phone_mode,
      data_strategy: next.data_strategy
    })
    return { ok: r.ok }
  },
  async applyAirplane(enabled) {
    if (!props.deviceId) return { ok: false }
    const r = await devicesService.setFlightMode(props.deviceId, enabled)
    return { ok: r.ok }
  },
  onChanged() {
    emit('policyChanged')
  }
})

const sourceLabel = computed(() => {
  if (!props.policy) return ''
  return props.policy.source === 'user' ? '手动设置' : '自动默认'
})

const policyStatus = computed(() => {
  if (fieldError.value || networkFailed.value || vowifiFailed.value || airplaneFailed.value) {
    return { tone: 'is-error', label: '部分设置未生效' }
  }
  if (fieldPending.value || networkPending.value || vowifiPending.value || airplanePending.value) {
    return { tone: 'is-pending', label: '正在同步' }
  }
  return { tone: 'is-saved', label: props.policy ? '已同步' : '等待策略' }
})

const policyProjection = computed(() => [
  `飞行 ${radioMode.value === 'airplane' ? '开启' : '关闭'}`,
  `网络 ${local.value.network_enabled ? '开启' : '关闭'}`,
  `通话 ${phoneModeLabel(local.value.phone_mode)}${local.value.vowifi_enabled ? '开启' : '关闭'}`,
  `IP ${ipVersion.value.toUpperCase()}`
].join(' · '))

const airplaneHint = computed(() => {
  if (rfLocked.value) {
    return '这张 Lebara UK 分享卡不能关飞行或开网络，驻国内网会切到 20404，WiFi calling 会废'
  }
  if (wifiCallingLocksRadio.value) {
    return 'WiFi calling 占用射频，飞行已锁定。改成蜂窝/VoLTE或关掉电话后才能注册运营商'
  }
  if (radioMode.value === 'airplane') {
    return '开启后关射频和流量，不再注册运营商'
  }
  return '关闭后会注册运营商；网络开关只控制流量'
})
</script>

<template>
  <section class="policy-workspace">
    <header class="policy-workspace-header">
      <div class="policy-heading-icon" aria-hidden="true">
        <el-icon><Sim24Regular /></el-icon>
      </div>
      <div>
        <span>SIM POLICY</span>
        <h2>卡策略</h2>
        <p>网络和 VoWiFi 设置跟随当前 SIM 卡，修改后通过真实设备与策略接口生效</p>
      </div>
    </header>

    <!-- 无 ICCID 提示 -->
    <div v-show="!iccid" class="policy-empty-state ui-panel-muted">
      <el-icon><Sim24Regular /></el-icon>
      <strong>SIM 身份未就绪</strong>
      <span>设备尚未识别到 ICCID，策略不可操作</span>
    </div>

    <!-- 离线提示（有 ICCID 但设备离线） -->
    <div v-show="iccid && !deviceOnline" class="mb-3 px-3 py-2 rounded-[var(--ui-radius-lg)] bg-yellow-50 dark:bg-yellow-900/20 text-xs text-yellow-700 dark:text-yellow-300">
      设备离线，运行模式开关已禁用；IP 版本和 APN 仍可保存
    </div>

    <!-- 用 v-show 让 el-switch 始终挂载，避免 element-plus 2.13 在挂载前访问未就绪 input 而崩溃 -->
    <div v-show="iccid" class="policy-card">
      <!-- ICCID + 来源 -->
      <div class="policy-card-status">
        <div>
          <span>当前卡 ICCID</span>
          <strong>{{ iccid }}</strong>
        </div>
        <div class="policy-status-meta">
          <el-tag v-if="sourceLabel" :type="policy?.source === 'user' ? 'primary' : 'info'" size="small">{{ sourceLabel }}</el-tag>
          <span class="policy-sync-state" :class="policyStatus.tone"><i aria-hidden="true" />{{ policyStatus.label }}</span>
        </div>
      </div>

      <div class="policy-setting-list">
        <div class="policy-setting-row">
          <span><strong>IP 版本</strong><small>修改后自动保存，下次开启网络时生效</small></span>
          <div class="policy-field-control">
          <el-select
            v-model="ipVersion"
            class="w-full"
            :disabled="!canEditPolicy || fieldPending !== null"
            @change="saveIPVersion"
          >
            <el-option label="IPv4" value="v4" />
            <el-option label="IPv6" value="v6" />
            <el-option label="IPv4 + IPv6（双栈）" value="v4v6" />
          </el-select>
          <small v-if="fieldPending === 'ip_version'">正在保存...</small>
          <div v-if="fieldError && errorField === 'ip_version'" role="alert" class="text-xs text-red-600 dark:text-red-400">
            {{ fieldError }}，请重试
          </div>
          </div>
        </div>

        <div class="policy-setting-row">
          <span><strong>APN</strong><small>留空时继续使用自动识别结果</small></span>
          <div class="policy-field-control">
          <el-input
            v-model="apn"
            placeholder="留空自动识别"
            :disabled="!canEditPolicy || fieldPending !== null"
            @blur="saveAPN"
            @keyup.enter="onAPNEnter"
          />
          <small v-if="fieldPending === 'apn'">正在保存...</small>
          <div v-if="fieldError && errorField === 'apn'" role="alert" class="text-xs text-red-600 dark:text-red-400">
            {{ fieldError }}，请重试
          </div>
          </div>
        </div>

        <div class="policy-setting-row">
          <span>
            <strong>前置代理</strong>
            <small>指定这张卡走哪条。不指定就按国家，多条时优先选 UDP 正常的；改完会自动重连</small>
          </span>
          <div class="policy-field-control">
            <el-select
              v-model="vowifiUpstreamProxyID"
              class="w-full"
              :disabled="!canEditPolicy || fieldPending !== null"
              @change="saveVowifiUpstreamProxy"
            >
              <el-option label="按国家规则" value="" />
              <el-option label="直连" value="direct" />
              <el-option
                v-for="proxy in upstreamOptions"
                :key="proxy.id"
                :label="proxy.name || proxy.id"
                :value="proxy.id"
              />
            </el-select>
            <small v-if="fieldPending === 'vowifi_upstream_proxy_id'">{{ local.vowifi_enabled ? '正在保存并重连…' : '正在保存...' }}</small>
            <div v-if="fieldError && errorField === 'vowifi_upstream_proxy_id'" role="alert" class="text-xs text-red-600 dark:text-red-400">
              {{ fieldError }}<template v-if="fieldErrorCode !== CARD_POLICY_SAVED_RESTART_FAILED">，请重试</template>
            </div>
          </div>
        </div>

        <div class="policy-setting-row" :class="{ 'is-active': radioMode === 'airplane' }">
          <span>
            <strong>飞行模式</strong>
            <small>{{ airplaneHint }}</small>
          </span>
          <div class="flex items-center gap-2">
            <span v-if="airplaneFailed" class="text-xs text-orange-500 dark:text-orange-400">未生效</span>
            <el-icon v-if="airplanePending" class="animate-spin text-[var(--ui-muted)]"><Loading /></el-icon>
            <el-switch
              :model-value="radioMode === 'airplane'"
              :disabled="!canToggle || airplanePending || wifiCallingLocksRadio || rfLocked"
              @change="onAirplaneToggle"
            />
          </div>
        </div>

        <div
          class="policy-setting-row"
          :class="{ 'is-active': local.network_enabled }"
        >
          <span><strong>网络</strong><small>只开数据流量。飞行开启或 WiFi calling 占用射频时不可用</small></span>
            <div class="flex items-center gap-2">
              <span v-if="networkFailed" class="text-xs text-orange-500 dark:text-orange-400">未生效</span>
              <el-icon v-if="networkPending" class="animate-spin text-[var(--ui-muted)]"><Loading /></el-icon>
              <el-switch
                v-model="local.network_enabled"
                :disabled="!canToggle || radioMode === 'airplane' || networkPending || wifiCallingLocksRadio || rfLocked"
                @change="onNetworkToggle"
              />
          </div>
        </div>

        <div class="policy-setting-row" :class="{ 'is-active': local.phone_mode === 'cellular' || local.phone_mode === 'volte' }">
          <span><strong>通话方式</strong><small>切换会按该方式启动。WiFi calling 开飞行；蜂窝走软件 IMS 数据；VoLTE 走模组原生 IMS</small></span>
          <div class="policy-field-control">
            <el-select
              :model-value="local.phone_mode ?? 'wifi'"
              class="w-full"
              :disabled="!canToggle || phoneModePending || rfLocked"
              @change="onPhoneModeChange"
            >
              <el-option label="WiFi calling" value="wifi" />
              <el-option label="蜂窝数据" value="cellular" :disabled="rfLocked" />
              <el-option label="VoLTE" value="volte" :disabled="rfLocked" />
            </el-select>
            <small v-if="phoneModePending">正在切换...</small>
            <div v-if="phoneModeFailed" class="text-xs text-orange-500 dark:text-orange-400">切换未生效</div>
          </div>
        </div>

        <div
          class="policy-setting-row"
          :class="{ 'is-active': local.vowifi_enabled }"
        >
          <span>
            <strong>{{ (local.phone_mode ?? 'wifi') === 'wifi' ? '启动' : '软件电话' }}</strong>
            <small>{{ (local.phone_mode ?? 'wifi') === 'wifi'
              ? '打开后开始注册。关掉只停服务，仍是 WiFi calling'
              : '开启后可拨号。关掉只停服务，通话方式不变' }}</small>
          </span>
            <div class="flex items-center gap-2">
              <span v-if="vowifiFailed" class="text-xs text-orange-500 dark:text-orange-400">未生效</span>
              <el-icon v-if="vowifiPending" class="animate-spin text-[var(--ui-muted)]"><Loading /></el-icon>
              <el-switch
                v-model="local.vowifi_enabled"
                :disabled="!canToggle || vowifiPending"
                aria-label="启动 WiFi calling"
                @change="onVoWiFiToggle"
              />
          </div>
        </div>

        <div v-if="(local.phone_mode ?? 'wifi') === 'cellular'" class="policy-setting-row">
          <span><strong>蜂窝数据策略</strong><small>只影响流量。仅打电话时开：挂断后关数据，待机不能被叫</small></span>
          <div class="policy-field-control">
            <el-select
              :model-value="local.data_strategy ?? 'on_demand'"
              class="w-full"
              :disabled="!canToggle || phoneModePending || radioMode === 'airplane'"
              @change="onDataStrategyChange"
            >
              <el-option label="仅打电话时开启" value="on_demand" />
              <el-option label="长时间开启" value="always" />
            </el-select>
          </div>
        </div>
      </div>

      <footer class="policy-projection">
        <div><span>策略投影</span><strong>{{ policyProjection }}</strong></div>
        <small>开关即时应用；IP 与 APN 在字段变更后自动保存</small>
      </footer>
    </div>
  </section>
</template>

<style scoped>
.policy-workspace-header { min-height: 82px; display: flex; align-items: center; gap: 12px; }
.policy-heading-icon { width: 44px; height: 44px; display: grid; place-items: center; border: 1px solid var(--ui-border); border-radius: var(--ui-radius-md); color: var(--ui-primary); font-size: 22px; }
.policy-workspace-header span,
.policy-card-status > div > span,
.policy-projection span { color: var(--ui-primary); font: 700 var(--ui-font-caption) "v-mono", ui-monospace, monospace; letter-spacing: .14em; text-transform: uppercase; }
.policy-workspace-header h2 { margin: 3px 0 0; color: var(--ui-text); font-size: 20px; font-weight: 650; }
.policy-workspace-header p { margin: 3px 0 0; color: var(--ui-text-muted); font-size: var(--ui-font-body-sm); }

.policy-empty-state { min-height: 220px; display: flex; align-items: center; justify-content: center; flex-direction: column; gap: 7px; color: var(--ui-text-muted); }
.policy-empty-state .el-icon { color: var(--ui-primary); font-size: 28px; }
.policy-empty-state strong { color: var(--ui-text); }
.policy-empty-state span { font-size: var(--ui-font-caption); }

.policy-card { overflow: hidden; border: 1px solid var(--ui-border); border-radius: var(--ui-radius-xl); background: var(--ui-surface-strong); }
.policy-card-status { min-height: 72px; padding: 14px 16px; display: flex; align-items: center; justify-content: space-between; gap: 16px; border-bottom: 1px solid var(--ui-border); background: color-mix(in srgb, var(--ui-primary) 5%, var(--ui-surface)); }
.policy-card-status strong { display: block; margin-top: 4px; color: var(--ui-text); font: var(--ui-font-body-sm) "v-mono", ui-monospace, monospace; overflow-wrap: anywhere; }
.policy-status-meta,
.policy-sync-state { display: flex; align-items: center; }
.policy-status-meta { flex-wrap: wrap; justify-content: flex-end; gap: 8px; }
.policy-sync-state { gap: 5px; color: var(--ui-text-muted); font-size: var(--ui-font-caption); }
.policy-sync-state i { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
.policy-sync-state.is-saved { color: var(--ui-success); }
.policy-sync-state.is-pending { color: var(--ui-warning); }
.policy-sync-state.is-error { color: var(--ui-danger); }

.policy-setting-list { padding: 0 16px; }
.policy-setting-row { min-height: 72px; padding: 13px 0; display: flex; align-items: center; justify-content: space-between; gap: 18px; border-bottom: 1px solid var(--ui-border-muted); }
.policy-setting-row.is-active { background: linear-gradient(90deg, transparent, color-mix(in srgb, var(--ui-primary) 5%, transparent)); }
.policy-setting-row > span { min-width: 0; display: flex; flex-direction: column; }
.policy-setting-row > span strong { color: var(--ui-text); font-size: 13px; }
.policy-setting-row > span small { margin-top: 3px; color: var(--ui-text-muted); font-size: var(--ui-font-body-sm); }
.policy-field-control { width: min(360px, 52%); }
.policy-inline-status { color: var(--ui-text); font-size: 13px; font-weight: 650; }
.policy-notice { margin: 0 0 10px; padding: 10px 12px; border-radius: var(--ui-radius-md); font-size: var(--ui-font-body-sm); line-height: 1.45; }
.policy-notice.is-warn { background: color-mix(in srgb, var(--ui-warning) 12%, var(--ui-surface)); color: var(--ui-warning); }
.policy-notice.is-info { background: color-mix(in srgb, var(--ui-primary) 8%, var(--ui-surface)); color: var(--ui-text); }
.policy-field-control > small { color: var(--ui-warning); font-size: var(--ui-font-caption); }

.policy-projection { min-height: 74px; padding: 13px 16px; display: flex; align-items: center; justify-content: space-between; gap: 16px; background: var(--ui-surface-muted); }
.policy-projection strong { display: block; margin-top: 4px; color: var(--ui-text); font-size: var(--ui-font-body-sm); }
.policy-projection > small { max-width: 280px; color: var(--ui-text-muted); font-size: var(--ui-font-caption); text-align: right; }

@media (max-width: 620px) {
  .policy-workspace-header { align-items: flex-start; }
  .policy-card-status,
  .policy-setting-row,
  .policy-projection { align-items: stretch; flex-direction: column; }
  .policy-status-meta { justify-content: flex-start; }
  .policy-field-control { width: 100%; }
  .policy-projection > small { max-width: none; text-align: left; }
}
</style>
