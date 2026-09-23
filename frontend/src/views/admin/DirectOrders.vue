<template>
  <div class="space-y-4">
    <div class="flex flex-wrap justify-between items-center gap-3">
      <div><h1 class="text-2xl font-bold text-ink">充值订单</h1><p class="text-sm text-muted mt-2">本 API 账号的直充订单；Zovo 网页端的历史订单可能不在此列表。</p></div>
      <el-button :loading="loading" :disabled="acting" @click="load(page)">刷新订单</el-button>
    </div>
    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon />
    <div class="card space-y-4">
      <div class="flex flex-wrap gap-3">
        <el-input v-model="search" placeholder="在本页搜索邮箱、订单号或卡片尾号" clearable aria-label="搜索本页订单" class="!max-w-sm" />
        <el-select v-model="filter" aria-label="筛选本页订单状态" class="!w-44"><el-option label="本页全部状态" value="" /><el-option label="处理中" value="processing" /><el-option label="充值成功" value="completed" /><el-option label="失败或需核对" value="review" /><el-option label="已取消订单" value="cancelled" /></el-select>
      </div>
      <p class="text-sm text-muted">金额区分预估与最终金额；充值成功不代表已关闭续费，服务费状态也不等于卡片扣款状态。</p>
      <el-table :data="visibleRows" v-loading="loading" empty-text="暂无符合条件的订单；没有真实充值时，列表为空是正常的。" stripe>
        <el-table-column label="账号 / 订单" min-width="210"><template #default="{row}"><div>{{ row.account_email || row.email || '上游未返回账号' }}</div><div class="text-sm text-muted">#{{ row.id }} · {{ row.client_request_id || '—' }}</div></template></el-table-column>
        <el-table-column label="卡片 / 套餐" min-width="140"><template #default="{row}"><div>{{ row.card_last_four ? '•••• '+row.card_last_four : '尾号未返回' }}</div><div class="text-sm text-muted">{{ row.product || 'GPT' }} / {{ row.plan || '—' }}</div></template></el-table-column>
        <el-table-column label="充值状态" min-width="140"><template #default="{row}"><el-tag :type="tone(row.status)">{{ status(row.status) }}</el-tag><div class="text-sm text-muted mt-1">{{ row.stage || '' }}</div></template></el-table-column>
        <el-table-column label="金额" min-width="155"><template #default="{row}"><div>{{ money(row.final_amount_minor,row.currency) }}</div><div class="text-sm text-muted">预估 {{ money(row.quoted_amount_minor,row.currency) }}</div></template></el-table-column>
        <el-table-column label="自动续费" min-width="140"><template #default="{row}"><el-tag :type="row.renewal_status==='success'?'success':'info'">{{ renewal(row.renewal_status) }}</el-tag></template></el-table-column>
        <el-table-column label="提交时间 / 耗时" min-width="190"><template #default="{row}"><div>{{ date(row.created_at) }}</div><div class="text-sm text-muted">{{ duration(row) }}</div></template></el-table-column>
        <el-table-column label="操作" width="110" fixed="right"><template #default="{row}"><el-button link type="primary" :disabled="detailLoading || acting" @click="open(row.id)">查看详情</el-button></template></el-table-column>
      </el-table>
      <div class="flex flex-wrap items-center justify-between gap-3"><span class="text-sm text-muted">共 {{ total }} 条 · 第 {{ page }} 页 · 搜索和筛选仅作用于本页</span><div class="flex gap-2"><el-button :disabled="page<=1 || loading || acting" @click="load(page-1)">上一页</el-button><el-button :disabled="page*20>=total || loading || acting" @click="load(page+1)">下一页</el-button></div></div>
    </div>
    <el-dialog v-model="showDetail" title="充值订单详情" width="min(850px, 95vw)" :close-on-click-modal="!acting" :close-on-press-escape="!acting" :show-close="!acting">
      <div v-loading="detailLoading" class="space-y-4">
        <el-alert v-if="detailError" :title="detailError" type="error" :closable="false" />
        <template v-if="detail">
          <el-descriptions :column="1" border>
            <el-descriptions-item label="订单">#{{ detail.order.id }} · {{ detail.order.client_request_id || '—' }}</el-descriptions-item>
            <el-descriptions-item label="账号">{{ detail.order.account_email || detail.order.email || '未返回' }}</el-descriptions-item>
            <el-descriptions-item label="充值状态">{{ status(detail.order.status) }} · {{ detail.order.stage || '阶段未返回' }}</el-descriptions-item>
            <el-descriptions-item label="支付事实">{{ detail.order.payment_fact || detail.order.payment_status || '上游未返回独立支付结果，请结合消费记录核对' }}</el-descriptions-item>
            <el-descriptions-item label="最终金额">{{ money(detail.order.final_amount_minor,detail.order.currency) }}（预估 {{ money(detail.order.quoted_amount_minor,detail.order.currency) }}）</el-descriptions-item>
            <el-descriptions-item label="服务费">{{ money(detail.order.service_fee_minor,'USD') }} · {{ feeStatus(detail.order.service_fee_status) }}</el-descriptions-item>
            <el-descriptions-item label="自动续费">{{ renewal(detail.order.renewal_status) }}<p>{{ detail.order.renewal_message || '' }}</p></el-descriptions-item>
            <el-descriptions-item label="结果说明">{{ detail.order.message || detail.order.error_code || '暂无补充说明' }}</el-descriptions-item>
            <el-descriptions-item label="时间">{{ date(detail.order.created_at) }} → {{ date(detail.order.completed_at) }} · {{ duration(detail.order) }}</el-descriptions-item>
          </el-descriptions>
          <div><h3 class="font-bold mb-3">处理记录</h3><el-empty v-if="!detail.events.length" description="上游暂未返回公开处理记录" :image-size="55" /><el-timeline v-else><el-timeline-item v-for="(event,index) in detail.events" :key="index" :timestamp="date(event.occurred_at || event.created_at)"><p>{{ event.public_message || event.public_code || event.step || event.category || '状态更新' }}</p><p class="text-sm text-muted">{{ event.to_status ? status(event.to_status) : '' }} {{ event.payment_fact || '' }}</p></el-timeline-item></el-timeline></div>
          <el-alert title="取消续费仅停止下一期自动扣款，本期权益保留，不会退款。取消订单仅适用于尚未开始支付的订单。" type="info" :closable="false" />
          <div class="flex flex-wrap gap-3"><el-button :disabled="acting" :loading="detailLoading" @click="open(detail.order.id)">刷新详情</el-button><el-button v-if="canCancel" type="danger" plain :loading="acting" :disabled="detailLoading" @click="act('cancel')">取消未支付订单</el-button><el-button v-if="canRenewal" type="warning" :loading="acting" :disabled="detailLoading" @click="act('cancel-renewal')">取消自动续费 / 复查</el-button></div>
        </template>
      </div>
    </el-dialog>
  </div>
