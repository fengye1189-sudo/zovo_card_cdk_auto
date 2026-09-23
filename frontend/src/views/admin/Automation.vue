<template>
  <div class="space-y-4">
    <div class="flex flex-wrap gap-3 justify-between items-center"><div><h1 class="text-2xl font-bold">自动化</h1><p class="text-sm text-muted mt-2">订单查询在服务器运行，关闭此页面仍会继续。暂停只阻止新的操作，不撤销已经发出的请求。</p></div><el-button type="danger" :loading="saving" @click="pause">暂停新的付款与自动操作</el-button></div>
    <el-alert v-if="error" :title="error" type="error" :closable="false" />
    <el-alert v-if="draftNotice" :title="draftNotice" type="warning" :closable="false">
      <template #default><el-button text type="warning" @click="discardDraft">放弃草稿并恢复服务器已保存的规则</el-button></template>
    </el-alert>
<div class="grid gap-3 sm:grid-cols-3"><div class="card"><p class="text-sm text-muted">后台心跳</p><p class="text-lg font-semibold mt-2">{{ health }}</p><p class="text-sm">{{ date(report.heartbeat) }}</p></div><div class="card"><p class="text-sm text-muted">新操作状态</p><p class="text-lg font-semibold mt-2">{{ report.blocked ? '已暂停 / 等待资金核对' : '按已保存规则执行' }}</p><p class="text-sm">充值还受「卡密管理」的开关和限额控制</p></div><div class="card"><p class="text-sm text-muted">待处理提醒</p><p class="text-2xl font-semibold mt-2">{{ report.alerts.length }}</p><p class="text-sm">外部提醒请在「通知与回复」中配置并启用</p></div></div>
    <div class="card space-y-4">
      <div class="flex flex-wrap items-center justify-between gap-2"><h2 class="text-xl font-bold">运行规则</h2><p role="status" class="text-sm">{{ dirty ? '有未保存的修改' : '已读取服务器规则' }} · 版本 {{ version }}<span v-if="savedAt"> · {{ savedAt }}</span></p></div>
      <el-form label-position="top" :disabled="!ready || saving">
        <div class="grid gap-4 sm:grid-cols-2">
          <el-form-item label="后台持续查单"><el-switch v-model="settings.sync_enabled" /><p class="hint">每轮最多跟进 10 笔；异常订单降低查询频率，重启后从记录继续。</p></el-form-item>
          <el-form-item label="暂停新的操作"><el-switch v-model="settings.paused" /><p class="hint">暂停付款、自动补款、开卡、销卡和自动取消续费；保留查单与到账核查。</p></el-form-item>
          <el-form-item label="自动处理取消续费"><el-switch v-model="settings.renewal_enabled" /><p class="hint">仅本站成功订单，且上游显示待确认或需复查；每笔最多尝试 3 次，至少间隔 15 分钟。结果不明不重试。</p></el-form-item>
        </div>
        <el-divider />
        <h3 class="text-lg font-semibold mb-4">资金自动化（从 Zovo 平台余额转入卡，不是自动充 USDT）</h3>
        <el-alert title="资金开关默认关闭。只管理你勾选的卡及明确授权自动开出的卡；拒付后不换卡重付。资金预算仅覆盖自动开卡和补款，不是订阅扣款总预算。" type="warning" :closable="false" class="mb-4" />
        <p class="text-sm text-muted mb-4">GPTPRO5x卡冲升级与 Pro 20X 商品在 <router-link class="app-link" to="/ops/products">商品与套餐</router-link>管理。客户确认兑换后分别为专卡初充 $120 / $150；两者共用过去 24 小时预算、每日开卡数量及平台余额保留额，普通卡的单次 $22 资金上限不用于 Pro 专卡。未确认到账不会提交升级付款。</p>
        <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          <el-form-item label="自动补充已勾选卡的余额"><el-switch v-model="settings.topup_enabled" /><p class="hint">付款结果已确认且余额低于阈值时可再次补款；有在途或待核对订单、资金未确认或处于 2 天冷静期时不补款。仍受每日预算和单次上限限制；Pro 成功入池卡不会自动补款。</p></el-form-item>
          <el-form-item label="达到条件后自动销卡并退回余额"><el-switch v-model="settings.retire_enabled" /><p class="hint">普通阶段用满后冷静 2 天，再允许最后 2 次成功付款并销卡；出现 1 次上游明确拒付也销卡。销卡前会确认没有在途订单或待核对资金，结果不明时不会重复提交。</p></el-form-item>
          <el-form-item label="可用卡剩余 2 张时提前开卡"><el-switch v-model="settings.open_enabled" /><p class="hint">可用卡剩余 2 张时自动补开 1 张，恢复到 3 张；到账核查期间或已有自动开卡尚未被勾选时，不会重复开卡。仍受预算和每日开卡上限限制。</p></el-form-item>
          <el-form-item label="自动选择高成功率卡头"><el-switch v-model="settings.auto_product_enabled" /><p class="hint">卡头先分为“星链卡”和“渠道1”，再按具体 BIN 统计近 30 天成功率。自动开卡每 5 次有 4 次选择统计分最高的卡头，1 次兼顾两种类型并优先试用新出现或样本最少的合格卡头。</p></el-form-item>
          <el-form-item label="自动开出的卡允许加入支付名单"><el-switch v-model="settings.enroll_created_cards" /><p class="hint">仅适用于本站自动开卡，确认到账后加入；其他新卡不会自动勾选。</p></el-form-item>
          <el-form-item v-for="field in moneyFields" :key="field.key" :label="field.label+'（USD）'"><el-input-number :model-value="(settings[field.key] || 0)/100" @update:model-value="value=>settings[field.key]=Math.round((value || 0)*100)" :min="0" :max="1000000" :precision="2" :step="1" /><p class="hint">{{ field.hint }}</p></el-form-item>
          <el-form-item label="24 小时最多自动开卡数量"><el-input-number v-model="settings.daily_open_limit" :min="0" :max="20" :precision="0" /></el-form-item>
          <el-form-item label="固定卡头产品码（关闭随机时使用）"><el-input v-model="settings.product_code" :disabled="settings.auto_product_enabled" placeholder="例如 P5378OX，以 Zovo 当前产品为准" maxlength="80" /></el-form-item>
          <el-form-item label="持卡人名"><el-input v-model="settings.first_name" placeholder="按发卡平台要求填写" maxlength="80" /></el-form-item>
          <el-form-item label="持卡人姓"><el-input v-model="settings.last_name" placeholder="按发卡平台要求填写" maxlength="80" /></el-form-item>
        </div>
        <div class="space-y-2">
          <p v-if="error" data-save-error class="rounded-lg border border-red-300 bg-red-50 px-3 py-2 text-sm text-red-700">{{ error }}</p>
          <el-button type="primary" :loading="saving" @click="save">确认并保存规则</el-button>
        </div>
      </el-form>
    </div>
    <div class="card space-y-3"><div class="flex justify-between gap-3"><h2 class="text-xl font-bold">异常提醒</h2><el-button :loading="loadingStatus" @click="loadStatus">刷新状态</el-button></div><el-empty v-if="!report.alerts.length" description="目前没有待处理提醒" :image-size="60" /><el-alert v-for="alert in report.alerts" :key="alert.key" :title="(alert.local_id?'卡密 #'+alert.local_id+'：':'')+alert.message" :description="date(alert.updated_at)" type="warning" :closable="false" show-icon /></div>
    <div class="card space-y-3"><h2 class="text-xl font-bold">卡头表现（近 30 天）</h2><p class="text-sm text-muted">每笔明确成功和明确拒付都会按曼谷日期统计；星链卡与渠道1分别计算，同类型、同一卡头即使产品码变化也共享表现。</p><el-table :data="report.product_stats" empty-text="暂无可统计的付款结果"><el-table-column prop="card_type_label" label="类型" width="100" /><el-table-column prop="product_code" label="产品" min-width="120" /><el-table-column prop="bin" label="卡头" min-width="120" /><el-table-column prop="successes" label="成功" width="80" /><el-table-column prop="declines" label="拒付" width="80" /><el-table-column label="成功率" width="100"><template #default="{row}">{{ row.attempts ? (row.success_rate*100).toFixed(1)+'%' : '暂无' }}</template></el-table-column><el-table-column prop="latest_day" label="最近统计日" min-width="120" /></el-table></div>
    <div class="card space-y-3"><h2 class="text-xl font-bold">卡片生命周期</h2><el-table :data="report.card_lifecycle" empty-text="暂无卡片记录"><el-table-column prop="card_id" label="卡片 ID" min-width="100" /><el-table-column label="类型 / 产品 / 卡头" min-width="240"><template #default="{row}">{{ row.card_type_label || '待识别' }} / {{ row.product_code || '待同步' }}<span v-if="row.bin"> / {{ row.bin }}</span></template></el-table-column><el-table-column label="阶段" min-width="125"><template #default="{row}">{{ cyclePhase(row) }}</template></el-table-column><el-table-column label="成功次数" width="105"><template #default="{row}">{{ row.success_count }} / {{ row.success_limit }}</template></el-table-column><el-table-column prop="decline_count" label="明确拒付" width="100" /><el-table-column label="销卡状态" min-width="150"><template #default="{row}">{{ retireState(row.retire_state) }}</template></el-table-column><el-table-column label="冷静期结束" min-width="190"><template #default="{row}">{{ row.cooldown_until ? date(row.cooldown_until) : '—' }}</template></el-table-column></el-table></div>
    <div class="card space-y-3"><h2 class="text-xl font-bold">后台跟进记录（最近 100 笔）</h2><el-table :data="report.orders" empty-text="暂无需要跟进的本站订单"><el-table-column prop="local_id" label="卡密 ID" width="95" /><el-table-column prop="upstream_id" label="上游订单" min-width="110" /><el-table-column prop="status" label="上游状态" min-width="135" /><el-table-column label="续费状态" min-width="130"><template #default="{row}">{{ renewal(row.renewal) }}</template></el-table-column><el-table-column label="上次查询" min-width="190"><template #default="{row}">{{ date(row.checked_at) }}</template></el-table-column><el-table-column label="下次查询" min-width="190"><template #default="{row}">{{ date(row.next_check) }}</template></el-table-column></el-table><router-link class="btn-secondary inline-block" to="/ops/orders">查看充值订单详情</router-link></div>
    <div class="card space-y-3"><h2 class="text-xl font-bold">自动资金记录（最近 100 笔）</h2><p class="text-sm text-muted">“余额已核查”仅表示卡内看到了预期余额，不等同于完整资金流水对账。结果不明的记录不自动解除锁定或退回预算。</p><el-table :data="report.operations" empty-text="未执行自动资金操作"><el-table-column prop="id" label="本站操作编号" min-width="260" /><el-table-column label="操作" width="120"><template #default="{row}">{{ row.action==='pro5x_reserve_open'?'5X 预备专卡':row.action==='pro5x_pay_fee'?'5X 服务费':row.action==='pro_open'?'Pro 专卡':row.action==='open'?'自动开卡':'自动补款' }}</template></el-table-column><el-table-column label="卡片 ID" min-width="100"><template #default="{row}">{{ row.result_card_id || row.card_id || '待确认' }}</template></el-table-column><el-table-column label="充值额 / 预留费用" min-width="180"><template #default="{row}">${{ (row.amount_minor/100).toFixed(2) }} / ${{ (row.reserved_minor/100).toFixed(2) }}</template></el-table-column><el-table-column label="状态" min-width="150"><template #default="{row}">{{ moneyState(row.state) }}</template></el-table-column><el-table-column label="时间" min-width="190"><template #default="{row}">{{ date(row.created_at) }}</template></el-table-column></el-table></div>
    <div class="card flex flex-wrap gap-3"><router-link to="/ops/finance" class="btn-secondary">财务流水与核对</router-link><router-link to="/ops/messages" class="btn-secondary">邮箱 / 手机通知设置</router-link><router-link to="/ops/health" class="btn-secondary">运行状态与备份</router-link></div>
  </div>
