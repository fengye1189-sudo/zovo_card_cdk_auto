<template>
  <main class="min-h-screen py-10 sm:py-14"><div class="max-w-2xl mx-auto px-5 space-y-6">
    <header class="flex justify-between gap-4"><div><h1 class="text-3xl font-bold text-ink mb-2">枫叶兑换站</h1><p class="text-muted">输入卡密，按提示完成账户升级</p><a class="app-link inline-flex mt-2" :href="supportUrl" target="_blank" rel="noopener noreferrer">💬 联系客服</a></div><ThemeToggle /></header>
    <div v-if="step < 4" class="grid grid-cols-3 gap-2 text-sm text-center" aria-label="兑换进度"><span v-for="(label,i) in ['CDK 验证','提交账号','确认提交']" :key="label" class="pill" :class="step===i+1?'pill-info':''" :aria-current="step===i+1?'step':undefined">{{ i+1 }}. {{ label }}</span></div>
    <p v-if="error" class="alert alert-error" role="alert">{{ error }}</p>
    <section v-if="replacementCode" class="card space-y-3"><h2 class="text-xl font-bold">换码成功，请保存新卡密</h2><textarea class="input font-mono" readonly :value="replacementCode" /><p>有效期至 {{ new Date(replacementExpires*1000).toLocaleDateString() }}，旧卡密已失效。</p><button type="button" class="btn-secondary" @click="saveReplacement">下载新卡密</button><button type="button" class="btn-primary ml-2" @click="useReplacement">使用新卡密</button></section>
    <form v-if="step===1" class="card space-y-4" @submit.prevent="preview">
      <label for="local-code" class="block text-xl font-bold">验证 CDK 卡密</label>
      <input id="local-code" v-model="code" class="input font-mono" placeholder="请输入或粘贴卡密（例如 PRO5X-…）" autocomplete="off" spellcheck="false" maxlength="100" />
      <button class="btn-primary w-full" :disabled="busy || !code.trim()">{{ busy?'验证中…':'验证 CDK' }}</button>
      <button type="button" class="btn-secondary w-full" :disabled="busy || !code.trim()" @click="replaceCode">更换未使用卡密</button>
      <p class="text-sm text-muted">换同产品新码，旧码立即失效；仅限未使用、未提交付款且未过期的卡密。换码不延长原有效期。</p>
      <p class="text-sm text-muted">已提交过？重新输入同一张卡密即可查询进度。</p>
    </form>
    <form v-else-if="step===2" class="card space-y-4" @submit.prevent="preflight">
      <h2 class="text-xl font-bold">提交要升级的账号</h2>
      <p v-if="previewPlan" class="alert alert-info">该卡密对应商品：{{ localPlanLabel(previewPlan) }}</p>
      <button type="button" class="app-link text-sm" :disabled="busy" @click="step=1">返回卡密页 / 更换未使用卡密</button>
      <p class="text-muted text-sm">请粘贴该账号的完整 Session 信息，仅用于本次充值处理与账号核验。本站不保存原文；不要填写邮箱密码，也不要在公共设备处理登录凭证。</p>
      <a class="app-link inline-flex items-center gap-1 text-sm font-medium" href="https://chatgpt.com/" target="_blank" rel="noopener noreferrer"><div style="margin: 10px 0; background: #fff7ed; border: 1px solid #ffedd5; border-radius: 8px; padding: 12px; font-size: 13px; color: #9a3412;">
  <div style="display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 8px;">
    <span>💡 <strong>不知道如何获取凭证？</strong></span>
    <a 
      href="https://chatgpt.com/api/auth/session" 
      target="_blank" 
      rel="noopener"
      style="display: inline-block; background-color: #ea580c; color: #ffffff; text-decoration: none; padding: 6px 14px; border-radius: 6px; font-weight: bold; font-size: 13px;"
    >
      👉 点击一键打开凭证页面
    </a>
  </div>
  <div style="margin-top: 8px; padding-top: 6px; border-top: 1px dashed #fed7aa; font-size: 12px; color: #7c2d12; line-height: 1.6;">
    1. 确保已在官网登录，点击上方橙色按钮打开凭据页面。<br>
    2. 在弹出的页面直接按键盘 <b>Ctrl + A</b>（全选），再按 <b>Ctrl + C</b>（复制）。<br>
    3. 回到本页粘贴到下方输入框（必须包含完整 JSON）。
  </div>
