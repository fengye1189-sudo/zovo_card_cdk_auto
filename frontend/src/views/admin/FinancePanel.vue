<template>
 <section class="card space-y-4">
  <div class="flex flex-wrap justify-between gap-3"><h2 class="text-xl font-bold">资金对账</h2><el-button :loading="busy" @click="load">刷新流水</el-button></div>
  <p class="text-sm text-muted">每 5 分钟同步最新钱包流水，并逐步补齐历史和卡片流水。仅查询，不充值、不退款。卡充值记录和卡资金流是同一业务的不同视图，不能相加计算支出。</p>
  <el-alert v-if="error || data.sync_error" :title="error || data.sync_error" type="warning" :closable="false" />
  <p v-if="source!=='transaction'" class="text-sm">上次成功轮询：{{ date(data.last_sync) }} · 已收录钱包流水 {{ data.imported_wallet || 0 }} / 上游当前 {{ data.upstream_total || 0 }} 条</p>
  <div class="flex flex-wrap gap-3"><el-select v-model="source" aria-label="流水类型" style="width:230px" @change="reset"><el-option label="平台钱包流水（USD）" value="wallet" /><el-option label="卡片充值记录（USD）" value="recharge" /><el-option label="卡片资金流（USD）" value="flow" /><el-option label="消费 / 商户退款（原币种）" value="transaction" /></el-select><el-select v-if="source!=='transaction'" v-model="state" aria-label="核对状态" style="width:200px" @change="reset"><el-option label="全部核对状态" value="" /><el-option label="金额不平" value="mismatch" /><el-option label="钱包金额一致" value="arithmetic_ok" /><el-option label="待核对关联" value="unlinked" /></el-select></div>
  <el-table v-if="source!=='transaction'" :data="data.list || []" empty-text="暂无已同步流水；后台会自动读取，请稍后刷新">
   <el-table-column label="流水 / 参考编号" min-width="150"><template #default="{row}">{{ row.entry.id }} / {{ row.entry.ref_id || '—' }}</template></el-table-column>
   <el-table-column label="卡片" min-width="135"><template #default="{row}">ID {{ row.entry.card_id || '—' }}<br>{{ row.entry.last4 ? '•••• '+row.entry.last4 : '' }}</template></el-table-column>
   <el-table-column label="类型 / 方向" min-width="155"><template #default="{row}">{{ row.entry.type || '充值' }} / {{ row.entry.direction || row.entry.wallet || '—' }}</template></el-table-column>
   <el-table-column label="金额 / 手续费" min-width="180"><template #default="{row}">{{ money(row.entry.amount_minor,row.entry.amount_decimal) }} / {{ money(row.entry.fee_minor,row.entry.fee_decimal) }}</template></el-table-column>
   <el-table-column label="钱包变化" min-width="230"><template #default="{row}">{{ money(row.entry.before_minor,row.entry.before_decimal) }} → {{ money(row.entry.after_minor,row.entry.after_decimal) }}</template></el-table-column>
   <el-table-column label="核对状态" min-width="175"><template #default="{row}">{{ checkState(row.check_state) }}<br>{{ row.entry.status }}</template></el-table-column>
   <el-table-column label="上游原始时间" min-width="210"><template #default="{row}">{{ row.entry.created_at || '未返回' }}</template></el-table-column>
  </el-table>
  <el-table v-else :data="data.list || []" empty-text="消费流水尚未同步或暂不存在">
   <el-table-column prop="card_id" label="卡片 ID" width="110" /><el-table-column prop="entry.auth_id" label="交易编号" min-width="180" /><el-table-column prop="entry.merchant_name" label="商户" min-width="150" />
   <el-table-column label="授权金额" min-width="160"><template #default="{row}">{{ row.entry.auth_amount ?? '未返回' }} {{ row.entry.auth_currency }}</template></el-table-column>
   <el-table-column label="结算金额" min-width="160"><template #default="{row}">{{ row.entry.settle_amount ?? '未返回' }} {{ row.entry.settle_currency }}</template></el-table-column>
   <el-table-column prop="entry.type" label="交易类型" min-width="135" /><el-table-column prop="entry.status" label="上游状态" min-width="135" /><el-table-column prop="entry.auth_time" label="上游原始时间" min-width="210" />
  </el-table>
  <el-pagination v-model:current-page="page" :page-size="20" :total="data.total || 0" layout="prev, pager, next, total" @current-change="load" />
  <el-alert title="钱包金额一致只代表「原余额 + 变动 = 新余额」，不代表已关联本站操作。上游未提供可靠的本站请求编号时，需要人工核对。上游未带时区的时间不作时区推断。" type="info" :closable="false" />
  <el-collapse><el-collapse-item title="记录人工核对结果（不会解除资金锁定）" name="review">
   <el-form label-position="top"><div class="grid gap-3 sm:grid-cols-2"><el-form-item label="本站自动资金操作编号"><el-input v-model="review.operation_id" maxlength="100" placeholder="从上方自动资金记录复制 auto- 开头编号" /></el-form-item><el-form-item label="对应的钱包流水编号"><el-input-number v-model="review.wallet_id" :min="1" :precision="0" /></el-form-item></div><el-form-item label="核对依据（不要填写卡号、密码或 Session）"><el-input v-model="review.note" type="textarea" maxlength="300" show-word-limit placeholder="说明已核对的上游记录、卡片和金额" /></el-form-item><el-button :loading="reviewing" @click="saveReview">确认并保存核对备注</el-button></el-form>
  </el-collapse-item></el-collapse>
  <el-table :data="data.reviews || []" empty-text="暂无人工核对备注"><el-table-column prop="operation_id" label="本站操作编号" min-width="240" /><el-table-column prop="wallet_id" label="钱包流水" width="110" /><el-table-column prop="note" label="核对依据" min-width="220" /><el-table-column prop="actor" label="操作人" width="120" /></el-table>
 </section>
