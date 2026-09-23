<template>
 <section class="card space-y-4">
  <div class="flex flex-wrap justify-between gap-3"><h2 class="text-xl font-bold">邮箱 / 手机提醒</h2><el-button :loading="loading" @click="load">刷新通知记录</el-button></div>
  <el-alert v-if="error" :title="error" type="error" :closable="false" />
  <p class="text-sm text-muted">关闭页面仍可发送。只发送异常提醒，不包含客户账号、卡密、卡号或 Session。同一配置下相同异常只通知一次；更改配置后会重新通知当前未解决异常。</p>
  <el-form label-position="top" :disabled="!ready || saving">
   <div class="grid gap-4 sm:grid-cols-2"><el-form-item label="开启外部通知"><el-switch v-model="settings.enabled" /></el-form-item><el-form-item label="接收方式"><el-select v-model="settings.channel" @change="()=>{settings.secret='';hasSecret=false}"><el-option label="邮箱（Resend 发送服务）" value="email" /><el-option label="Telegram（手机通知，不是短信）" value="telegram" /></el-select></el-form-item>
    <el-form-item :label="settings.channel==='telegram'?'Telegram Chat ID':'接收邮箱'"><el-input v-model="settings.recipient" maxlength="254" autocomplete="off" /></el-form-item>
    <el-form-item v-if="settings.channel==='email'" label="Resend 已验证发件邮箱"><el-input v-model="settings.from" maxlength="254" autocomplete="off" /></el-form-item>
    <el-form-item :label="settings.channel==='telegram'?'Telegram Bot Token':'Resend API Key'"><el-input v-model="settings.secret" type="password" autocomplete="new-password" maxlength="200" :placeholder="hasSecret?'已保存，留空保持原凭证':'仅在此输入，不要发到聊天中'" /></el-form-item>
   </div>
   <p class="text-sm text-muted mb-3">{{ settings.channel==='telegram'?'需要先在 Telegram 创建机器人，并由接收账号先向机器人发送消息。':'需要 Resend 账户和已验证的发件域名；这里不是填写邮箱登录密码。发送服务可能有额度或费用，请以服务商账户为准。' }}</p>
   <div class="flex flex-wrap gap-3"><el-button type="primary" :loading="saving" @click="save">确认并保存通知</el-button><el-button :disabled="!savedEnabled || saving" :loading="testing" @click="test">向已保存的接收地址发送测试</el-button></div>
  </el-form>
  <el-table :data="records" empty-text="尚未发送通知"><el-table-column label="状态" min-width="145"><template #default="{row}">{{ status(row.state) }}</template></el-table-column><el-table-column prop="attempts" label="发送次数" width="100" /><el-table-column prop="error" label="说明" min-width="280" /><el-table-column label="更新时间" min-width="200"><template #default="{row}">{{ new Date(row.updated_at*1000).toLocaleString() }}</template></el-table-column></el-table>
 </section>
</template>
<script setup lang="ts">
import {onMounted,reactive,ref} from 'vue'
import {authFetch} from '../../lib/api'
import {dialog} from '../../lib/dialog'
const settings=reactive({enabled:false,channel:'email',recipient:'',from:'',secret:''}),version=ref(0),hasSecret=ref(false),ready=ref(false),records=ref<any[]>([]),error=ref(''),saving=ref(false),testing=ref(false),loading=ref(false),savedEnabled=ref(false)
async function api(path='',options:RequestInit={}){const r=await authFetch('/api/v1/admin/automation/notifications'+path,options);const d=await r.json();if(!r.ok)throw new Error(d.error||'通知请求失败');return d}
function apply(d:any){Object.assign(settings,d.settings,{secret:''});if(!settings.channel)settings.channel='email';version.value=d.version;hasSecret.value=d.secret_configured;records.value=d.records;savedEnabled.value=d.settings.enabled;ready.value=true}
async function load(){if(loading.value||saving.value)return;loading.value=true;try{apply(await api())}catch(e:any){error.value=e.message}finally{loading.value=false}}
async function save(){if(saving.value)return;saving.value=true;error.value='';try{if(!await dialog.confirm(settings.enabled?`启用后会向 ${settings.recipient} 发送当前未解决的异常和今后的新异常。确认授权发送？`:'确认关闭外部通知？已发送的通知无法撤回。',{title:'通知授权',okText:'确认保存'}))return;apply(await api('',{method:'PUT',body:JSON.stringify({settings,version:version.value,confirmed:true})}));dialog.toast('通知配置已保存')}catch(e:any){error.value=e.message}finally{saving.value=false}}
async function test(){if(testing.value)return;testing.value=true;try{if(!await dialog.confirm('向服务器已保存的接收地址发送一条测试消息；尚未保存的页面修改不会生效。确认发送？',{title:'测试通知',okText:'确认发送'}))return;const d=await api('/test',{method:'POST',body:JSON.stringify({confirmed:true})});dialog.toast(d.message)}catch(e:any){error.value=e.message}finally{testing.value=false}}
function status(v:string){return ({pending:'等待发送',sending:'正在发送',accepted:'平台已接收',failed:'发送失败',unknown:'结果不明，不重发',retry:'等待限次重试',cancelled:'已取消旧通知'} as Record<string,string>)[v]||v}
onMounted(load)
</script>
