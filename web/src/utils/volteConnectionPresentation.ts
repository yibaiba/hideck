import { t } from '../i18n'
import type { NativeVoLTEStatus } from '../types/api'

export type VoLTEConnectionStage = Readonly<{
  key: string
  ready: boolean | undefined
}>

export type VoLTEServiceState = Readonly<{
  tone: 'is-idle' | 'is-pending' | 'is-ready' | 'is-failed'
  title: string
  detail: string
}>

export function volteRegistered(status?: NativeVoLTEStatus): boolean {
  return status?.ims_registered === true || status?.phase === 'registered'
}

export function createVoLTEStages(
  hasSIM: boolean | undefined,
  status?: NativeVoLTEStatus
): readonly VoLTEConnectionStage[] {
  const voiceReady = status?.voice_indications_ready === false ? false : status?.voice_available
  return Object.freeze([
    Object.freeze({ key: 'SIM', ready: hasSIM }),
    Object.freeze({ key: 'LTE', ready: volteStageReady(status?.lte_registered, status) }),
    Object.freeze({ key: 'PDN', ready: volteStageReady(status?.ims_pdn_active, status) }),
    Object.freeze({ key: 'IMS', ready: volteStageReady(status?.ims_registered, status) }),
    Object.freeze({ key: 'Voice', ready: volteStageReady(voiceReady, status) })
  ])
}

export function volteServiceState(enabled: boolean, status?: NativeVoLTEStatus): VoLTEServiceState {
  if (!enabled) {
    return Object.freeze({
      tone: 'is-idle',
      title: 'VoLTE 未开启',
      detail: '打开卡策略里的电话后，模组会注册原生 IMS'
    })
  }
  // A failed pass can still carry a stale reboot_required flag (provisioning
  // writes UAC before verifying), and showing "restart the modem" would hide
  // the actual error the operator needs to read.
  if (status?.phase === 'failed') {
    return Object.freeze({
      tone: 'is-failed',
      title: 'VoLTE 失败',
      detail: status.last_error || '请检查模组 IMS 注册'
    })
  }
  if (status?.reboot_required) {
    return Object.freeze({
      tone: 'is-pending',
      title: 'VoLTE 需重启模组',
      detail: status.last_error || 'USB/UAC 变更后需要重启模组'
    })
  }
  if (volteRegistered(status)) {
    return Object.freeze({
      tone: 'is-ready',
      title: 'VoLTE 已注册',
      detail: volteReadyDetail(status)
    })
  }
  if (
    status?.phase === 'registering'
    || status?.phase === 'enabling'
    || status?.phase === 'ims_enabled_unverified'
  ) {
    return Object.freeze({
      tone: 'is-pending',
      title: 'VoLTE 正在注册',
      detail: status.last_error || status.provision_stage || '等待模组 IMS 注册'
    })
  }
  return Object.freeze({
    tone: 'is-idle',
    title: 'VoLTE 等待注册',
    detail: status?.last_error || '尚未收到 IMS 状态'
  })
}

function volteStageReady(ok: boolean | undefined, status?: NativeVoLTEStatus): boolean | undefined {
  if (ok) return true
  if (status?.phase === 'failed') return false
  return undefined
}

function volteReadyDetail(status?: NativeVoLTEStatus): string {
  const parts = [status?.plmn, status?.mbn_name].map((value) => String(value || '').trim()).filter(Boolean)
  if (parts.length) return parts.join(' · ')
  return t('overview.volteRegistered')
}