</template>
<script setup lang="ts">
import {computed,onMounted,onUnmounted,reactive,ref,watch} from 'vue'
import {authFetch} from '../../lib/api'
import {dialog} from '../../lib/dialog'
const settings=reactive<Record<string,any>>({}),version=ref(0),ready=ref(false),saving=ref(false),loadingStatus=ref(false),error=ref(''),clock=ref(Date.now())
const draftKey='maplepass_cdk_automation_draft_v1',draftNotice=ref('')
const serverSnapshot=ref(''),savedAt=ref('')
const dirty=computed(()=>ready.value&&JSON.stringify(settings)!==serverSnapshot.value)
const report=reactive<{heartbeat:number;blocked:boolean;alerts:any[];orders:any[];operations:any[];card_lifecycle:any[];product_stats:any[]}>({heartbeat:0,blocked:true,alerts:[],orders:[],operations:[],card_lifecycle:[],product_stats:[]})
const moneyFields=[
 {key:'daily_budget_minor',label:'过去 24 小时总预算',hint:'包括自动开卡费、卡充值额、充值手续费和 Pro 专卡服务费；不包括订阅扣款。0 不允许资金操作。'},
 {key:'max_operation_minor',label:'普通卡单次资金操作最高总费用',hint:'普通开卡和补款含本金及手续费，不得超过总预算；Pro 专卡走单独的计划金额与共享 24 小时预算。'},
 {key:'wallet_floor_minor',label:'平台可消费余额最低保留额',hint:'在平台自身保证金要求之外，再额外保留这笔可消费余额。'},
 {key:'threshold_minor',label:'卡余额低于此值时补款',hint:'只补已勾选且符合上游用卡规则的卡。'},
 {key:'target_minor',label:'每次补到的卡余额',hint:'必须高于补款触发值，并满足产品最低充值额。'},
 {key:'card_ceiling_minor',label:'卡内余额上限',hint:'按操作前实时余额核查；其他渠道同时操作可能改变余额。'},
 {key:'init_amount_minor',label:'自动开卡初始充值额',hint:'另加产品开卡费，必须符合产品最低金额。'}]
