<template>
  <div class="space-y-6 min-w-0">
    <h1 class="text-2xl font-bold">卡密记录与售后</h1>
    <section class="card space-y-4">
      <div class="grid sm:grid-cols-2 xl:grid-cols-4 gap-3">
        <label>搜索<input v-model="filters.q" class="input mt-1" placeholder="前缀 / 批次 / 商户订单号 / ID" @keyup.enter="search" /></label>
        <label>支付状态<select v-model="filters.status" class="input mt-1"><option value="">全部</option><option v-for="(label,key) in statuses" :key="key" :value="key">{{ label }}</option></select></label>
        <label>创建开始日期<input v-model="filters.from" type="date" class="input mt-1" /></label>
        <label>创建结束日期<input v-model="filters.to" type="date" class="input mt-1" /></label>
        <label>日期时区<select v-model="filters.tz" class="input mt-1"><option value="Asia/Bangkok">曼谷时间（UTC+7）</option><option value="UTC">UTC</option></select></label>
        <label>批次（精确）<input v-model="filters.batch" class="input mt-1" /></label>
        <label>上游订单 ID<input v-model="filters.order_id" class="input mt-1" inputmode="numeric" /></label>
        <label>商品<select v-model="filters.product" class="input mt-1"><option value="">全部商品</option><option v-for="p in products" :key="p.id" :value="p.id">{{ p.name }}</option></select></label>
        <label>售后状态<select v-model="filters.support_status" class="input mt-1"><option value="">全部</option><option value="open">待处理</option><option value="in_progress">处理中</option><option value="resolved">已结案</option></select></label>
        <label>处理人<input v-model="filters.assignee" class="input mt-1" placeholder="精确匹配处理人" /></label>
      </div>
      <div class="flex flex-wrap gap-3 items-center"><label class="flex gap-2"><input v-model="filters.orders_only" type="checkbox" />只看已提交订单</label><button class="btn-primary" :disabled="loading" @click="search">查询</button><button class="btn-secondary" :disabled="loading" @click="reset">重置</button><button class="btn-secondary" :disabled="exporting||loading" @click="download">{{ exporting?'导出中…':'导出当前查询全部结果 CSV' }}</button></div>
      <p class="text-sm text-muted">导出包含当前查询的全部记录；仅含卡密前缀，不含完整卡密或登录凭证。筛选、显示和导出统一采用所选时区。</p>
    </section>
    <div v-if="error" class="alert alert-error" role="alert">{{ error }}</div><div v-if="notice" class="text-green-700" role="status">{{ notice }}</div>
    <section class="card space-y-4">
      <div class="flex flex-wrap justify-between items-center gap-3"><p>共 {{ total }} 条 · 第 {{ page }} / {{ Math.max(1,Math.ceil(total/pageSize)) }} 页 · {{ applied.tz==='Asia/Bangkok'?'曼谷时间':'UTC' }}</p><label>每页 <select v-model.number="pageSize" :disabled="loading" class="input !w-auto" @change="search"><option :value="25">25</option><option :value="50">50</option><option :value="100">100</option></select></label></div>
      <p v-if="loading" role="status">正在查询…</p>
      <div class="space-y-3 md:hidden"><article v-for="r in rows" :key="r.id" class="rounded-xl border p-3 space-y-2 break-words"><div class="flex justify-between gap-2"><strong>#{{ r.id }} · {{ r.product_name }}</strong><span>{{ statuses[r.status]||r.status }}</span></div><p class="font-mono text-sm">{{ r.prefix }}…</p><p class="text-sm break-all">批次：{{ r.batch_id }}<br />订单：{{ r.request_id||'未提交' }}<span v-if="r.upstream_id"><br />上游订单：{{ r.upstream_id }}</span></p><p class="text-sm text-muted">创建：{{ date(r.created_at) }}<br />到期：{{ date(r.expires_at) }}</p><div class="flex flex-wrap justify-between items-center gap-2"><span class="text-sm">{{ supportLabels[r.support_status]||'未建售后' }} {{ r.assignee }}</span><button class="btn-secondary" @click="openSupport(r.id)">详情 / 售后</button></div></article></div>
      <div class="hidden md:block overflow-x-auto"><table class="w-full text-sm text-left"><thead><tr><th class="py-3">卡密 / 商品</th><th>支付状态</th><th>批次 / 订单</th><th>创建 / 到期</th><th>售后</th><th>操作</th></tr></thead><tbody>
        <tr v-for="r in rows" :key="r.id" class="border-t"><td class="py-4 pr-3">#{{ r.id }} · {{ r.prefix }}…<br />{{ r.product_name }}</td><td class="pr-3">{{ statuses[r.status]||r.status }}<br /><span class="text-muted">支付卡 {{ r.card_id||'—' }}</span></td><td class="pr-3 max-w-xs break-all">{{ r.batch_id }}<br />{{ r.request_id||'未提交订单' }}<br /><span v-if="r.upstream_id">上游 {{ r.upstream_id }}</span></td><td class="pr-3 whitespace-nowrap">{{ date(r.created_at) }}<br />{{ date(r.expires_at) }}</td><td class="pr-3">{{ supportLabels[r.support_status]||'未建售后' }}<br />{{ r.assignee }}</td><td><button class="btn-secondary" @click="openSupport(r.id)">详情 / 售后</button></td></tr>
      </tbody></table></div><p v-if="!loading&&!rows.length" class="text-muted">没有符合条件的记录。</p>
      <div class="flex gap-3"><button class="btn-secondary" :disabled="loading||page<=1" @click="load(page-1)">上一页</button><button class="btn-secondary" :disabled="loading||page*pageSize>=total" @click="load(page+1)">下一页</button></div>
    </section>
    <section v-if="detail" ref="detailEl" class="card space-y-4">
      <div class="flex justify-between gap-2"><h2 class="text-xl font-bold">记录 #{{ detail.record.id }} · 售后工作台</h2><button class="btn-secondary" @click="detail=null">关闭</button></div>
      <p>支付状态：{{ statuses[detail.record.status]||detail.record.status }}。{{ detail.record.message }}</p>
      <p class="text-sm text-muted">售后记录独立于付款状态。填写退款或补发结论只保存备注，不会执行退款、补发或再次付款。</p>
      <div v-if="supportError" class="alert alert-error" role="alert">{{ supportError }}</div>
      <fieldset :disabled="!auth.can('support.write')" class="space-y-4"><div class="grid sm:grid-cols-2 gap-3"><label>售后状态<select v-model="support.status" class="input mt-1"><option value="open">待处理</option><option value="in_progress">处理中</option><option value="resolved">已结案</option></select></label><label>处理人<input v-model="support.assignee" class="input mt-1" maxlength="80" /></label></div>
      <label class="block">处理结论<textarea v-model="support.conclusion" class="input mt-1" maxlength="2000" placeholder="结案时必填，请写明实际核查和处理结果" /></label><label class="block">新增备注<textarea v-model="support.note" class="input mt-1" maxlength="4000" placeholder="记录问题、联系情况和核对结果。请勿填写密码或登录凭证。" /></label>
      <button v-if="auth.can('support.write')" class="btn-primary" :disabled="saving" @click="saveSupport">{{ saving?'保存中…':'保存售后记录' }}</button></fieldset>
      <h3 class="font-bold">处理历史</h3><p v-if="!detail.events.length" class="text-muted">尚无处理记录。</p><article v-for="event in detail.events" :key="event.id" class="border-t pt-3 space-y-2"><p class="text-sm text-muted">{{ date(event.created_at) }} · {{ event.actor||'管理员' }}</p><p class="whitespace-pre-wrap">{{ event.note||'更新了售后信息' }}</p><p class="text-sm">{{ supportLabels[event.changes?.after?.status] }} · 处理人：{{ event.changes?.after?.assignee||'未分配' }}</p><p v-if="event.changes?.after?.conclusion" class="text-sm whitespace-pre-wrap">处理结论：{{ event.changes.after.conclusion }}</p></article>
    </section>
  </div>