</template>
<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { authFetch } from '../../lib/api'
import { dialog } from '../../lib/dialog'
type Order=Record<string,any>
const rows=ref<Order[]>([]),total=ref(0),page=ref(1),search=ref(''),filter=ref(''),loading=ref(false),error=ref('')
const detail=ref<{order:Order;events:Order[]}|null>(null),showDetail=ref(false),detailLoading=ref(false),detailError=ref(''),acting=ref(false)
const failed=['declined','failed_precharge','failed','requires_action']
const terminal=['completed','declined','failed_precharge','failed','cancelled']
const visibleRows=computed(()=>rows.value.filter(r=>{
 const text=[r.account_email,r.email,r.id,r.client_request_id,r.card_last_four].join(' ').toLowerCase()
 const match=!filter.value || (filter.value==='processing'?!terminal.includes(r.status)&&!failed.includes(r.status):filter.value==='review'?failed.includes(r.status):r.status===filter.value)
 return match && text.includes(search.value.trim().toLowerCase())
}))
const canCancel=computed(()=>['queued','awaiting_card','funding_pending'].includes(detail.value?.order.status))
const canRenewal=computed(()=>detail.value?.order.status==='completed' && (!detail.value.order.product || detail.value.order.product==='gpt') && ['pending','warning'].includes(detail.value.order.renewal_status))
async function api(path:string,options:RequestInit={}) {const response=await authFetch('/api/v1/admin/direct-orders'+path,options);const data=await response.json();if(!response.ok)throw new Error(data.error || '请求失败');return data}
async function load(next=page.value){if(loading.value)return;loading.value=true;error.value='';try{const data=await api('?page='+next);rows.value=data.list;total.value=data.total;page.value=next}catch(e:any){error.value=e.message}finally{loading.value=false}}
async function open(id:number){if(detailLoading.value)return;showDetail.value=true;detail.value=null;detailError.value='';detailLoading.value=true;try{detail.value=await api('/'+id)}catch(e:any){detailError.value=e.message}finally{detailLoading.value=false}}
async function act(action:'cancel'|'cancel-renewal') {
 if(acting.value || !detail.value)return
 const order={...detail.value.order}
 acting.value=true
 try {
  const message=action==='cancel'?`确认取消订单 #${order.id}？仅未开始支付时可以取消；此操作不会退回已扣款项，也不会重新发起充值。`:`确认关闭订单 #${order.id}（${order.account_email || order.email || '当前订单账号'}）的自动续费？本期会员保留，到期后不再自动扣款，不会退款。`
  if(!await dialog.confirm(message,{title:action==='cancel'?'确认取消订单':'确认取消自动续费',okText:'确认提交',cancelText:'暂不操作',danger:true}))return
  const data=await api('/'+order.id+'/'+action,{method:'POST',body:JSON.stringify({confirmed:true,expected_status:order.status})})
  dialog.toast(data.message,'info');await open(order.id);await load()
 }catch(e:any){detailError.value=e.message}finally{acting.value=false}
}
function status(s:string){return ({queued:'排队中',running:'处理中',dispatching:'正在提交',awaiting_card:'等待卡片',funding_pending:'等待资金',pending:'待确认',plus_paid:'已支付，待升级',requires_action:'需要处理 / 核对',completed:'充值成功',declined:'支付被拒，请核对资金',failed_precharge:'支付前失败',failed:'失败，请核对资金',cancelled:'订单已取消'} as Record<string,string>)[s] || s || '未知'}
function renewal(s:string){return ({success:'续费已取消',pending:'取消待确认',warning:'取消需复查',not_requested:'尚未请求取消'} as Record<string,string>)[s] || '续费状态未返回'}
function feeStatus(s:string){return ({held:'预留中',settled:'已结算',released:'已释放',refunded:'已退回'} as Record<string,string>)[s] || s || '未返回'}
function tone(s:string):'success'|'danger'|'info'|'warning'{return s==='completed'?'success':failed.includes(s)?'danger':s==='cancelled'?'info':'warning'}
function money(n:any,currency:string){return typeof n==='number' && Number.isFinite(n) && currency ? currency+' '+(n/100).toFixed(2):'未返回'}
function millis(v:any){if(typeof v==='number')return v>1e12?v:v*1000;return typeof v==='string'?Date.parse(v):NaN}
function date(v:any){const n=millis(v);return Number.isFinite(n)&&n>0?new Date(n).toLocaleString():'未返回'}
function duration(r:Order){const start=millis(r.created_at),end=millis(r.completed_at);if(!Number.isFinite(start)||start<=0)return '耗时未知';if(!Number.isFinite(end)||end<=0)return terminal.includes(r.status)?'结束时间未返回':'处理中';return end>=start?Math.round((end-start)/1000)+' 秒':'时间待核对'}
onMounted(()=>load(1))
</script>
