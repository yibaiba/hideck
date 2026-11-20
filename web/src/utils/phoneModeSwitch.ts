import type { PhoneDevice } from '../services/phone'
import { phoneModeCampsOnCell, phoneModeLabel } from './phoneMode'

export type PhoneModeWarning = { title: string; message: string }

export function phoneModeDisabledReason(device: PhoneDevice | undefined, mode: string): string | undefined {
  if (!device) return '请先选择设备'
  if ((mode === 'wifi' || mode === 'cellular') && device.software_ims_blocked) {
    return device.native_voice_unavailable_reason
      ? `此 SIM 卡不支持软件 IMS。${device.native_voice_unavailable_reason}`
      : '此 SIM 卡不支持软件 IMS，请使用 VoLTE 或模组直拨'
  }
  if ((mode === 'volte' || mode === 'modem_voice') && device.native_voice_unavailable_reason) {
    return device.native_voice_unavailable_reason
  }
  if (mode === 'cellular' && device.cellular_ims_unavailable_reason) {
    return device.cellular_ims_unavailable_reason
  }
  if (phoneModeCampsOnCell(mode) && device.rf_lock) {
    return '此 SIM 卡禁止蜂窝驻网，不能切换到该模式'
  }
}

export function phoneModeWarning(device: PhoneDevice, mode: string): PhoneModeWarning | undefined {
  if (!phoneModeCampsOnCell(mode)) return
  const label = phoneModeLabel(mode)
  const region = device.phone_region?.toUpperCase()
  const roaming = region && region !== 'CN'
    ? '这是一张境外 SIM 卡；在归属地以外驻网或通话可能产生漫游费用，也可能受运营商漫游限制。'
    : '在 SIM 卡归属地以外使用，可能产生漫游费用。'
  const audio = mode === 'modem_voice'
    ? '模组直拨需要已适配的 USB 音频和 ADB 接口；未禁用自动准备时，会为 QDC507GLEFM21 模组尝试开启 ADB。若 USB 配置已写入但 ADB 仍未出现，无通话时会自动重启目标模组一次，期间该设备会暂时离线；失败不反复重启。'
    : ''
  return {
    title: `切换到${label}？`,
    message: `设备 ${device.name || device.id}（${device.id}）将使用蜂窝驻网，并退出飞行模式；若正在使用 WiFi calling，其服务将停止。${roaming}${audio}是否继续？`
  }
}

type ChangeOptions = {
  target: PhoneDevice
  mode: string
  current: () => PhoneDevice | undefined
  hasCall: () => boolean
  confirm: (warning: PhoneModeWarning) => Promise<unknown>
  apply: () => Promise<void>
}

export async function confirmPhoneModeChange(options: ChangeOptions): Promise<boolean> {
  const disabledReason = phoneModeDisabledReason(options.target, options.mode)
  if (disabledReason) throw new Error(disabledReason)
  const warning = phoneModeWarning(options.target, options.mode)
  if (warning) {
    try {
      await options.confirm(warning)
    } catch (error) {
      if (error === 'cancel' || error === 'close') return false
      throw error
    }
  }
  const current = options.current()
  const target = options.target
  if (!current || options.hasCall() || current.id !== target.id || current.iccid !== target.iccid
    || current.phone_mode !== target.phone_mode || current.vowifi_enabled !== target.vowifi_enabled
    || current.rf_lock !== target.rf_lock || current.software_ims_blocked !== target.software_ims_blocked
    || current.native_voice_unavailable_reason !== target.native_voice_unavailable_reason
    || current.cellular_ims_unavailable_reason !== target.cellular_ims_unavailable_reason) {
    throw new Error('设备、SIM 卡或通话状态已变化，请重新确认切换')
  }
  await options.apply()
  return true
}
