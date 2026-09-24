<template>
  <div class="space-y-6">
    <h1 class="text-2xl font-bold text-ink">卡密管理</h1>
    <p class="text-muted">本站独立生成卡密，不购买 Zovo CDK。生成不扣费，兑换时才调用直充通道。</p>
    <div v-if="error" class="alert alert-error" role="alert">{{ error }}</div>
    <section class="card space-y-2"><h2 class="text-xl font-bold">客户换码备用库存</h2><p class="text-sm text-muted">每个启用商品保持 5 张备用码，换出后自动补足。只允许未使用、未提交付款的同产品卡密更换，原到期日不变。</p><p v-for="item in reserveStock" :key="item.product_id">{{ item.name }}：{{ item.count }} / {{ item.target }} 张</p></section>
    <section v-if="auth.can('cdks.issue')" class="card space-y-4">
      <h2 class="text-xl font-bold">生成商品卡密</h2>
      <label class="block">商品<select v-model="productID" class="input mt-2" @change="selectProduct"><option v-for="product in products.filter(p=>p.enabled)" :key="product.id" :value="product.id">{{ product.name }} · {{ localPlanLabel(product.plan) }}</option></select></label>
      <router-link class="app-link" to="/ops/products">管理商品与有效期</router-link>
      <div class="grid sm:grid-cols-2 gap-4">
        <label>数量<input v-model.number="count" type="number" min="1" max="100" class="input mt-2" /></label>
        <label>有效期<input value="固定 90 天" readonly class="input mt-2" /></label>
      </div>
      <p class="text-sm text-muted">新卡密为产品前缀（PULS- / GO- / PRO5X- / PRO-）加 15 位随机大写字母。完整卡密只显示一次，请下载保存。数据库仅保存校验摘要，无法找回原码；旧卡密仍可使用。</p>
      <button class="btn-primary" :disabled="busy || codes.length > 0" @click="issue">{{ busy ? '处理中…' : '免费生成本站卡密' }}</button>
      <button v-if="!codes.length" class="app-link ml-3" :disabled="busy" @click="newBatch">更换批次编号</button>
      <div v-if="codes.length" class="space-y-3">
        <label class="block">本次生成的卡密<textarea readonly :value="codes.join('\n')" class="input font-mono h-40 mt-2" /></label>
        <button class="btn-primary" @click="download">下载卡密文件</button>
        <button class="btn-secondary ml-3" @click="clearCodes">已妥善保存，清空显示</button>
      </div>
    </section>
    <details v-if="auth.can('system.manage')" class="card" :open="!settings.enabled">
      <summary class="font-bold cursor-pointer">充值通道：{{ settings.enabled ? '已开启' : '未开启（仍可生成和验证卡密）' }}</summary>
      <div class="space-y-4 mt-4">
        <p v-if="settings.pro_dedicated_enabled" class="alert alert-info">Pro 专卡模式已配置：客户确认后新开 P5378OX；Pro 5X 初始充值 $120，Pro 20X 初始充值 $150。对应升级由卡台确认成功后，卡片自动加入 Plus / Go 随机支付池；不设成功次数上限，也没有 2 天冷静期。达到安全销卡条件后是否自动销卡，以“自动化”页面的开关为准。</p>
        <p class="text-sm text-muted">普通 Plus / Go 卡从下方已勾选且符合付款条件的卡中随机选择；不设成功次数上限，也没有 2 天冷静期。系统仍会避开有在途订单、待核对资金或余额不足的卡；出现 1 次上游明确拒付则进入待销状态。</p>
        <p>API 密钥：{{ configured ? '已配置' : '未配置，请前往通道对接页面设置' }}</p>
        <div class="rounded-xl border border-orange-300 bg-orange-50/70 p-4 flex flex-wrap items-center justify-between gap-3">
          <div class="text-sm"><p><strong>白名单与通道设置</strong><span v-if="draftNotice" class="ml-2 text-orange-700">· {{ draftConflict ? '旧草稿与服务器版本冲突，请放弃草稿后重新修改' : '有未保存修改，已保留本机草稿' }}</span><span v-else class="ml-2 text-muted">· 已与服务器同步</span></p><p v-if="savedAt" class="mt-1 text-muted">最近保存：{{ savedAt }}</p><p v-if="error" data-channel-save-error class="mt-2 text-red-700">{{ error }}</p></div>
          <div class="flex gap-2"><button v-if="draftNotice" type="button" class="btn-secondary" :disabled="busy" @click="discardDraft">放弃草稿并重新加载</button><button type="button" class="btn-primary" :disabled="busy || !settingsReady || draftConflict" @click="saveSettings">{{ busy?'正在保存…':'保存当前设置' }}</button></div>
        </div>
        <fieldset class="space-y-3" :disabled="busy || !settingsReady">
          <legend class="font-bold">勾选支付卡（已选 {{ selectedIDs.length }} / 20）</legend>
          <p class="text-sm text-muted">勾选普通卡加入白名单，再保存设置。Pro 成功入池卡由系统自动管理，不占 20 张普通白名单名额。余额为平台参考值，可能延迟；入池不代表该卡一定能支付，仍以卡台实时检查为准。</p>
          <div class="flex flex-wrap gap-2 items-center">
            <button type="button" class="btn-secondary" :disabled="cardsLoading" @click="loadCards(cardPage)">{{ cardsLoading ? '正在读取卡片…' : '刷新卡片列表' }}</button>
            <button type="button" class="btn-secondary" :disabled="cardsLoading || !selectableCurrentPageIDs.length" @click="toggleSelectAllCurrentPage">{{ allCurrentPageSelected ? '取消全选' : '一键全选' }}</button>
            <span v-if="selectableCurrentPageIDs.length" class="text-sm text-muted">当前页可选 {{ selectableCurrentPageIDs.length }} 张</span>
          </div>
          <p v-if="cardsError" class="text-red-600" role="alert">{{ cardsError }}</p>
          <div v-if="!cardsLoading && !cardsError && !cardChoices.length" class="text-muted">暂无卡片，请先在 Zovo 查看是否已有卡。</div>
          <div class="grid sm:grid-cols-2 gap-3">
            <label v-for="card in cardChoices" :key="card.id" class="flex gap-3 items-start rounded-xl border p-4 cursor-pointer" :class="selectedIDs.includes(card.id) || (card.pool_member && card.card_kind==='pro') ? 'border-orange-500 bg-orange-500/5' : 'border-current/15'">
              <input type="checkbox" class="mt-1 h-5 w-5" :checked="selectedIDs.includes(card.id) || (card.pool_member && card.card_kind==='pro')" :disabled="card.card_kind==='pro' || (!selectedIDs.includes(card.id) && (selectedIDs.length >= 20 || card.status !== 'ACTIVE'))" @change="toggleCard(card.id,($event.target as HTMLInputElement).checked)" />
              <span class="min-w-0 flex-1"><span class="block font-semibold">尾号 {{ card.last4 || '未知' }} <span class="float-right">{{ cardBalance(card.balance_usd) }}</span></span><span class="block text-sm text-muted break-all">{{ card.product_code || '未知卡类型' }} · {{ card.status === 'ACTIVE' ? '已激活' : card.status || '状态未知' }}</span><span class="block text-sm text-muted">卡片 ID：{{ card.id }}</span><span v-if="card.pool_member" class="block text-sm" :class="card.retired?'text-orange-700':'text-muted'">{{ card.card_kind==='pro'?'Pro 成功入池卡':'普通池卡' }} · {{ card.phase==='final'?'最后阶段':'普通阶段' }} Plus 成功 {{ card.completed_count }} / {{ card.success_limit }} 次 · {{ card.retire_state&&card.retire_state!=='active'?retireLabel(card.retire_state):card.cooling?'冷静至 '+dateTime(card.cooldown_until):'剩余 '+card.remaining_uses+' 次' }} · 明确拒付 {{ card.decline_count || 0 }} 次</span></span>
            </label>
          </div>
          <div v-if="cardTotal>20" class="flex gap-3 items-center"><button type="button" class="btn-secondary" :disabled="cardsLoading || cardPage<=1" @click="loadCards(cardPage-1)">上一页</button><span>第 {{ cardPage }} 页 · 共 {{ cardTotal }} 张</span><button type="button" class="btn-secondary" :disabled="cardsLoading || cardPage*20>=cardTotal" @click="loadCards(cardPage+1)">下一页</button></div>
          <div class="space-y-2">
            <p class="font-semibold">支付卡池（随机选择）</p><p v-if="!selectedIDs.length" class="text-sm text-muted">尚未选择卡片，不会使用任何卡。</p>
            <div v-for="(id,index) in selectedIDs" :key="id" class="flex flex-wrap gap-2 items-center rounded-lg border p-3">
              <span class="flex-1 min-w-40">{{ index+1 }}. {{ selectedCardLabel(id) }}</span>
              <button type="button" class="btn-secondary" :aria-label="'移除 '+selectedCardLabel(id)" @click="toggleCard(id,false)">移除</button>
            </div>
          </div>
        </fieldset>
        <div class="grid sm:grid-cols-2 gap-4">
          <label>选卡最低余额（美分）<input v-model.number="settings.min_card_balance_minor" type="number" min="1" class="input mt-2" /><span class="block text-sm text-muted mt-1">1000 = 10 美元。参考平台余额，可能延迟；此门槛不是付款限额，也不保证足够支付实际报价。</span></label>
          <label>允许的报价币种<input v-model="settings.currency" class="input mt-2" placeholder="填写真实报价币种" maxlength="3" /></label>
          <label>单笔订阅金额上限（最小货币单位）<input v-model.number="settings.max_amount_minor" type="number" min="1" class="input mt-2" /></label>
          <label>单笔 API 服务费上限（美分）<input v-model.number="settings.max_fee_minor" type="number" min="0" class="input mt-2" /></label>
          <p class="text-sm text-muted">本站只记录卡台权威确认的成功付款与明确拒付，用于追踪卡片表现；成功次数不再限制后续用卡。符合实时付款条件的卡将随机使用，不设 2 天冷静期。结果不明、验证码或查询失败不会触发销卡。</p>
        </div>
        <p class="text-sm text-muted">例如 100 美分 = 1 美元。订阅金额单位以通道报价为准，不能把不同币种混用；两项上限不含另外的开卡、充值及汇兑费用。</p>
        <p class="text-sm text-muted">一旦提交付款，无论失败或结果不明，都不会自动换卡再付。多张卡的余额不能合并；待核对订单需先查清资金结果。</p>
        <label class="flex gap-2 items-start"><input v-model="settings.enabled" type="checkbox" class="mt-1" />开启真实兑换，并承担客户兑换产生的订阅款及 API 服务费</label>
        <button class="btn-primary" :disabled="busy || !settingsReady || draftConflict" @click="saveSettings">保存通道设置</button>
      </div>
    </details>
    <section class="card space-y-4">
      <div class="flex flex-wrap justify-between items-center gap-3"><h2 class="text-xl font-bold">近期卡密记录</h2><div class="flex gap-2"><router-link class="btn-secondary" to="/ops/records">搜索全部记录 / 导出 / 售后</router-link><router-link class="btn-secondary" to="/ops/orders">查看充值订单</router-link><button class="btn-secondary" :disabled="busy" @click="load">刷新</button></div></div>
      <p class="text-sm text-muted">待核对订单不会自动解锁或再次扣款。按商户订单号在 Zovo 核对，再填上游订单 ID 查询，不能随意标记成功。</p>
      <form class="flex flex-wrap gap-2" @submit.prevent="searchRecords"><input v-model="searchQuery" class="input flex-1" placeholder="完整卡密、前缀、批次或编号" maxlength="200" /><select v-model="searchStatus" class="input w-auto"><option value="">全部状态</option><option value="unused">未使用</option><option value="disabled">已禁用</option><option value="consumed">已兑换</option><option value="reserved">处理中</option><option value="review">待核对</option></select><button class="btn-primary" :disabled="busy">搜索</button></form>
      <div class="overflow-x-auto">
        <table class="w-full text-sm text-left"><thead><tr><th>ID / 卡密前缀</th><th>状态 / 有效期</th><th>批次 / 商户订单号</th><th>操作</th></tr></thead>
          <tbody><tr v-for="row in rows" :key="row.id" class="border-t"><td class="py-4 pr-3">{{ row.id }} · {{ row.prefix }}…</td><td class="pr-3">{{ statusLabel(row.status) }}<br />{{ date(row.expires_at) }}</td><td class="max-w-xs break-all pr-3">{{ row.batch_id }}<br /><span v-if="row.request_id">{{ row.request_id }}</span><p v-if="row.message" class="text-muted">{{ row.message }}</p></td><td><button v-if="auth.can('records.write') && row.status === 'unused'" class="btn-secondary" :disabled="busy" @click="disable(row)">禁用</button><button v-if="auth.can('records.write') && ['reserved','review'].includes(row.status)" class="btn-secondary" :disabled="busy" @click="reconcile(row)">核对订单</button></td></tr></tbody>
        </table>
      </div>
      <p v-if="!rows.length" class="text-muted">还没有本站卡密，可以在上方生成。</p>
    </section>
  </div>
