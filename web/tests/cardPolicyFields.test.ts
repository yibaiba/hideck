import assert from 'node:assert/strict'
import test from 'node:test'
import { nextTick, ref } from 'vue'
import { useCardPolicyFields } from '../src/composables/useCardPolicyFields'
import type { CardPolicy } from '../src/types/api'

function policy(): CardPolicy {
  return {
    iccid: '8986001',
    network_enabled: false,
    vowifi_enabled: false,
    airplane_enabled: false,
    ip_version: 'v4v6',
    apn: 'ims',
    source: 'user'
  }
}

test('clearing APN persists an explicit empty value', async () => {
  const source = ref<CardPolicy | null>(policy())
  const patches: Array<Record<string, unknown>> = []
  const fields = useCardPolicyFields(source, async (patch) => {
    patches.push(patch)
    return { ok: true }
  })
  await nextTick()

  fields.apn.value = '   '
  await fields.saveAPN()

  assert.deepEqual(patches, [{ apn: '' }])
  assert.equal(fields.apn.value, '')
  assert.equal(fields.error.value, '')
})

test('failed field save restores the persisted value and exposes the error', async () => {
  const source = ref<CardPolicy | null>(policy())
  const fields = useCardPolicyFields(source, async () => ({
    ok: false,
    error: { message: 'database unavailable' }
  }))
  await nextTick()

  fields.ipVersion.value = 'v6'
  await fields.saveIPVersion()

  assert.equal(fields.ipVersion.value, 'v4v6')
  assert.equal(fields.error.value, 'database unavailable')
  assert.equal(fields.errorField.value, 'ip_version')
  assert.equal(fields.pending.value, null)
})

test('saving a VoWiFi upstream proxy override persists the selected node', async () => {
  const source = ref<CardPolicy | null>(policy())
  const patches: Array<Record<string, unknown>> = []
  const fields = useCardPolicyFields(source, async (patch) => {
    patches.push(patch)
    return { ok: true }
  })
  await nextTick()

  fields.vowifiUpstreamProxyID.value = 'uk-node-2'
  await fields.saveVowifiUpstreamProxy()

  assert.deepEqual(patches, [{ vowifi_upstream_proxy_id: 'uk-node-2' }])
})

test('a saved proxy policy keeps its value and exposes a restart failure', async () => {
  const source = ref<CardPolicy | null>(policy())
  let changed = 0
  const fields = useCardPolicyFields(source, async () => ({
    ok: false,
    error: {
      code: 'card_policy_saved_restart_failed',
      message: '卡策略已保存，但 WiFi calling 重连失败'
    }
  }), () => { changed++ })
  await nextTick()

  fields.vowifiUpstreamProxyID.value = 'uk-node-2'
  await fields.saveVowifiUpstreamProxy()

  assert.equal(fields.vowifiUpstreamProxyID.value, 'uk-node-2')
  assert.equal(fields.errorCode.value, 'card_policy_saved_restart_failed')
  assert.equal(fields.error.value, '卡策略已保存，但 WiFi calling 重连失败')
  assert.equal(changed, 1)
})

test('manual local number is trimmed and an empty value clears the override', async () => {
  const source = ref<CardPolicy | null>(policy())
  const patches: Array<Record<string, unknown>> = []
  const fields = useCardPolicyFields(source, async (patch) => {
    patches.push(patch)
    return { ok: true }
  })
  await nextTick()

  fields.localPhoneOverride.value = '  +447911123456  '
  await fields.saveLocalPhoneOverride()
  fields.localPhoneOverride.value = '  '
  await fields.saveLocalPhoneOverride()

  assert.deepEqual(patches, [
    { local_phone_override: '+447911123456' },
    { local_phone_override: '' }
  ])
})

test('failed manual local number save restores the persisted value', async () => {
  const source = ref<CardPolicy | null>({ ...policy(), local_phone_override: '+447700900123' })
  const fields = useCardPolicyFields(source, async () => ({
    ok: false,
    error: { message: '本机号码只能包含数字和开头的 +' }
  }))
  await nextTick()

  fields.localPhoneOverride.value = '+44 7700'
  await fields.saveLocalPhoneOverride()

  assert.equal(fields.localPhoneOverride.value, '+447700900123')
  assert.equal(fields.errorField.value, 'local_phone_override')
})

test('voicemail indication defaults to auto and saves the selected mode', async () => {
  const source = ref<CardPolicy | null>(policy())
  const patches: Array<Record<string, unknown>> = []
  const fields = useCardPolicyFields(source, async (patch) => {
    patches.push(patch)
    return { ok: true }
  })
  await nextTick()
  assert.equal(fields.mwiMode.value, 'auto')

  fields.mwiMode.value = 'off'
  await fields.saveMWIMode()
  fields.mwiMode.value = 'off'
  await fields.saveMWIMode()

  assert.deepEqual(patches, [{ mwi_mode: 'off' }])
})

test('redetecting MWI resends on even when the setting is already on', async () => {
  const source = ref<CardPolicy | null>({ ...policy(), mwi_mode: 'on' })
  const patches: Array<Record<string, unknown>> = []
  const fields = useCardPolicyFields(source, async (patch) => {
    patches.push(patch)
    return { ok: true }
  })
  await nextTick()
  await fields.redetectMWI()
  assert.deepEqual(patches, [{ mwi_mode: 'on', redetect_mwi: true }])
})

test('a saved policy whose follow-up failed is reloaded instead of rolled back', async () => {
  const source = ref<CardPolicy | null>({ ...policy(), mwi_mode: 'off' })
  let reloads = 0
  const fields = useCardPolicyFields(source, async () => ({
    ok: false,
    error: { message: '清除失败', code: 'card_policy_saved_mwi_clear_failed' }
  }), () => { reloads++ })
  await nextTick()
  fields.mwiMode.value = 'on'
  await fields.saveMWIMode()
  assert.equal(reloads, 1)
  assert.equal(fields.mwiMode.value, 'on')
  assert.equal(fields.errorField.value, 'mwi_mode')
})
