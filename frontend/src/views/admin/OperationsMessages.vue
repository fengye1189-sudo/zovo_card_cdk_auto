<template>
 <div class="space-y-4"><div><h1 class="text-2xl font-bold">通知与客服回复</h1><p class="text-muted mt-2">统一处理团队异常提醒和订单回复文案。</p></div>
  <NotificationPanel v-if="auth.can('system.manage')" />
  <section class="card space-y-4"><h2 class="text-xl font-bold">订单回复模板</h2><p>填写订单编号即可复制回复。{order_id} 会替换为下方编号；复制后在你的客服聊天窗口发送。</p>
   <p v-if="error" role="alert" class="text-red-700">{{ error }}</p><p v-if="saved" role="status">{{ saved }}</p>
   <label class="block">订单编号<input v-model="orderID" class="input mt-2" placeholder="例如 MAPLE-ORDER-…" maxlength="120" /></label>
   <div v-for="entry in labels" :key="entry.key" class="space-y-2"><label :for="'reply-'+entry.key">{{ entry.label }}</label><textarea :id="'reply-'+entry.key" v-model="templates[entry.key]" class="input min-h-24" :readonly="!auth.can('system.manage')" maxlength="1000" /><button class="btn-secondary" @click="copy(entry.key)">复制{{ entry.label }}回复</button></div>
   <button v-if="auth.can('system.manage')" class="btn-primary" :disabled="busy" @click="save">保存模板</button>
  </section>
 </div>
</template>
<script setup lang="ts">
import {onMounted,reactive,ref} from 'vue'
import {useAuthStore} from '../../stores/auth'
import {authFetch} from '../../lib/api'
import {copyToClipboard} from '../../lib/clipboard'
import {dialog} from '../../lib/dialog'
import NotificationPanel from './NotificationPanel.vue'
const auth=useAuthStore(),templates=reactive<Record<string,string>>({}),labels=[{key:'processing',label:'处理中'},{key:'completed',label:'已完成'},{key:'review',label:'待核对'},{key:'failed',label:'未完成'}],revision=ref(''),error=ref(''),saved=ref(''),orderID=ref(''),busy=ref(false)
async function api(init:RequestInit={}){const r=await authFetch('/api/v1/admin/operations/reply-templates',init);const d=await r.json();if(!r.ok)throw Error(d.error||'模板操作失败');return d}
function apply(d:any){Object.assign(templates,d.templates);revision.value=d.revision}
async function save(){if(busy.value)return;busy.value=true;error.value='';try{apply(await api({method:'PUT',body:JSON.stringify({templates,revision:revision.value})}));saved.value='模板已保存 '+new Date().toLocaleTimeString()}catch(e:any){error.value=e.message}finally{busy.value=false}}
async function copy(key:string){if(!orderID.value.trim()){error.value='请先填写订单编号';return}error.value='';const text=(templates[key]||'').replaceAll('{order_id}',orderID.value.trim());dialog.toast(await copyToClipboard(text)?'回复已复制':'复制失败，请手动选择文字复制')}
onMounted(async()=>{try{apply(await api())}catch(e:any){error.value=e.message}})
</script>