</template>
<script setup lang="ts">
import { localPlanLabel } from '../../lib/local-plans'
import { computed, onMounted, onUnmounted, ref, reactive, watch } from 'vue'
import { authFetch } from '../../lib/api'
import { dialog } from '../../lib/dialog'
import { useAuthStore } from '../../stores/auth'
const auth=useAuthStore()
const busy=ref(false), error=ref(''), count=ref(1), days=ref(90), codes=ref<string[]>([]), rows=ref<any[]>([]), configured=ref(false)
const searchQuery=ref(''),searchStatus=ref('')
const reserveStock=ref<any[]>([])
async function searchRecords(){await run(async()=>{const d=await api('local-cdks/search',{method:'POST',body:JSON.stringify({q:searchQuery.value.trim(),status:searchStatus.value})});rows.value=d.list})}
type CardChoice={id:number;last4:string;product_code:string;status:string;balance_usd:number|null;completed_count?:number;success_limit?:number;remaining_uses?:number;retired?:boolean;cooling?:boolean;cooldown_until?:number;card_kind?:string;pool_member?:boolean;phase?:string;decline_count?:number;retire_state?:string}
const products=ref<any[]>([]),productID=ref('plus')
function selectProduct(){days.value=90}
const selectedIDs=ref<number[]>([]), cardChoices=ref<CardChoice[]>([]), cardsLoading=ref(false), cardsError=ref(''), cardPage=ref(1), cardTotal=ref(0), settingsReady=ref(false)
const draftKey='maplepass_cdk_channel_draft_v1',draftNotice=ref(''),draftConflict=ref(false),sourceRevision=ref(''),savedAt=ref('')
let savedSnapshot=''
const cardLabels=reactive<Record<number,CardChoice>>({})
function toggleCard(id:number,checked:boolean){if(checked){if(!selectedIDs.value.includes(id)&&selectedIDs.value.length<20)selectedIDs.value.push(id)}else selectedIDs.value=selectedIDs.value.filter(v=>v!==id)}
const selectableCurrentPageIDs=computed(()=>cardChoices.value.filter(card=>card.card_kind!=='pro'&&card.status==='ACTIVE').map(card=>card.id))
const allCurrentPageSelected=computed(()=>selectableCurrentPageIDs.value.length>0&&selectableCurrentPageIDs.value.every(id=>selectedIDs.value.includes(id)))
function toggleSelectAllCurrentPage(){
  const current=new Set(selectableCurrentPageIDs.value)
  if(allCurrentPageSelected.value){
    selectedIDs.value=selectedIDs.value.filter(id=>!current.has(id))
    return
  }
  const next=[...selectedIDs.value]
  for(const id of selectableCurrentPageIDs.value){
    if(!next.includes(id)&&next.length<20)next.push(id)
  }
  selectedIDs.value=next
}
function moveCard(index:number,delta:number){const to=index+delta;if(to<0||to>=selectedIDs.value.length)return;[selectedIDs.value[index],selectedIDs.value[to]]=[selectedIDs.value[to],selectedIDs.value[index]]}
function selectedCardLabel(id:number){return cardLabels[id]?`尾号 ${cardLabels[id].last4 || '未知'} · ID ${id}`:`ID ${id}（详情尚未加载，保留原选择）`}
function cardBalance(value:number|null){return typeof value==='number'&&Number.isFinite(value)?`$${value.toFixed(2)}`:'余额未知'}
function dateTime(n?:number){return n?new Date(n*1000).toLocaleString():'未知时间'}
function retireLabel(v?:string){return ({queued:'等待安全销卡',inflight:'正在销卡',unknown:'销卡待核对',closed:'已销卡'} as Record<string,string>)[v||'']||'停止使用'}
async function loadCards(page=1){if(cardsLoading.value)return;cardsLoading.value=true;cardsError.value='';try{const d=await api('local-card-choices?page='+page);cardChoices.value=d.list;cardTotal.value=d.total;cardPage.value=page;for(const card of d.list)cardLabels[card.id]=card}catch(e:any){cardsError.value=e.message || '加载失败，请重试'}finally{cardsLoading.value=false}}
const settings=reactive({pro_dedicated_enabled:false,use_upstream_card_limit:true,enabled:false,card_id:0,card_ids:[] as number[],min_card_balance_minor:0,currency:'',max_amount_minor:0,max_fee_minor:0,max_successful_payments_per_card:3})
function settingsSnapshot(){return JSON.stringify({...settings,card_id:0,card_ids:[...selectedIDs.value],currency:String(settings.currency||'').trim().toUpperCase()})}
function saveDraft(){if(!settingsReady.value)return;if(settingsSnapshot()===savedSnapshot&&!draftConflict.value){draftNotice.value='';try{localStorage.removeItem(draftKey)}catch{};return};draftNotice.value='有未保存修改';try{localStorage.setItem(draftKey,JSON.stringify({selected_ids:selectedIDs.value,settings,source_revision:sourceRevision.value}))}catch{}}
watch([selectedIDs,settings],saveDraft,{deep:true,flush:'sync'})
function discardDraft(){try{localStorage.removeItem(draftKey)}catch{};draftNotice.value='';draftConflict.value=false;settingsReady.value=false;load()}
let batch=sessionStorage.getItem('maple-pending-batch') || crypto.randomUUID()
async function api(path:string, init:RequestInit={}) { const r=await authFetch('/api/v1/admin/'+path,init); const d=await r.json(); if(!r.ok){const failure=new Error(d.error || '请求失败') as Error & {status:number};failure.status=r.status;throw failure}; return d }
async function run(action:()=>Promise<void>) { if(busy.value) return; busy.value=true; error.value=''; try{await action()}catch(e:any){error.value=e.message || '网络异常，请刷新核对结果'}finally{busy.value=false} }
async function load(){settingsReady.value=false;await run(async()=>{const [a,b,p]=await Promise.all([api('operations/records?page_size=25'),auth.can('system.manage')?api('local-cdk-settings'):Promise.resolve(null),api('operations/products')]);rows.value=a.list;products.value=p.list;if(!b)return;Object.assign(settings,b.settings);settings.use_upstream_card_limit=true;selectedIDs.value=[...(b.settings.card_ids ?? (b.settings.card_id>0?[b.settings.card_id]:[]))];configured.value=b.api_configured;sourceRevision.value=b.revision;savedAt.value=b.saved_at||'';draftConflict.value=false;draftNotice.value='';if(Number(settings.max_successful_payments_per_card)<=0)settings.max_successful_payments_per_card=3;savedSnapshot=settingsSnapshot()
 try{const raw=localStorage.getItem(draftKey);if(raw){const draft=JSON.parse(raw);if(draft&&typeof draft==='object'){if(Array.isArray(draft.selected_ids))selectedIDs.value=draft.selected_ids.filter((id:any)=>Number.isInteger(id)&&id>0).slice(0,20);if(draft.settings&&typeof draft.settings==='object')Object.assign(settings,draft.settings);draftConflict.value=draft.source_revision!==b.revision;sourceRevision.value=draft.source_revision||'';draftNotice.value='已恢复上次未保存的白名单草稿。核对后请点击“保存当前设置”。'}}}catch{}
 if(Number(settings.max_successful_payments_per_card)<=0)settings.max_successful_payments_per_card=3;if(settingsSnapshot()===savedSnapshot){draftNotice.value='';draftConflict.value=false;sourceRevision.value=b.revision;try{localStorage.removeItem(draftKey)}catch{}};settingsReady.value=true});if(settingsReady.value)await loadCards(cardPage.value)}
