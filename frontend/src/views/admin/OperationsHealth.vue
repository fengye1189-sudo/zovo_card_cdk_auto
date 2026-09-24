<template>
  <div class="space-y-5">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div><h1 class="text-2xl font-bold">运维中心</h1><p class="text-sm text-muted mt-2">查看订单、卡池、通知与备份。每分钟自动刷新。</p></div>
      <el-button :loading="loading" @click="load">刷新状态</el-button>
    </div>
    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon />
    <el-alert v-if="notice" :title="notice" type="success" :closable="false" show-icon />

    <template v-if="report">
      <el-alert v-if="report.restore_pending" title="恢复后核对尚未完成" description="请核对备份之后的付款、卡片成功次数和卡密状态。核对完成前，保持充值和自动资金操作暂停。" type="warning" :closable="false" show-icon />
      <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <div class="card"><p class="text-sm text-muted">数据库 / 后台任务</p><p class="text-xl font-semibold mt-2">{{ report.database_ok && report.heartbeat_ok ? '运行正常' : '需要检查' }}</p><p class="hint">最近心跳：{{ date(report.heartbeat) }}</p><p class="hint">数据库 {{ bytes(report.database_bytes) }}</p></div>
        <div class="card"><p class="text-sm text-muted">近 24 小时提交的订单</p><p class="text-2xl font-semibold mt-2">{{ report.submitted_24h }}</p><p class="hint">已成功 {{ report.completed_24h_cohort }} · 未完成 {{ report.failed_24h_cohort }}</p><p class="hint">当前完成比例 {{ successRate }}%</p></div>
        <div class="card"><p class="text-sm text-muted">处理中 / 待核对</p><p class="text-2xl font-semibold mt-2">{{ report.outstanding }}</p><p class="hint">超过 30 分钟 {{ report.overdue }} · 需人工核对 {{ report.review }}</p><router-link to="/ops/orders" class="text-primary text-sm">查看订单</router-link></div>
        <div class="card"><p class="text-sm text-muted">支付卡池</p><p class="text-2xl font-semibold mt-2">{{ report.candidate_cards }} <span class="text-sm font-normal">张本地候选卡</span></p><p class="hint">池内 {{ report.selected_cards }} · 暂停或待销 {{ report.capped_cards }}</p><p class="hint">符合实时付款条件的卡随机使用；无成功次数上限和 2 天冷静期</p></div>
      </div>
      <p class="text-sm text-muted">近 24 小时订单按后台首次跟进时间统计，完成比例为这批订单当前已完成的比例；未结束的订单会继续变化。</p>

      <div class="grid gap-4 lg:grid-cols-2">
        <section class="card space-y-3">
          <h2 class="text-lg font-semibold">任务与通知</h2>
          <dl class="health-list">
            <div><dt>充值通道</dt><dd>{{ report.channel_enabled ? '已开启' : '已关闭' }}</dd></div>
            <div><dt>自动化</dt><dd>{{ report.paused ? '新操作已暂停' : report.sync_enabled ? '按规则运行' : '后台查单已关闭' }}</dd></div>
            <div><dt>待核对资金操作</dt><dd>{{ report.money_holds }}</dd></div>
            <div><dt>外部提醒</dt><dd>{{ report.notifications_enabled ? channelName : '未启用有效通知' }}</dd></div>
            <div><dt>待发送</dt><dd>{{ report.notifications_pending }}</dd></div>
            <div><dt>近 24 小时失败 / 结果不明</dt><dd>{{ report.notifications_failed }} / {{ report.notifications_unknown }}</dd></div>
            <div><dt>最近通知平台接收</dt><dd>{{ date(report.last_notification_accepted) }}</dd></div>
          </dl>
          <p class="hint">通知平台接收不代表用户已阅读。外部提醒在「通知与回复」页面配置；站点整体停机需要独立站外监控。</p>
          <router-link to="/ops/messages" class="btn-secondary inline-block">查看通知配置</router-link>
        </section>
        <section class="card space-y-3">
          <h2 class="text-lg font-semibold">最近备份</h2>
          <p class="text-xl font-semibold">{{ date(report.last_backup_success) }}</p>
          <el-alert v-if="report.last_backup_error" :title="report.last_backup_error" type="warning" :closable="false" />
          <p class="hint">每天自动备份并验证。保留最近 {{ dailyRetention }} 份自动备份、{{ manualRetention }} 份手动备份，总量上限 {{ bytes(storageLimit) }}，超出时清理较旧备份。</p>
          <p class="hint">备份包含卡密记录、订单、接入设置和审计数据，仅管理员可下载。下载至另一台设备，可在原服务器磁盘损坏后使用。</p>
          <el-button type="primary" :loading="busy === 'create'" :disabled="!!busy" @click="createBackup">立即备份并校验</el-button>
        </section>
      </div>

      <section class="card space-y-3">
        <h2 class="text-lg font-semibold">异常提醒 <span class="text-sm font-normal">{{ report.active_alerts }} 项</span></h2>
        <el-empty v-if="!report.alerts.length" description="目前没有待处理提醒" :image-size="60" />
        <el-alert v-for="item in report.alerts" :key="item.key" :title="item.message" :description="date(item.updated_at)" type="warning" :closable="false" show-icon />
      </section>

      <section class="card space-y-3">
        <h2 class="text-lg font-semibold">每张卡的使用次数</h2>
        <p class="hint">表中次数只统计当前阶段权威确认成功的 Plus；Go 不计次。首次达到上限后暂停 2 天，再进入最后 2 次阶段；出现 1 次上游明确拒付也会待销。</p>
        <el-table :data="report.cards" empty-text="尚未设置支付白名单">
          <el-table-column prop="id" label="卡片 ID" min-width="120" />
          <el-table-column label="类型" min-width="110"><template #default="{ row }">{{ row.kind==='pro' ? 'Pro 入池卡' : '普通池卡' }}</template></el-table-column>
          <el-table-column label="本周期 Plus" min-width="120"><template #default="{ row }">{{ row.completed }} / {{ row.limit }}</template></el-table-column>
          <el-table-column prop="remaining" label="剩余次数" min-width="100" />
          <el-table-column prop="outstanding" label="处理中 / 待核对" min-width="145" />
          <el-table-column prop="declines" label="明确拒付" min-width="100" />
          <el-table-column label="本地状态" min-width="210"><template #default="{row}"><el-tag :type="row.capped ? 'info' : row.outstanding ? 'warning' : 'success'">{{ healthCardState(row) }}</el-tag></template></el-table-column>
        </el-table>
      </section>
    </template>

    <section class="card space-y-3">
      <h2 class="text-lg font-semibold">备份与恢复检查</h2>
      <p class="hint">“恢复检查”会把备份还原为独立的临时数据库，检查完整性、设置和订单记录。检查不会覆盖当前数据。实际恢复通过服务器恢复工具先生成暂停充值的待恢复文件，再核对备份之后的订单。</p>
      <el-table :data="backups" empty-text="尚无已完成的备份；可立即创建一份">
        <el-table-column label="创建时间" min-width="190"><template #default="{row}">{{ date(row.created_at) }}</template></el-table-column>
        <el-table-column label="类型" width="90"><template #default="{row}">{{ row.kind === 'daily' ? '每日自动' : '手动' }}</template></el-table-column>
        <el-table-column label="大小" width="110"><template #default="{row}">{{ bytes(row.bytes) }}</template></el-table-column>
        <el-table-column label="内容" min-width="220"><template #default="{row}">卡密 {{ row.codes }} · 已成功 {{ row.completed }}<br><span class="hint">待核对订单 {{ row.unresolved }} · 资金锁定 {{ row.money_holds }}</span></template></el-table-column>
        <el-table-column label="操作" min-width="250"><template #default="{row}"><div class="flex flex-wrap gap-2"><el-button size="small" :loading="busy === 'verify:'+row.name" :disabled="!!busy" @click="verifyBackup(row)">恢复检查</el-button><el-button size="small" :loading="busy === 'download:'+row.name" :disabled="!!busy" @click="downloadBackup(row)">下载备份</el-button><el-button size="small" @click="downloadManifest(row)">校验信息</el-button></div></template></el-table-column>
      </el-table>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { authFetch } from '../../lib/api'