</template>
<script setup lang="ts">
import {onMounted,ref} from 'vue'
import {authFetch} from '../../lib/api'
import {dialog} from '../../lib/dialog'
const data=ref<any>({}),page=ref(1),source=ref('wallet'),state=ref(''),busy=ref(false),error=ref(''),reviewing=ref(false)
const review=ref({operation_id:'',wallet_id:1,note:''})
let generation=0
async function load(){const turn=++generation;busy.value=true;error.value='';try{const r=await authFetch(`/api/v1/admin/automation/finance?page=${page.value}&source=${source.value}&state=${state.value}`);const d=await r.json();if(!r.ok)throw new Error(d.error||'读取失败');if(turn===generation)data.value=d}catch(e:any){if(turn===generation)error.value=e.message}finally{if(turn===generation)busy.value=false}}
function reset(){page.value=1;load()}
function money(v:any,raw?:string){return typeof v==='number'?'$'+(v/100).toFixed(2):raw?'$'+raw:'未返回'}
function date(v:number){return v?new Date(v*1000).toLocaleString():'等待首次同步'}
function checkState(v:string){return ({arithmetic_ok:'钱包金额一致，关联待核对',mismatch:'金额不平，请核查',precision_review:'上游含分以下小数，按原值待核对',unlinked:'待核对关联'} as Record<string,string>)[v]||v}
async function saveReview(){if(reviewing.value)return;reviewing.value=true;try{if(!await dialog.confirm('仅保存人工核对备注。不会退款、重新付款、恢复预算或解除结果不明的操作。确认已核对这笔钱包流水？',{title:'保存核对结果',okText:'确认保存'}))return;const r=await authFetch('/api/v1/admin/automation/finance/review',{method:'POST',body:JSON.stringify({...review.value,confirmed:true})});const d=await r.json();if(!r.ok)throw new Error(d.error||'保存失败');dialog.toast(d.message);await load()}catch(e:any){error.value=e.message}finally{reviewing.value=false}}
onMounted(load)
</script>