</div> <span aria-hidden="true">↗</span></a>
      <label for="local-session" class="block">账号 Session</label>
      <textarea id="local-session" v-model="session" class="input font-mono h-40" autocomplete="off" spellcheck="false" placeholder="粘贴完整 Session JSON" />
      <div class="flex gap-3"><button type="button" class="btn-secondary" :disabled="busy" @click="reset">返回</button><button class="btn-primary flex-1" :disabled="busy || !session.trim()">{{ busy?'验证账号中…':'验证账号' }}</button></div>
    </form>
    <section v-else-if="step===3" class="card space-y-4">
      <h2 class="text-xl font-bold">确认兑换</h2>
      <p v-if="account.dedicated_card" class="alert alert-info">{{ localPlanLabel(account.plan) }} 将由系统自动处理，提交后可在本页查询结果。</p>
      <dl class="space-y-3"><div><dt class="text-muted">充值账号</dt><dd class="font-bold break-all">{{ account.email }}</dd></div><div><dt class="text-muted">兑换套餐</dt><dd>{{ localPlanLabel(account.plan) }}</dd></div><div><dt class="text-muted">当前套餐</dt><dd>{{ account.current_plan || '未返回' }}</dd></div></dl>
      <p class="text-sm text-muted">请核对账号。提交后将开始真实充值，处理中不要重复操作。</p>
      <label class="flex gap-2"><input v-model="confirmed" type="checkbox" />我确认账号无误，并同意提交兑换</label>
      <div class="flex gap-3"><button class="btn-secondary" :disabled="busy" @click="step=2;confirmed=false">修改账号</button><button class="btn-primary flex-1" :disabled="busy || !confirmed" @click="redeem">{{ busy?'提交中…':'确认兑换' }}</button></div>
    </section>
    <section v-else class="card space-y-4" aria-live="polite">
      <h2 class="text-xl font-bold">{{ statusLabel }}</h2><p>{{ result.message || '正在查询订单结果…' }}</p><p v-if="isProcessing" class="alert alert-info">正在自动刷新结果，请勿重复提交；完成后这里会自动显示“已完成”。</p><p v-if="result.email" class="break-all">充值账号：{{ result.email }}</p>
      <dl v-if="result.status==='completed'" class="space-y-3 rounded-xl border border-line p-4">
        <div><dt class="text-muted">升级类型</dt><dd class="font-bold">{{ result.upgrade_type || localPlanLabel(result.plan) }}</dd></div>
        <div><dt class="text-muted">开通日期</dt><dd>{{ formatResultDate(result.activated_at) }}</dd></div>
        <div><dt class="text-muted">{{ result.expiry_estimated ? '预计到期日期' : '到期日期' }}</dt><dd>{{ formatResultDate(result.subscription_expires_at) }}</dd></div>
      </dl>
      <a v-if="['failed','review'].includes(result.status)" class="btn-primary w-full inline-flex justify-center" :href="result.support_url || supportUrl" target="_blank" rel="noopener noreferrer">💬 联系客服处理</a>
      <button class="btn-secondary" :disabled="busy" @click="query">{{ busy?'正在刷新…':'立即刷新' }}</button><button class="app-link ml-4" :disabled="busy" @click="reset">查询其他卡密</button>
      <p class="text-sm text-muted">刷新页面不会重复提交处理。升级失败的卡密会永久锁定，不会再次兑换；待核对不代表失败退款。</p>
    </section>
  </div></main>
