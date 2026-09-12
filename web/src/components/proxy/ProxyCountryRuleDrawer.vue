<script setup lang="ts">
import { computed } from 'vue'
import {
  Add24Regular,
  Delete24Regular,
  Earth24Regular,
  Info24Regular
} from '@vicons/fluent'
import type {
  UpstreamProxyCountry,
  UpstreamProxyCountryRule
} from '../../types/api'
import type { UpstreamProxyPresentation } from '../../utils/proxyPresentation'
import EmptyState from '../EmptyState.vue'
import ProxyStatusBadge from './ProxyStatusBadge.vue'

const props = defineProps<{
  availableCountries: readonly UpstreamProxyCountry[]
  rules: readonly UpstreamProxyCountryRule[]
  target: UpstreamProxyPresentation | null
}>()

const open = defineModel<boolean>({ required: true })
const selectedCountryCode = defineModel<string>('selectedCountryCode', { required: true })

defineEmits<{
  delete: [countryCode: string]
  save: []
}>()

const drawerTitle = computed(() => {
  const targetName = props.target?.name || '未选择代理'
  return `国家规则 — ${targetName}`
})

function formatCountryOption(country: UpstreamProxyCountry): string {
  const name = country.country_name || country.country_code
  const mccs = country.mccs?.length ? ` · MCC ${country.mccs.join('/')}` : ''
  return `${country.country_code} · ${name}${mccs}`
}
</script>

<template>
  <el-drawer
    v-model="open"
    class="proxy-country-drawer"
    :title="drawerTitle"
    size="min(92vw, 560px)"
  >
    <div class="proxy-rule-content">
      <section class="proxy-rule-intro" aria-labelledby="proxy-rule-summary-title">
        <header>
          <span aria-hidden="true"><Earth24Regular /></span>
          <div>
            <small>ROUTE TARGET</small>
            <h3 id="proxy-rule-summary-title">前置代理摘要</h3>
          </div>
        </header>
        <dl>
          <div><dt>代理名称</dt><dd>{{ target?.name || '不可用' }}</dd></div>
          <div><dt>地址（SOCKS5）</dt><dd><code>{{ target?.address || '不可用' }}</code></dd></div>
          <div>
            <dt>启用状态</dt>
            <dd><ProxyStatusBadge :label="target?.enabledLabel || '不可用'" :tone="target?.enabledTone || 'neutral'" /></dd>
          </div>
          <div><dt>认证状态</dt><dd>{{ target?.authenticationLabel || '不可用' }}</dd></div>
        </dl>
      </section>

      <form class="proxy-rule-composer" @submit.prevent="$emit('save')">
        <label for="proxy-country-rule-select">添加国家规则</label>
        <div>
          <el-select
            id="proxy-country-rule-select"
            v-model="selectedCountryCode"
            aria-label="选择国家或地区"
            class="proxy-country-select"
            filterable
            placeholder="选择国家 / 地区"
          >
            <el-option
              v-for="country in availableCountries"
              :key="country.country_code"
              :label="formatCountryOption(country)"
              :value="country.country_code"
            />
          </el-select>
          <el-button native-type="submit" type="primary" :disabled="!selectedCountryCode">
            <el-icon aria-hidden="true"><Add24Regular /></el-icon>
            添加规则
          </el-button>
        </div>
      </form>

      <section class="proxy-rule-list" aria-labelledby="proxy-rule-list-title">
        <header>
          <div>
            <small>COUNTRY ROUTES</small>
            <h3 id="proxy-rule-list-title">已路由到该代理</h3>
          </div>
          <strong>{{ rules.length }}</strong>
        </header>

        <EmptyState
          v-if="rules.length === 0"
          title="暂无国家规则"
          subtitle="未配置的国家会默认直连。同一国家可以绑到多条代理，开 VoWiFi 时优先选 UDP 正常的节点。"
        />

        <div v-else class="proxy-rule-table" role="table" aria-label="国家路由规则">
          <div class="proxy-rule-table-head" role="row">
            <span role="columnheader">国家 / 地区</span>
            <span role="columnheader">路由目标</span>
            <span role="columnheader">启用状态</span>
            <span role="columnheader"><span class="sr-only">操作</span></span>
          </div>
          <div v-for="rule in rules" :key="rule.country_code" class="proxy-rule-row" role="row">
            <span role="cell">
              <b>{{ rule.country_code }}</b>
              <small>{{ rule.country_name || rule.country_code }}</small>
              <code>MCC {{ rule.mccs.join('/') || '不可用' }}</code>
            </span>
            <span role="cell">
              <b>{{ target?.name || '不可用' }}</b>
              <small>前置代理</small>
            </span>
            <span role="cell">
              <ProxyStatusBadge :label="rule.enabled ? '已启用' : '已禁用'" :tone="rule.enabled ? 'success' : 'neutral'" />
            </span>
            <span role="cell">
              <button
                type="button"
                :aria-label="`删除 ${rule.country_code} 国家规则`"
                :title="`删除 ${rule.country_code} 国家规则`"
                @click="$emit('delete', rule.country_code)"
              >
                <Delete24Regular aria-hidden="true" />
              </button>
            </span>
          </div>
        </div>
      </section>

      <p class="proxy-rule-notice">
        <Info24Regular aria-hidden="true" />
        <span>规则按 SIM 归属 MCC 解析国家；没有配置规则的国家默认直连。规则变更后需要重启 VoWiFi 生效。</span>
      </p>
    </div>
  </el-drawer>
</template>