async function issue(){await run(async()=>{sessionStorage.setItem('maple-pending-batch',batch);const d=await api('local-cdks',{method:'POST',body:JSON.stringify({count:count.value,days:days.value,request_id:batch,product_id:productID.value})});codes.value=d.codes;sessionStorage.removeItem('maple-pending-batch');batch=crypto.randomUUID();rows.value=(await api('operations/records?page_size=25')).list})}
function download(){const blob=new Blob([codes.value.join('\r\n')],{type:'text/plain;charset=utf-8'});const url=URL.createObjectURL(blob);const a=document.createElement('a');a.href=url;a.download='maple-plus-cdks-'+Date.now()+'.txt';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000)}
async function clearCodes(){if(await dialog.confirm('确认已保存全部卡密？清空后无法从本站恢复原码。'))codes.value=[]}
async function newBatch(){if(await dialog.confirm('如果上次生成结果不明，请先在列表中核对并禁用丢失的卡密。确认开始新批次？')){batch=crypto.randomUUID();sessionStorage.removeItem('maple-pending-batch');error.value=''}}
function showChannelError(message:string){error.value=message;requestAnimationFrame(()=>document.querySelector('[data-channel-save-error]')?.scrollIntoView({behavior:'smooth',block:'center'}))}
function validateChannelSettings(){if(!settings.enabled)return '';if(!selectedIDs.value.length)return '请至少勾选一张支付卡后再保存。';if(Number(settings.min_card_balance_minor)<=0)return '请填写“选卡最低余额”，例如 2050 代表 $20.50。';if(!/^[A-Za-z]{3}$/.test(String(settings.currency||'').trim()))return '请填写三位报价币种，例如 USD；请以通道实际报价为准。';if(Number(settings.max_amount_minor)<=0)return '请填写“单笔订阅金额上限”，单位是该报价币种的最小货币单位。';if(Number(settings.max_fee_minor)<0)return '单笔 API 服务费上限不能小于 0。';if(!Number.isInteger(Number(settings.max_successful_payments_per_card))||Number(settings.max_successful_payments_per_card)<1||Number(settings.max_successful_payments_per_card)>100)return '每张卡最多成功支付次数应为 1–100。';return ''}
async function saveSettings(){if(!settingsReady.value||draftConflict.value)return;const invalid=validateChannelSettings();if(invalid){showChannelError(invalid);return}if(settings.enabled && !await dialog.confirm('开启后，客户确认兑换将从白名单中选一张卡付款，费用由你承担；提交后不自动换卡重付。确认保存？'))return;await run(async()=>{const ids=[...selectedIDs.value];let d;try{d=await api('local-cdk-settings',{method:'PUT',body:JSON.stringify({...settings,card_id:0,card_ids:ids,expected_revision:sourceRevision.value})})}catch(e:any){if(e.status===409){draftConflict.value=true;saveDraft()};throw e};settingsReady.value=false;settings.card_ids=ids;settings.card_id=0;sourceRevision.value=d.revision;savedAt.value=d.saved_at;savedSnapshot=settingsSnapshot();try{localStorage.removeItem(draftKey)}catch{};draftNotice.value='';draftConflict.value=false;settingsReady.value=true;dialog.toast('已保存到服务器')});if(error.value)showChannelError(error.value)}
async function disable(row:any){if(!await dialog.confirm('确认禁用这张未兑换卡密？'))return;await run(async()=>{await api(`local-cdks/${row.id}/disable`,{method:'POST'});rows.value=(await api('local-cdks')).list})}
async function reconcile(row:any){const value=await dialog.prompt('请输入与此商户订单号对应的 Zovo 直充订单 ID。只查单，不重新付款。',{defaultValue:row.upstream_id?String(row.upstream_id):''});if(!value)return;await run(async()=>{await api(`local-cdks/${row.id}/reconcile`,{method:'POST',body:JSON.stringify({order_id:Number(value)})});rows.value=(await api('local-cdks')).list})}
function statusLabel(s:string){return ({unused:'未使用',reserved:'处理中',review:'待核对',consumed:'已兑换',disabled:'已禁用',failed:'未完成，请核对资金'} as Record<string,string>)[s] || s}
function date(n:number){return new Date(n*1000).toLocaleDateString()}
let cardRefreshTimer:ReturnType<typeof setInterval>|undefined
onMounted(()=>{load();api('local-cdks/reserve').then(d=>reserveStock.value=d.list).catch(()=>{});cardRefreshTimer=setInterval(()=>{if(!document.hidden && !busy.value){api('local-cdks/reserve').then(d=>reserveStock.value=d.list).catch(()=>{});if(settingsReady.value)loadCards(cardPage.value)}},60000)})
onUnmounted(()=>{if(cardRefreshTimer)clearInterval(cardRefreshTimer)})
</script>
