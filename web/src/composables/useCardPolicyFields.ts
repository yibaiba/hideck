import { ref, watch, type Ref } from 'vue'
import type { CardPolicy } from '../types/api'

type EditablePolicyFields = Pick<CardPolicy, 'ip_version' | 'apn' | 'vowifi_upstream_proxy_id' | 'local_phone_override' | 'mwi_mode'>
type SaveResult = { ok: boolean; error?: { message?: string; code?: string } }

export const CARD_POLICY_SAVED_RESTART_FAILED = 'card_policy_saved_restart_failed'
// Error codes for a saved policy whose follow-up action failed start with this
// prefix; the UI reloads the policy instead of reverting the field.
const CARD_POLICY_SAVED_PREFIX = 'card_policy_saved_'

export function useCardPolicyFields(
  source: Ref<CardPolicy | null>,
  save: (patch: Partial<EditablePolicyFields> & { redetect_mwi?: boolean }) => Promise<SaveResult>,
  onChanged?: () => void
) {
  const ipVersion = ref<CardPolicy['ip_version']>('v4')
  const apn = ref('')
  const vowifiUpstreamProxyID = ref('')
  const localPhoneOverride = ref('')
  const mwiMode = ref<NonNullable<CardPolicy['mwi_mode']>>('auto')
  const pending = ref<keyof EditablePolicyFields | null>(null)
  const error = ref('')
  const errorCode = ref('')
  const errorField = ref<keyof EditablePolicyFields | null>(null)
  const persisted = ref<Record<keyof EditablePolicyFields, string>>({
    ip_version: 'v4', apn: '', vowifi_upstream_proxy_id: '', local_phone_override: '', mwi_mode: 'auto'
  })

  watch(source, (policy) => {
    if (!policy || pending.value) return
    ipVersion.value = policy.ip_version || 'v4'
    apn.value = policy.apn || ''
    vowifiUpstreamProxyID.value = policy.vowifi_upstream_proxy_id || ''
    localPhoneOverride.value = policy.local_phone_override || ''
    mwiMode.value = policy.mwi_mode || 'auto'
    persisted.value = {
      ip_version: ipVersion.value,
      apn: apn.value,
      vowifi_upstream_proxy_id: vowifiUpstreamProxyID.value,
      local_phone_override: localPhoneOverride.value,
      mwi_mode: mwiMode.value
    }
  }, { immediate: true })

  function currentValue(field: keyof EditablePolicyFields) {
    if (field === 'ip_version') return ipVersion.value
    if (field === 'apn') return apn.value
    if (field === 'vowifi_upstream_proxy_id') return vowifiUpstreamProxyID.value
    if (field === 'mwi_mode') return mwiMode.value
    return localPhoneOverride.value
  }

  function restore(field: keyof EditablePolicyFields) {
    const value = persisted.value[field]
    if (field === 'ip_version') ipVersion.value = value as CardPolicy['ip_version']
    else if (field === 'apn') apn.value = value
    else if (field === 'vowifi_upstream_proxy_id') vowifiUpstreamProxyID.value = value
    else if (field === 'mwi_mode') mwiMode.value = value as NonNullable<CardPolicy['mwi_mode']>
    else localPhoneOverride.value = value
  }

  async function persist(field: keyof EditablePolicyFields) {
    if (!source.value || pending.value) return
    const previous = persisted.value[field]
    if (field === 'apn') apn.value = apn.value.trim()
    if (field === 'local_phone_override') localPhoneOverride.value = localPhoneOverride.value.trim()
    const value = currentValue(field)
    if (value === previous) return

    pending.value = field
    error.value = ''
    errorCode.value = ''
    errorField.value = null
    const result = await save({ [field]: value })
    pending.value = null
    if (!result.ok) {
      const code = result.error?.code || ''
      if (code.startsWith(CARD_POLICY_SAVED_PREFIX)) onChanged?.()
      else restore(field)
      error.value = result.error?.message || '策略保存失败'
      errorCode.value = code
      errorField.value = field
      return
    }
    persisted.value = { ...persisted.value, [field]: value }
    onChanged?.()
  }

  // Redetect while already "on": skip the unchanged-value check so the server
  // clears the rejection records.
  async function redetectMWI() {
    if (!source.value || pending.value) return
    pending.value = 'mwi_mode'
    error.value = ''
    errorCode.value = ''
    errorField.value = null
    const result = await save({ mwi_mode: 'on', redetect_mwi: true })
    pending.value = null
    if (!result.ok) {
      error.value = result.error?.message || '重新检测失败'
      errorCode.value = result.error?.code || ''
      errorField.value = 'mwi_mode'
      if (errorCode.value.startsWith(CARD_POLICY_SAVED_PREFIX)) onChanged?.()
      return
    }
    mwiMode.value = 'on'
    persisted.value = { ...persisted.value, mwi_mode: 'on' }
    onChanged?.()
  }

  return {
    redetectMWI,
    ipVersion, apn, vowifiUpstreamProxyID, localPhoneOverride, mwiMode, pending, error, errorCode, errorField,
    saveIPVersion: () => persist('ip_version'),
    saveAPN: () => persist('apn'),
    saveVowifiUpstreamProxy: () => persist('vowifi_upstream_proxy_id'),
    saveLocalPhoneOverride: () => persist('local_phone_override'),
    saveMWIMode: () => persist('mwi_mode')
  }
}