const health=computed(()=>report.heartbeat && clock.value/1000-report.heartbeat<600?'后台正在运行':'暂无近期心跳，请刷新检查')
async function api(path:string,options:RequestInit={}){const r=await authFetch('/api/v1/admin/automation/'+path,options);const d=await r.json();if(!r.ok)throw new Error(d.error || '请求失败');return d}
function saveDraft(){if(!ready.value||saving.value)return;try{if(dirty.value)localStorage.setItem(draftKey,JSON.stringify({version:version.value,settings}));else localStorage.removeItem(draftKey)}catch{}}
watch(settings,saveDraft,{deep:true,flush:'sync'})
function discardDraft(){try{localStorage.removeItem(draftKey)}catch{};window.location.reload()}
async function loadSettings(){ready.value=false;const d=await api('settings');Object.assign(settings,d.settings);version.value=d.version;serverSnapshot.value=JSON.stringify(settings)
 try{const raw=localStorage.getItem(draftKey);if(raw){const draft=JSON.parse(raw);if(draft&&draft.version===d.version&&draft.settings&&typeof draft.settings==='object'){Object.assign(settings,draft.settings);draftNotice.value='已恢复本机未保存的修改；服务器仍按已保存规则执行。'}else{draftNotice.value='服务器规则已更新，当前显示最新保存值；旧草稿未覆盖服务器。';localStorage.removeItem(draftKey)}}}catch{}
 ready.value=true}