</template>
<script setup lang="ts">
import { localPlanLabel } from '../../lib/local-plans'
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import ThemeToggle from '../../components/ThemeToggle.vue'
import { dialog } from '../../lib/dialog'
import { normalizeLocalCode, isLocalCode, safeStorage, secureBrowserID } from '../../lib/local-code.mjs'
const props=defineProps<{initialCode?:string}>()
const supportUrl='https://t.me/fengye1189'
const localStorage=safeStorage('localStorage'),sessionStorage=safeStorage('sessionStorage')
const step=ref(1),code=ref(''),session=ref(''),token=ref(''),pf=ref(''),busy=ref(false),error=ref(''),confirmed=ref(false),previewPlan=ref(''),account=ref<any>({}),result=ref<any>({})
const route=useRoute()
const router=useRouter()
const replacementCode=ref(''),replacementExpires=ref(0)
let replacementRequest=''
let replacementOriginal=''
try{const saved=JSON.parse(sessionStorage.getItem('maple-code-replacement')||'null');if(saved){replacementRequest=saved.request;replacementOriginal=saved.original;code.value=saved.original;replacementCode.value=saved.code||'';replacementExpires.value=saved.expires||0}}catch{}
async function replaceCode(){
 if(busy.value)return
 if(!await dialog.confirm('确认换取同产品新卡密？旧码将立即失效，到期日不变。已使用或提交付款的卡密不能更换。'))return
 await run(async()=>{
  const original=normalizeLocalCode(code.value)
  if(replacementOriginal!==original||!replacementRequest){replacementOriginal=original;replacementRequest=secureBrowserID()}
  sessionStorage.setItem('maple-code-replacement',JSON.stringify({original,request:replacementRequest}))
  const d=await api('replace',{code:original,request_id:replacementRequest,confirmed:true})
  replacementCode.value=d.code;replacementExpires.value=d.expires_at
  sessionStorage.removeItem('maple-redemption');token.value='';pf.value='';session.value=''
  sessionStorage.setItem('maple-code-replacement',JSON.stringify({original,request:replacementRequest,code:d.code,expires:d.expires_at}))
 })
}
function saveReplacement(){const blob=new Blob([replacementCode.value+'\n'],{type:'text/plain;charset=utf-8'});const url=URL.createObjectURL(blob);const a=document.createElement('a');a.href=url;a.download='新卡密.txt';a.click();URL.revokeObjectURL(url)}
function useReplacement(){code.value=replacementCode.value;replacementCode.value='';sessionStorage.removeItem('maple-code-replacement');replacementRequest='';replacementOriginal='';preview()}
function formatResultDate(value:any){const seconds=Number(value);if(!Number.isFinite(seconds)||seconds<=0)return '正在确认';return new Date(seconds*1000).toLocaleDateString('zh-CN',{year:'numeric',month:'2-digit',day:'2-digit'})}
const device=localStorage.getItem('maple-device') || secureBrowserID();localStorage.setItem('maple-device',device)
let timer:ReturnType<typeof setTimeout>|undefined
const isProcessing=computed(()=>step.value===4 && result.value.status==='pending')
const statusLabel=computed(()=>({completed:'已完成',pending:'正在处理中',review:'订单待核对',failed:'订单未完成',unused:'尚未提交充值',query_expired:'查询期已结束'} as Record<string,string>)[result.value.status] || '兑换进度')
class ApiError extends Error{status:number;constructor(message:string,status:number){super(message);this.status=status}}
async function api(path:string,body:any){const r=await fetch('/api/v1/public/local-cdk/'+path,{method:'POST',headers:{'Content-Type':'application/json','X-Redemption-Device':device},body:JSON.stringify(body),cache:'no-store'});const d=await r.json();if(!r.ok){if(r.status===401){reset()}throw new ApiError(d.error || '服务暂时不可用，请稍后再试',r.status)}return d}
async function run(action:()=>Promise<void>){if(busy.value)return;busy.value=true;error.value='';try{await action()}catch(e:any){error.value=e.message || '网络连接异常，请稍后再试'}finally{busy.value=false}}
function credential(){return {mode:'session',session:session.value.trim()}}
async function preview(){await run(async()=>{code.value=normalizeLocalCode(code.value);const d=await api('preview',{code:code.value});token.value=d.redemption_token;previewPlan.value=d.plan||'';sessionStorage.setItem('maple-redemption',token.value);if(d.status!=='unused'){step.value=4;await refresh()}else{step.value=2}})}
async function preflight(){await run(async()=>{account.value=await api('preflight',{redemption_token:token.value,credential:credential()});pf.value=account.value.preflight_token;confirmed.value=false;step.value=3})}
async function redeem(){if(!confirmed.value)return;await run(async()=>{try{result.value=await api('redeem',{redemption_token:token.value,preflight_token:pf.value,credential:credential(),confirmed:true});step.value=4;schedule()}catch(e){if(e instanceof ApiError&&e.status===409){try{result.value=await api('result',{redemption_token:token.value});if(result.value.status==='unused'){step.value=2;throw e}}catch(check){if(check===e)throw e}}step.value=4;schedule();throw e}finally{session.value='';pf.value='';confirmed.value=false}})}
function schedule(){
 if(timer)clearTimeout(timer)
 if(!isProcessing.value || document.visibilityState==='hidden')return
 timer=setTimeout(()=>{timer=undefined;void query()},6000)
}
async function refresh(){try{result.value=await api('result',{redemption_token:token.value});if(result.value.status==='unused'){step.value=2;error.value='尚未提交充值，请重新验证账号。'}}catch(e){if(e instanceof ApiError&&(e.status===404||e.status===410)){result.value={status:'query_expired',message:e.message};error.value=e.message;token.value='';sessionStorage.removeItem('maple-redemption');if(timer)clearTimeout(timer);timer=undefined;return}throw e}finally{schedule()}}
async function query(){await run(refresh)}
function reset(){if(timer)clearTimeout(timer);sessionStorage.removeItem('maple-redemption');step.value=1;token.value='';pf.value='';session.value='';code.value='';previewPlan.value='';error.value='';result.value={};confirmed.value=false}
function refreshWhenVisible(){if(document.visibilityState==='visible'&&isProcessing.value)void query()}
onMounted(()=>{
  document.addEventListener('visibilitychange',refreshWhenVisible)
  const legacyCode=String(route.query.code || '')
  const supplied=normalizeLocalCode(props.initialCode || legacyCode)
  if(legacyCode){
    const cleanQuery={...route.query}
    delete cleanQuery.code
    void router.replace({path:route.path,query:cleanQuery,hash:route.hash})
  }
  if(isLocalCode(supplied)){
    code.value=supplied
    preview()
    return
  }
  const saved=sessionStorage.getItem('maple-redemption')
  if(saved){token.value=saved;step.value=4;query()}
})
onUnmounted(()=>{if(timer)clearTimeout(timer);document.removeEventListener('visibilitychange',refreshWhenVisible);session.value=''})
</script>