<style scoped>
.proxy-rule-content { display: grid; gap: 16px; padding-bottom: 16px; }
.proxy-rule-intro,
.proxy-rule-list { overflow: hidden; border: 1px solid var(--ui-border); border-radius: var(--ui-radius-lg); background: var(--ui-surface-strong); }
.proxy-rule-intro > header,
.proxy-rule-list > header { min-height: 64px; padding: 12px 14px; display: flex; align-items: center; gap: 10px; border-bottom: 1px solid var(--ui-border); background: color-mix(in srgb, var(--ui-primary) 4%, var(--ui-surface)); }
.proxy-rule-intro > header > span { width: 34px; height: 34px; display: grid; place-items: center; color: var(--ui-primary); }
.proxy-rule-intro svg { width: 21px; height: 21px; }
.proxy-rule-intro small,
.proxy-rule-list header small { color: var(--ui-primary); font: 700 var(--ui-font-caption) "v-mono", ui-monospace, monospace; letter-spacing: .11em; }
.proxy-rule-intro h3,
.proxy-rule-list h3 { margin: 3px 0 0; color: var(--ui-text); font-size: 15px; }
.proxy-rule-intro dl { margin: 0; display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); }
.proxy-rule-intro dl > div { min-width: 0; min-height: 68px; padding: 12px 14px; display: grid; align-content: center; gap: 7px; border-bottom: 1px solid var(--ui-border-muted); }
.proxy-rule-intro dl > div:nth-child(odd) { border-right: 1px solid var(--ui-border-muted); }
.proxy-rule-intro dl > div:nth-last-child(-n+2) { border-bottom: 0; }
.proxy-rule-intro dt { color: var(--ui-text-muted); font-size: var(--ui-font-caption); }
.proxy-rule-intro dd { min-width: 0; margin: 0; color: var(--ui-text); font-size: 12px; overflow-wrap: anywhere; }
.proxy-rule-intro code { font: var(--ui-font-body-sm)/1.5 "v-mono", ui-monospace, monospace; }
.proxy-rule-composer { padding: 14px; display: grid; gap: 8px; border: 1px solid var(--ui-border); border-radius: var(--ui-radius-lg); background: var(--ui-surface); }
.proxy-rule-composer > label { color: var(--ui-text); font-size: 12px; font-weight: 650; }
.proxy-rule-composer > div { display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 8px; }
.proxy-country-select { width: 100%; }
.proxy-rule-list > header { justify-content: space-between; }
.proxy-rule-list > header strong { min-width: 30px; padding: 4px 8px; border-radius: 999px; background: var(--ui-surface-muted); color: var(--ui-text); font: 12px "v-mono", ui-monospace, monospace; text-align: center; }
.proxy-rule-table-head,
.proxy-rule-row { display: grid; grid-template-columns: minmax(118px, 1.35fr) minmax(84px, .9fr) minmax(94px, .9fr) 36px; align-items: center; gap: 8px; }
.proxy-rule-table-head { min-height: 38px; padding: 0 12px; color: var(--ui-text-muted); font-size: var(--ui-font-caption); }
.proxy-rule-row { min-height: 78px; padding: 11px 12px; border-top: 1px solid var(--ui-border-muted); color: var(--ui-text); font-size: 12px; }
.proxy-rule-row > span { min-width: 0; }
.proxy-rule-row b,
.proxy-rule-row small,
.proxy-rule-row code { display: block; overflow-wrap: anywhere; }
.proxy-rule-row small { margin-top: 3px; color: var(--ui-text-muted); font-size: var(--ui-font-caption); }
.proxy-rule-row code { margin-top: 5px; color: var(--ui-text-muted); font: var(--ui-font-body-sm) "v-mono", ui-monospace, monospace; }
.proxy-rule-row button { width: 36px; height: 36px; display: grid; place-items: center; border: 1px solid transparent; border-radius: var(--ui-radius-sm); background: transparent; color: var(--ui-text-muted); cursor: pointer; }
.proxy-rule-row button:hover,
.proxy-rule-row button:focus-visible { border-color: var(--ui-border); background: var(--ui-surface-muted); color: var(--ui-danger); }
.proxy-rule-row button svg { width: 17px; height: 17px; }
.proxy-rule-notice { margin: 0; padding: 11px 13px; display: flex; align-items: flex-start; gap: 9px; border: 1px solid color-mix(in srgb, var(--ui-communication) 35%, var(--ui-border)); border-radius: var(--ui-radius-lg); background: color-mix(in srgb, var(--ui-communication) 6%, var(--ui-surface)); color: var(--ui-text-muted); font-size: var(--ui-font-body-sm); line-height: 1.55; }
.proxy-rule-notice svg { width: 17px; height: 17px; flex: 0 0 17px; color: var(--ui-communication); }

@media (max-width: 480px) {
  .proxy-rule-intro dl { grid-template-columns: minmax(0, 1fr); }
  .proxy-rule-intro dl > div { border-right: 0 !important; }
  .proxy-rule-intro dl > div:nth-last-child(2) { border-bottom: 1px solid var(--ui-border-muted); }
  .proxy-rule-composer > div { grid-template-columns: minmax(0, 1fr); }
  .proxy-rule-composer .el-button { min-height: 44px; }
  .proxy-rule-table-head { display: none; }
  .proxy-rule-row { grid-template-columns: minmax(0, 1fr) auto; gap: 12px; }
  .proxy-rule-row > span:nth-child(2),
  .proxy-rule-row > span:nth-child(3) { grid-column: 1; }
  .proxy-rule-row > span:last-child { grid-column: 2; grid-row: 1 / span 3; align-self: center; }
  .proxy-rule-row button { width: 44px; height: 44px; }
}
</style>