async function loadStatus(){if(loadingStatus.value)return;loadingStatus.value=true;try{Object.assign(report,await api('status'))}catch(e:any){error.value=e.message}finally{loadingStatus.value=false;clock.value=Date.now()}}
function validateSettings(){
 const n=(key:string)=>Number(settings[key]||0)
 for(const key of ['daily_budget_minor','max_operation_minor','wallet_floor_minor','threshold_minor','target_minor','card_ceiling_minor','init_amount_minor','daily_open_limit']){if(!Number.isFinite(n(key))||n(key)<0||n(key)>100000000)return '存在不正确的金额或数量，请检查后再保存。'}
 if((settings.renewal_enabled||settings.topup_enabled||settings.open_enabled||settings.retire_enabled)&&!settings.sync_enabled)return '开启自动取消续费、自动补款、自动开卡或自动销卡前，必须同时开启“后台持续查单”。'
 if(settings.topup_enabled||settings.open_enabled){if(n('daily_budget_minor')<=0||n('max_operation_minor')<=0||n('max_operation_minor')>n('daily_budget_minor')||n('card_ceiling_minor')<=0)return '开启资金自动化时，请填写正数的 24 小时总预算、单次上限和卡内余额上限；单次上限不能超过总预算。'}
 if(settings.topup_enabled&&(n('threshold_minor')<=0||n('target_minor')<=n('threshold_minor')||n('target_minor')>n('card_ceiling_minor')))return '自动补款要求：触发余额大于 0、补到的余额高于触发值，且不超过卡内余额上限。'
 if(settings.open_enabled&&(n('init_amount_minor')<=0||n('init_amount_minor')>n('card_ceiling_minor')||n('daily_open_limit')<1||n('daily_open_limit')>20||(!settings.auto_product_enabled&&!String(settings.product_code||'').trim())||!String(settings.first_name||'').trim()||!String(settings.last_name||'').trim()))return '自动开卡要求：初始充值额、每日数量、持卡人名和姓均须完整填写；关闭随机卡头时还必须填写产品码，且初始充值额不能超过卡内余额上限。'
 return ''
}
function showSaveError(message:string){error.value=message;requestAnimationFrame(()=>document.querySelector('[data-save-error]')?.scrollIntoView({behavior:'smooth',block:'center'}))}
async function save(){if(!ready.value||saving.value)return;const invalid=validateSettings();if(invalid){showSaveError(invalid);return}error.value='';saving.value=true;try{
 const mode=`后台查单：${settings.sync_enabled?'开':'关'}；自动取消续费：${settings.renewal_enabled?'开':'关'}；自动补款：${settings.topup_enabled?'开':'关'}；自动销卡：${settings.retire_enabled?'开':'关'}；自动开卡：${settings.open_enabled?'开':'关'}；高成功率卡头：${settings.auto_product_enabled?'开':'关'}；总暂停：${settings.paused?'开':'关'}。\n可用卡剩余 2 张时补开 1 张；24 小时自动资金预算 $${((settings.daily_budget_minor||0)/100).toFixed(2)}，单次上限 $${((settings.max_operation_minor||0)/100).toFixed(2)}。\n保存后服务器按规则执行，可能消耗 Zovo 余额；达到规则会永久销卡并由 Zovo 退回卡内余额。已发出的请求不会被撤销。确认保存？`
 if(!await dialog.confirm(mode,{title:'确认自动化授权',okText:'确认规则并保存',cancelText:'暂不保存',danger:true}))return
 const d=await api('settings',{method:'PUT',body:JSON.stringify({settings,version:version.value,confirmed:true})});Object.assign(settings,d.settings);version.value=d.version;serverSnapshot.value=JSON.stringify(settings);savedAt.value='保存于 '+new Date().toLocaleTimeString();try{localStorage.removeItem(draftKey)}catch{};draftNotice.value='';dialog.toast('规则已保存');await loadStatus()
 }catch(e:any){showSaveError('规则尚未保存：'+e.message)}finally{saving.value=false}}
