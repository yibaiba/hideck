import assert from 'node:assert/strict'
import test from 'node:test'
import { confirmPhoneModeChange, phoneModeDisabledReason, phoneModeWarning } from '../src/utils/phoneModeSwitch'
import { readFileSync } from 'node:fs'
import type { PhoneDevice } from '../src/services/phone'

const device = (): PhoneDevice => ({ id: 'wwan0', name: 'VOXI', iccid: 'card-a',
  phone_region: 'GB', phone_mode: 'wifi', vowifi_enabled: true, voice: {} })

test('disabled software IMS and RF modes never prompt, apply or redirect to another mode', async () => {
  for (const mode of ['wifi', 'cellular', 'volte', 'modem_voice']) {
    const target = { ...device(), software_ims_blocked: true, rf_lock: 'locked' }
    let prompted = false
    let applied = false
    await assert.rejects(confirmPhoneModeChange({
      target, mode, current: () => target, hasCall: () => false,
      confirm: async () => { prompted = true }, apply: async () => { applied = true }
    }), /不支持软件 IMS|禁止蜂窝驻网/)
    assert.equal(prompted, false)
    assert.equal(applied, false)
  }
})

test('mode availability follows SIM capability and RF lock, not country alone', () => {
  const domestic = { ...device(), phone_region: 'CN', software_ims_blocked: true }
  assert.ok(phoneModeDisabledReason(domestic, 'wifi'))
  assert.ok(phoneModeDisabledReason(domestic, 'cellular'))
  assert.equal(phoneModeDisabledReason(domestic, 'volte'), undefined)
  assert.equal(phoneModeDisabledReason(domestic, 'modem_voice'), undefined)
  assert.equal(phoneModeDisabledReason({ ...device(), rf_lock: 'locked' }, 'wifi'), undefined)
  assert.equal(phoneModeDisabledReason(device(), 'wifi'), undefined)
  assert.equal(phoneModeDisabledReason({ ...device(), phone_region: 'CN' }, 'wifi'), undefined)
  assert.ok(phoneModeDisabledReason(undefined, 'wifi'))
})

test('ML307C blocks native voice and cellular IMS modes and preserves One NZ WiFi calling', () => {
  const reason = 'ML307C 在 HiDeck 中不支持原生 VoLTE 和模组直拨'
  const cellularReason = 'ML307C 在 HiDeck 中不支持通过模组蜂窝数据使用软件 IMS'
  const oneNZ = {
    ...device(), phone_region: 'NZ', native_voice_unavailable_reason: reason,
    cellular_ims_unavailable_reason: cellularReason
  }
  assert.equal(phoneModeDisabledReason(oneNZ, 'volte'), reason)
  assert.equal(phoneModeDisabledReason(oneNZ, 'modem_voice'), reason)
  assert.equal(phoneModeDisabledReason(oneNZ, 'wifi'), undefined)
  assert.equal(phoneModeDisabledReason(oneNZ, 'cellular'), cellularReason)
  const china = { ...oneNZ, phone_region: 'CN', software_ims_blocked: true }
  assert.equal(phoneModeDisabledReason(china, 'wifi'), `此 SIM 卡不支持软件 IMS。${reason}`)
})

test('mode radio buttons use the group change event, not clicks on disabled labels', () => {
  const source = readFileSync(new URL('../src/views/Phone.vue', import.meta.url), 'utf8')
  const group = source.match(/<el-radio-group[\s\S]*?<\/el-radio-group>/)![0]
  assert.match(group, /@change="changePhoneMode"/)
  assert.doesNotMatch(group, /@click/)
  assert.equal((group.match(/:disabled="!!phoneModeDisabledReason/g) || []).length, 4)
  assert.doesNotMatch(source, /mode = 'volte'/)
})

test('every RF mode warns before switching, including overseas cards and retries', () => {
  for (const mode of ['cellular', 'volte', 'modem_voice']) {
    const warning = phoneModeWarning(device(), mode)!
    assert.match(warning.message, /wwan0/)
    assert.match(warning.message, /境外 SIM 卡/)
    assert.match(warning.message, /漫游费用/)
    assert.match(warning.message, /退出飞行模式/)
    assert.match(warning.message, /WiFi calling/)
    assert.ok(phoneModeWarning({ ...device(), phone_mode: mode }, mode))
  }
  assert.match(phoneModeWarning(device(), 'modem_voice')!.message, /尝试开启 ADB/)
  assert.match(phoneModeWarning(device(), 'modem_voice')!.message, /无通话时会自动重启目标模组一次/)
  assert.match(phoneModeWarning(device(), 'modem_voice')!.message, /失败不反复重启/)
  assert.equal(phoneModeWarning(device(), 'wifi'), undefined)
  for (const region of ['CN', undefined]) {
    const warning = phoneModeWarning({ ...device(), phone_region: region }, 'volte')!
    assert.doesNotMatch(warning.message, /这是一张境外/)
    assert.match(warning.message, /归属地以外/)
  }
})

test('cancel and close never apply a mode change; unexpected errors remain visible', async () => {
  let applied = false
  for (const action of ['cancel', 'close']) {
    const result = await confirmPhoneModeChange({
      target: device(), mode: 'modem_voice', current: device, hasCall: () => false,
      confirm: async () => { throw action }, apply: async () => { applied = true }
    })
    assert.equal(result, false)
    assert.equal(applied, false)
  }
  await assert.rejects(confirmPhoneModeChange({
    target: device(), mode: 'modem_voice', current: device, hasCall: () => false,
    confirm: async () => { throw new Error('dialog failed') }, apply: async () => { applied = true }
  }), /dialog failed/)
  assert.equal(applied, false)
})

test('waits for confirmation before applying exactly once', async () => {
  let confirm!: () => void
  let applied = 0
  const result = confirmPhoneModeChange({
    target: device(), mode: 'modem_voice', current: device, hasCall: () => false,
    confirm: () => new Promise<void>((resolve) => { confirm = resolve }),
    apply: async () => { applied++ }
  })
  assert.equal(applied, 0)
  confirm()
  assert.equal(await result, true)
  assert.equal(applied, 1)
})

test('device removal, SIM change, policy change and new calls invalidate confirmation', async () => {
  const changes: Array<Partial<PhoneDevice> | undefined> = [
    undefined, { id: 'wwan1' }, { iccid: 'card-b' }, { phone_mode: 'volte' },
    { rf_lock: 'locked' }, { software_ims_blocked: true }, { vowifi_enabled: false }
  ]
  let applied = false
  for (const change of changes) {
    let current: PhoneDevice | undefined = device()
    await assert.rejects(confirmPhoneModeChange({
      target: device(), mode: 'modem_voice', current: () => current, hasCall: () => false,
      confirm: async () => { current = change ? { ...device(), ...change } : undefined },
      apply: async () => { applied = true }
    }), /状态已变化/)
  }
  await assert.rejects(confirmPhoneModeChange({
    target: device(), mode: 'modem_voice', current: device, hasCall: () => true,
    confirm: async () => {}, apply: async () => { applied = true }
  }), /状态已变化/)
  assert.equal(applied, false)
})

test('returning to WiFi calling needs no RF confirmation and exposes API errors', async () => {
  let prompted = false
  const options = {
    target: device(), mode: 'wifi', current: device, hasCall: () => false,
    confirm: async () => { prompted = true }, apply: async () => {}
  }
  assert.equal(await confirmPhoneModeChange(options), true)
  assert.equal(prompted, false)
  await assert.rejects(confirmPhoneModeChange({ ...options,
    apply: async () => { throw new Error('API rejected') }
  }), /API rejected/)
})
