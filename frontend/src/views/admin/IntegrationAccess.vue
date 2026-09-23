<template>
 <div class="space-y-4">
  <div><h1 class="text-2xl font-bold">商城与机器人接入</h1><p class="text-muted mt-2">给不同系统分配独立的只读密钥，用于查询卡密订单和商品目录。</p></div>
  <p v-if="error" role="alert" class="card text-red-700">{{ error }}</p>
  <section class="card space-y-3"><h2 class="text-xl font-bold">创建接入密钥</h2>
   <div class="grid gap-3 sm:grid-cols-2"><label>用途名称<input v-model="name" maxlength="100" class="input mt-2" placeholder="例如：商城订单查询" /></label><label>有效天数<input v-model.number="days" type="number" min="1" max="365" class="input mt-2" /></label></div>
   <div class="flex flex-wrap gap-4"><label><input v-model="scopes" type="checkbox" value="records:read" /> 查询订单记录</label><label><input v-model="scopes" type="checkbox" value="products:read" /> 查询商品目录</label></div>
   <button class="btn-primary" :disabled="busy" @click="create">创建密钥</button>
   <div v-if="created" class="rounded-xl border p-4 space-y-2"><p>新密钥只在本次显示：</p><code class="break-all">{{ created }}</code><div class="flex gap-3"><button class="btn-secondary" @click="copyCreated">复制密钥</button><button class="btn-secondary" @click="created=''">关闭显示</button></div></div>
  </section>
  <section class="card space-y-3"><h2 class="text-xl font-bold">已有接入</h2><div class="overflow-x-auto"><table class="w-full text-left"><thead><tr><th>用途</th><th>标识</th><th>权限</th><th>有效期</th><th>最后使用</th><th>操作</th></tr></thead><tbody><tr v-for="item in rows" :key="item.id"><td>{{ item.name }}</td><td>{{ item.prefix }}…</td><td>{{ item.scopes.join(', ') }}</td><td>{{ item.revoked_at?'已撤销':date(item.expires_at) }}</td><td>{{ date(item.last_used_at) }}</td><td><button v-if="!item.revoked_at" class="btn-secondary" :disabled="busy" @click="revoke(item)">撤销</button></td></tr></tbody></table></div><p v-if="!rows.length">暂无接入密钥。</p></section>
  <section class="card space-y-3"><h2 class="text-xl font-bold">接入说明</h2><p>请求方式 GET；请求头使用 Authorization: Bearer &lt;接入密钥&gt;。每个密钥每分钟最多 60 次。</p><pre class="overflow-x-auto rounded-xl border p-3">{{ origin }}/api/v1/integration/records?page=1&amp;page_size=50
{{ origin }}/api/v1/integration/products</pre><p>订单返回本站记录；支持的筛选条件与「查询与售后」一致。接入密钥可随时撤销，不提供资金操作权限。</p></section>
 </div>
</template>
<script setup lang="ts">
import {onMounted,ref} from 'vue'
import {authFetch} from '../../lib/api'
import {copyToClipboard} from '../../lib/clipboard'
import {dialog} from '../../lib/dialog'
const name=ref(''),days=ref(30),scopes=ref(['records:read']),rows=ref<any[]>([]),created=ref(''),busy=ref(false),error=ref(''),origin=window.location.origin
async function api(path='',init:RequestInit={}){const r=await authFetch('/api/v1/admin/operations/api-tokens'+path,init);const d=await r.json();if(!r.ok)throw Error(d.error||'接入操作失败');return d}
async function load(){try{rows.value=(await api()).list}catch(e:any){error.value=e.message}}
async function create(){if(busy.value)return;busy.value=true;error.value='';try{const d=await api('',{method:'POST',body:JSON.stringify({name:name.value,days:days.value,scopes:scopes.value})});created.value=d.token;name.value='';await load()}catch(e:any){error.value=e.message}finally{busy.value=false}}
async function revoke(item:any){if(!await dialog.confirm('撤销后，此系统将无法再使用这个密钥查询。确认撤销？'))return;busy.value=true;error.value='';try{await api('/'+item.id,{method:'DELETE'});await load()}catch(e:any){error.value=e.message}finally{busy.value=false}}
async function copyCreated(){dialog.toast(await copyToClipboard(created.value)?'已复制':'复制失败，请手动选择复制')}
function date(n:number){return n?new Date(n*1000).toLocaleString():'尚未使用'}
onMounted(load)
</script>
<style scoped>td,th{padding:12px;border-bottom:1px solid var(--brd);white-space:nowrap}</style>