</template>
<script setup lang="ts">
import { nextTick, onMounted, reactive, ref } from 'vue'
import { authFetch } from '../../lib/api'
import { useAuthStore } from '../../stores/auth'
const auth=useAuthStore()
const defaults=()=>({q:'',status:'',from:'',to:'',tz:'Asia/Bangkok',batch:'',order_id:'',product:'',support_status:'',assignee:'',orders_only:false})
const filters=reactive(defaults()),applied=ref(defaults()),page=ref(1),pageSize=ref(25),total=ref(0),rows=ref<any[]>([]),products=ref<any[]>([]),loading=ref(false),exporting=ref(false),saving=ref(false),error=ref(''),notice=ref(''),supportError=ref(''),detail=ref<any>(null),detailEl=ref<HTMLElement|null>(null)
const support=reactive({status:'open',assignee:'',conclusion:'',note:'',version:0})
const statuses:Record<string,string>={unused:'未使用',reserved:'处理中',review:'待核对',consumed:'已兑换',disabled:'已禁用',failed:'未完成，需核对资金'}
const supportLabels:Record<string,string>={open:'待处理',in_progress:'处理中',resolved:'已结案'}
function date(n:number){return n?new Date(n*1000).toLocaleString('zh-CN',{timeZone:applied.value.tz,hour12:false}):'—'}
function params(){const p=new URLSearchParams();for(const[k,v]of Object.entries(applied.value)){if(v!==''&&v!==false)p.set(k,String(v))}return p}
async function api(path:string,init:RequestInit={}){const r=await authFetch('/api/v1/admin/operations/'+path,init);const d=await r.json();if(!r.ok)throw Error(d.error||'请求失败');return d}
async function load(nextPage=1){if(loading.value)return;loading.value=true;error.value='';try{const p=params();p.set('page',String(nextPage));p.set('page_size',String(pageSize.value));const d=await api('records?'+p);rows.value=d.list;total.value=d.total;page.value=nextPage}catch(e:any){error.value=e.message}finally{loading.value=false}}
function search(){if(loading.value)return;applied.value={...filters};notice.value='';load(1)}
function reset(){Object.assign(filters,defaults());search()}
async function download(){if(exporting.value)return;exporting.value=true;error.value='';try{const r=await authFetch('/api/v1/admin/operations/records/export?'+params());if(!r.ok){const d=await r.json();throw Error(d.error||'导出失败')};const blob=await r.blob();const url=URL.createObjectURL(blob);const a=document.createElement('a');a.href=url;a.download='maple-records-'+new Date().toISOString().slice(0,10)+'.csv';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);notice.value='筛选记录已导出'}catch(e:any){error.value=e.message}finally{exporting.value=false}}
async function openSupport(id:number){supportError.value='';try{detail.value=await api('records/'+id+'/support');Object.assign(support,detail.value.support,{note:''});await nextTick();detailEl.value?.scrollIntoView({behavior:'smooth',block:'start'})}catch(e:any){error.value=e.message}}
async function saveSupport(){if(saving.value||!detail.value)return;saving.value=true;supportError.value='';const id=detail.value.record.id;try{await api('records/'+id+'/support',{method:'PUT',body:JSON.stringify(support)});await openSupport(id);notice.value='售后记录已保存';await load(page.value)}catch(e:any){supportError.value=e.message}finally{saving.value=false}}
onMounted(async()=>{await load();try{products.value=(await api('products')).list}catch(e:any){error.value=e.message}})
</script>
