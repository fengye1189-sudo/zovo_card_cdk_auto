<template>
  <div class="space-y-6">
    <div class="flex flex-wrap items-center justify-between gap-3"><h1 class="text-2xl font-bold">商品与套餐</h1><button v-if="auth.can('catalog.write')" class="btn-primary" @click="create">新增商品</button></div>
    <p class="text-muted">商品可独立设置名称、说明、有效期和启停。支持 Plus、Go、Pro 5X 和 Pro 20X。Go 每单上限 350 PHP，Pro 5X 每单上限 6,500 PHP，Pro 20X 每单上限 9,500 PHP；Plus 保持通道设置。开卡、补款和每日预算不变；参考售价只用于记录，实际充值仍按通道报价和原有限额执行。</p>
    <div v-if="error" class="alert alert-error" role="alert">{{ error }}</div><div v-if="notice" role="status" class="text-green-700">{{ notice }}</div>
    <section v-if="editing && auth.can('catalog.write')" class="card space-y-4">
      <h2 class="text-xl font-bold">{{ form.version ? '编辑商品' : '新增商品' }}</h2>
      <div class="grid sm:grid-cols-2 gap-4">
        <label>商品编号<input v-model="form.id" class="input mt-2" :disabled="form.version>0" placeholder="例如 plus-team" maxlength="48" /></label>
        <label>商品名称<input v-model="form.name" class="input mt-2" maxlength="80" /></label>
        <label>兑换套餐<select v-model="form.plan" class="input mt-2"><option value="plus">ChatGPT Plus</option><option value="go">ChatGPT Go</option><option value="pro_5x">GPTPRO5x卡冲升级</option><option value="pro_20x">ChatGPT Pro 20X</option></select></label>
        <label>卡密有效期<input value="固定 90 天" readonly class="input mt-2" /></label>
        <label>参考售价（最小货币单位，0 为未设置）<input v-model.number="form.reference_price_minor" type="number" min="0" class="input mt-2" /></label>
        <label>参考售价币种<input v-model="form.currency" class="input mt-2" maxlength="3" placeholder="USD" /></label>
      </div>
      <label class="block">商品说明<textarea v-model="form.description" class="input mt-2" maxlength="1000" rows="3" /></label>
      <label class="flex gap-2"><input v-model="form.enabled" type="checkbox" />启用商品（停用后停止发码，并暂停该商品未使用卡密的兑换）</label>
      <div class="flex gap-3"><button class="btn-primary" :disabled="busy" @click="save">{{ busy?'保存中…':'保存商品' }}</button><button class="btn-secondary" :disabled="busy" @click="editing=false">取消</button></div>
    </section>
    <div class="grid md:grid-cols-2 gap-4">
      <article v-for="product in products" :key="product.id" class="card space-y-3">
        <div class="flex justify-between gap-2"><h2 class="font-bold text-lg">{{ product.name }}</h2><span :class="product.enabled?'text-green-700':'text-muted'">{{ product.enabled?'启用中':'已停用' }}</span></div>
        <p class="text-sm text-muted">{{ product.id }} · {{ localPlanLabel(product.plan) }} · 有效期 {{ product.default_days }} 天</p><p>{{ product.description || '暂无说明' }}</p>
        <p class="text-sm">参考售价：{{ product.reference_price_minor ? product.reference_price_minor+' '+product.currency+'（最小货币单位）' : '未设置' }}</p>
        <button v-if="auth.can('catalog.write')" class="btn-secondary" @click="edit(product)">编辑 / 启停</button>
      </article>
    </div><p v-if="loading" role="status">正在读取商品…</p>
  </div>
</template>
<script setup lang="ts">
import { localPlanLabel } from '../../lib/local-plans'
import { onMounted, reactive, ref } from 'vue'
import { authFetch } from '../../lib/api'
import { useAuthStore } from '../../stores/auth'
const auth=useAuthStore()
type Product={id:string;name:string;description:string;plan:string;enabled:boolean;default_days:number;reference_price_minor:number;currency:string;version:number;updated_at:number}
const defaults=():Product=>({id:'',name:'',description:'',plan:'plus',enabled:true,default_days:90,reference_price_minor:0,currency:'USD',version:0,updated_at:0})
const products=ref<Product[]>([]),form=reactive(defaults()),editing=ref(false),busy=ref(false),loading=ref(false),error=ref(''),notice=ref('')
async function load(){loading.value=true;try{const r=await authFetch('/api/v1/admin/operations/products');const d=await r.json();if(!r.ok)throw Error(d.error||'读取失败');products.value=d.list}catch(e:any){error.value=e.message}finally{loading.value=false}}
function create(){Object.assign(form,defaults());editing.value=true;notice.value='';error.value=''}
function edit(p:Product){Object.assign(form,p);editing.value=true;notice.value='';error.value='';window.scrollTo({top:0,behavior:'smooth'})}
async function save(){if(busy.value)return;busy.value=true;error.value='';notice.value='';try{const r=await authFetch('/api/v1/admin/operations/products'+(form.version?'/'+encodeURIComponent(form.id):''),{method:form.version?'PUT':'POST',body:JSON.stringify(form)});const d=await r.json();if(!r.ok)throw Error(d.error||'保存失败');editing.value=false;notice.value='商品已保存';await load()}catch(e:any){error.value=e.message}finally{busy.value=false}}
onMounted(load)
</script>