async function pause(){if(saving.value)return;saving.value=true;try{const d=await api('pause',{method:'POST'});Object.assign(settings,d.settings);version.value=d.version;serverSnapshot.value=JSON.stringify(settings);try{localStorage.removeItem(draftKey)}catch{};draftNotice.value='';ready.value=true;dialog.toast('已暂停新的操作；已发出的请求仍需核对','info');await loadStatus()}catch(e:any){error.value=e.message}finally{saving.value=false}}
function date(v:number){return v>0?new Date(v*1000).toLocaleString():'尚未查询'}
function renewal(v:string){return ({success:'续费已取消',pending:'取消待确认',warning:'需要复查',not_requested:'尚未请求'} as Record<string,string>)[v]||'未返回'}
function moneyState(v:string){return ({inflight:'请求处理中',pending:'待核查余额',unknown:'结果不明，已锁定',balance_verified:'余额已核查'} as Record<string,string>)[v]||v}
function cyclePhase(row:any){if(row.retire_state==='closed')return '已结束';if(row.phase==='final')return '最后 2 次';if(row.cooldown_until)return '冷静期';return '普通阶段'}
function retireState(v:string){return ({active:'使用中',queued:'等待安全销卡',inflight:'正在销卡',unknown:'销卡待人工核对',closed:'已销卡'} as Record<string,string>)[v]||v}
let timer:ReturnType<typeof setInterval>|undefined
onMounted(()=>{loadSettings().then(loadStatus).catch((e:any)=>{error.value=e.message});timer=setInterval(()=>{if(!document.hidden)loadStatus()},30000)})
onUnmounted(()=>{if(timer)clearInterval(timer)})
</script>
<style scoped>.hint{font-size:14px;line-height:1.6;color:var(--el-text-color-secondary);margin-top:8px;flex-basis:100%}</style>