type Backup = { name:string; kind:string; created_at:number; verified_at:number; bytes:number; sha256:string; codes:number; completed:number; unresolved:number; money_holds:number }
type Health = {
  database_ok:boolean; database_bytes:number; heartbeat:number; heartbeat_ok:boolean; sync_enabled:boolean; paused:boolean; channel_enabled:boolean; restore_pending:boolean;
  submitted_24h:number; completed_24h_cohort:number; failed_24h_cohort:number; outstanding:number; review:number; overdue:number; money_holds:number;
  selected_cards:number; capped_cards:number; candidate_cards:number; payment_limit:number; cards:{id:number;completed:number;remaining:number;limit:number;kind:string;cooldown_until:number;outstanding:number;capped:boolean;phase:string;retire_state:string;declines:number}[];
  notifications_enabled:boolean; notification_channel:string; notifications_pending:number; notifications_failed:number; notifications_unknown:number; last_notification_accepted:number;
  last_backup_success:number; last_backup_error:string; active_alerts:number; alerts:{key:string;message:string;updated_at:number}[];
}
const report = ref<Health|null>(null), backups = ref<Backup[]>([]), error = ref(''), notice = ref(''), busy = ref(''), loading = ref(false)
const dailyRetention = ref(7), manualRetention = ref(3), storageLimit = ref(768*1024*1024)
const successRate = computed(() => report.value?.submitted_24h ? ((report.value.completed_24h_cohort/report.value.submitted_24h)*100).toFixed(1) : '0.0')
const channelName = computed(() => report.value?.notification_channel === 'telegram' ? 'Telegram 已启用' : '邮件已启用')
const apiBase = '/api/v1/admin/operations'
function date(value:number) { return value ? new Date(value*1000).toLocaleString() : '暂无记录' }
function healthCardState(row:Health['cards'][number]) { if(row.retire_state&&row.retire_state!=='active')return ({queued:'等待安全销卡',inflight:'正在销卡',unknown:'销卡待核对',closed:'已销卡'} as Record<string,string>)[row.retire_state]||'停止使用';if(row.cooldown_until)return '冷静至 '+date(row.cooldown_until);if(row.outstanding)return '等待订单核对';return row.phase==='final'?'最后 2 次阶段':'可候选，等待卡台检查' }
function bytes(value:number) { return value >= 1024*1024 ? (value/(1024*1024)).toFixed(1)+' MB' : (value/1024).toFixed(1)+' KB' }
async function api(path:string, method='GET') {
  const response = await authFetch(apiBase+path,{method})
  const data = await response.json()
  if (!response.ok) throw new Error(data.error || '请求未完成，请稍后重试')
  return data
}
async function load() {
  if (loading.value) return
  loading.value = true
  try {
    const [health, archive] = await Promise.all([api('/health'),api('/backups')])
    report.value = health; backups.value = archive.items
    dailyRetention.value = archive.daily_retention; manualRetention.value = archive.manual_retention; storageLimit.value = archive.storage_limit_bytes
    error.value = ''
  } catch (e:any) { error.value = e.message } finally { loading.value = false }
}
async function createBackup() {
  if (busy.value) return
  busy.value = 'create'; error.value = ''; notice.value = ''
  try { const item = await api('/backups','POST'); notice.value = `备份已完成并通过独立恢复检查，包含 ${item.codes} 条卡密记录。`; await load() }
  catch(e:any) { error.value = e.message } finally { busy.value = '' }
}
async function verifyBackup(item:Backup) {
  if (busy.value) return
  busy.value = 'verify:'+item.name; error.value = ''; notice.value = ''
  try { const result = await api('/backups/'+encodeURIComponent(item.name)+'/verify','POST'); notice.value = result.message }
  catch(e:any) { error.value = e.message } finally { busy.value = '' }
}
function saveFile(blob:Blob,name:string) {
  const url = URL.createObjectURL(blob), link = document.createElement('a')
  link.href = url; link.download = name; document.body.appendChild(link); link.click(); link.remove()
  setTimeout(() => URL.revokeObjectURL(url),1000)
}
async function downloadBackup(item:Backup) {
  if (busy.value) return
  busy.value = 'download:'+item.name; error.value = ''
  try {
    const response = await authFetch(apiBase+'/backups/'+encodeURIComponent(item.name)+'/download')
    if (!response.ok) { const data = await response.json(); throw new Error(data.error || '下载未完成') }
    saveFile(await response.blob(),item.name)
    notice.value = '已开始下载备份。需要服务器恢复时，可同时下载这一行的“校验信息”。'
  } catch(e:any) { error.value = e.message } finally { busy.value = '' }
}
function downloadManifest(item:Backup) { saveFile(new Blob([JSON.stringify(item,null,2)],{type:'application/json'}),item.name+'.json') }
let timer:ReturnType<typeof setInterval>|undefined
onMounted(() => { void load(); timer = setInterval(() => { if (!document.hidden && !busy.value) void load() },60000) })
onUnmounted(() => { if (timer) clearInterval(timer) })
</script>

<style scoped>
.hint { font-size:14px; line-height:1.6; color:var(--el-text-color-secondary); margin-top:6px; }
.health-list>div { display:flex; align-items:start; justify-content:space-between; gap:16px; padding:8px 0; border-bottom:1px solid var(--el-border-color-lighter); }
.health-list dt { color:var(--el-text-color-secondary); }
.health-list dd { text-align:right; overflow-wrap:anywhere; }
</style>
