<template>
  <div class="space-y-6 min-w-0">
    <div>
      <h1 class="text-2xl font-bold">客户订阅与到期</h1>
      <p class="text-sm text-muted mt-2">数据来自兑换站已确认成功的记录；同一账号只统计最近一次升级，续费或换套餐不会重复计算。</p>
    </div>

    <div v-if="error" class="alert alert-error" role="alert">{{ error }}</div>
    <div v-if="expiryNotice" class="alert alert-warning text-sm" role="note">{{ expiryNotice }}</div>

    <section class="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
      <article v-for="item in planCards" :key="item.plan" class="card space-y-4">
        <h2 class="text-lg font-bold">{{ planLabel(item.plan) }}</h2>
        <div class="grid grid-cols-3 gap-2 text-center">
          <button class="metric" :class="{ 'metric-active': isSelected(item.plan, 'active') }" type="button" :disabled="loading" @click="toggleSegment(item.plan, 'active')"><strong>{{ item.valid_accounts }}</strong><span>有效账号</span></button>
          <button class="metric" :class="{ 'metric-active': isSelected(item.plan, 'new_today') }" type="button" :disabled="loading" @click="toggleSegment(item.plan, 'new_today')"><strong>{{ item.new_today }}</strong><span>今日新增</span></button>
          <button class="metric" :class="{ 'metric-active': isSelected(item.plan, 'expiring_3d') }" type="button" :disabled="loading" @click="toggleSegment(item.plan, 'expiring_3d')"><strong>{{ item.expiring_3d }}</strong><span>3天内到期</span></button>
        </div>
      </article>
    </section>

    <section class="grid gap-4 sm:grid-cols-3">
      <button class="card text-left" :class="{ 'summary-active': isSelected('', 'active') }" type="button" :disabled="loading" @click="toggleSegment('', 'active')"><p class="text-sm text-muted">合计 · 有效账号</p><p class="mt-2 text-3xl font-bold mono">{{ totals.valid_accounts }}</p></button>
      <button class="card text-left" :class="{ 'summary-active': isSelected('', 'new_today') }" type="button" :disabled="loading" @click="toggleSegment('', 'new_today')"><p class="text-sm text-muted">合计 · 今日新增</p><p class="mt-2 text-3xl font-bold mono">{{ totals.new_today }}</p></button>
      <button class="card text-left" :class="{ 'summary-active': isSelected('', 'expiring_3d') }" type="button" :disabled="loading" @click="toggleSegment('', 'expiring_3d')"><p class="text-sm text-muted">合计 · 3天内到期</p><p class="mt-2 text-3xl font-bold mono">{{ totals.expiring_3d }}</p></button>
    </section>

    <section v-if="detailsOpen" class="card space-y-4">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div><h2 class="text-xl font-bold">{{ detailTitle }}</h2><p class="text-sm text-muted mt-1">点击上方其他数字可切换；再次点击当前数字即可收起。</p></div>
        <div class="flex gap-2"><button class="btn-secondary" type="button" :disabled="loading || exporting" @click="exportCurrent">{{ exporting ? '导出中…' : '导出当前名单' }}</button><button class="btn-secondary" type="button" @click="collapse">收起</button></div>
      </div>
      <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-5">
        <label>搜索升级账号<input v-model.trim="filters.q" class="input mt-1" placeholder="升级使用的邮箱" autocomplete="off" @keyup.enter="search" /></label>
        <label>升级类型<select v-model="filters.plan" class="input mt-1"><option value="">全部套餐</option><option value="plus">Plus</option><option value="go">Go</option><option value="pro_5x">Pro 5X</option><option value="pro_20x">Pro 20X</option></select></label>
        <label>客户状态<select v-model="filters.state" class="input mt-1"><option value="active">有效</option><option value="expired">已到期</option><option value="all">全部</option></select></label>
        <label>到期范围<select v-model.number="filters.expiry_days" class="input mt-1"><option :value="0">不限</option><option :value="3">3天内</option><option :value="7">7天内</option><option :value="15">15天内</option></select></label>
        <label>日期时区<select v-model="filters.timezone" class="input mt-1"><option value="Asia/Bangkok">曼谷时间（UTC+7）</option><option value="UTC">UTC</option></select></label>
      </div>
      <div class="flex flex-wrap gap-3 items-center">
        <button class="btn-primary" type="button" :disabled="loading" @click="search">{{ loading ? '查询中…' : '查询' }}</button>
        <button class="btn-secondary" type="button" :disabled="loading" @click="reset">重置</button>
        <span class="text-sm text-muted">共 {{ total }} 个当前客户账号</span>
      </div>
      <p v-if="dataQuality.missing_email || dataQuality.missing_dates" class="alert alert-warning text-sm">
        有 {{ dataQuality.missing_email }} 条成功记录缺少账号，{{ dataQuality.missing_dates }} 条缺少开通或到期日期，未纳入客户统计。
      </p>
      <p v-if="identityNotice" class="alert alert-warning text-sm">{{ identityNotice }}</p>
    </section>

    <section v-if="detailsOpen" class="card space-y-4">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <h2 class="text-xl font-bold">客户列表</h2>
        <label>每页 <select v-model.number="pageSize" class="input !w-auto" :disabled="loading" @change="search"><option :value="25">25</option><option :value="50">50</option><option :value="100">100</option></select></label>
      </div>
      <p v-if="loading" role="status">正在读取兑换记录…</p>
      <div class="space-y-3 md:hidden">
        <article v-for="row in rows" :key="row.id" class="rounded-xl border p-4 space-y-2 break-words">
          <div class="flex justify-between gap-3"><strong class="break-all">升级邮箱：{{ row.email }}</strong><span>{{ planLabel(row.plan) }}</span></div>
          <p>购买人：{{ row.buyer_name || '未记录' }} <span class="text-xs text-muted">{{ identityLabel(row) }}</span></p>
          <p class="break-all">联系邮箱：{{ row.buyer_email || row.email }} <span class="text-xs text-muted">{{ identityLabel(row) }}</span></p>
          <p>纸飞机：<a v-if="telegramLink(row)" class="text-primary" :href="telegramLink(row)">{{ telegramLabel(row) }}</a><span v-else>未绑定</span><span v-if="row.telegram_id"> · ID {{ row.telegram_id }}</span></p>
          <p>开通：{{ date(row.activated_at) }}</p>
          <p>{{ row.expiry_estimated ? '预计到期' : '到期' }}：{{ date(row.subscription_expires_at) }}</p>
          <p :class="remainingTone(row)">{{ remaining(row) }}</p>
          <p class="text-sm text-muted">兑换记录 #{{ row.id }} · 商城订单 {{ shortOrder(row.marketplace_order_id) }} · 上游 {{ row.upstream_id || '—' }}</p>
        </article>
      </div>
      <div class="hidden md:block overflow-x-auto">
        <table class="w-full text-sm text-left">
          <thead><tr><th class="py-3">升级邮箱</th><th>购买人</th><th>纸飞机</th><th>联系邮箱</th><th>升级类型</th><th>开通 / 到期</th><th>剩余时间</th><th>订单对应</th></tr></thead>
          <tbody><tr v-for="row in rows" :key="row.id" class="border-t">
            <td class="py-4 pr-4 font-mono break-all">{{ row.email }}</td>
            <td class="pr-4">{{ row.buyer_name || '未记录' }}<br /><span class="text-xs text-muted">{{ identityLabel(row) }}</span></td>
            <td class="pr-4 whitespace-nowrap"><a v-if="telegramLink(row)" class="text-primary" :href="telegramLink(row)">{{ telegramLabel(row) }}</a><span v-else>未绑定</span><br /><span v-if="row.telegram_id" class="text-xs text-muted">ID {{ row.telegram_id }}</span></td>
            <td class="pr-4 font-mono break-all">{{ row.buyer_email || row.email }}<br /><span class="text-xs text-muted">{{ identityLabel(row) }}</span></td>
            <td class="pr-4">{{ row.upgrade_type || planLabel(row.plan) }}</td>
            <td class="pr-4 whitespace-nowrap">开通 {{ date(row.activated_at) }}<br />{{ row.expiry_estimated ? '预计到期' : '到期' }} {{ date(row.subscription_expires_at) }}</td>
            <td class="pr-4 whitespace-nowrap" :class="remainingTone(row)">{{ remaining(row) }}</td>
            <td class="whitespace-nowrap">兑换 #{{ row.id }}<br /><span class="text-muted">商城 {{ shortOrder(row.marketplace_order_id) }}</span><br /><span class="text-muted">上游 {{ row.upstream_id || '—' }}</span></td>
          </tr></tbody>
        </table>
      </div>
      <p v-if="!loading && !rows.length" class="text-muted">没有符合条件的客户。</p>
      <div class="flex gap-3">
        <button class="btn-secondary" type="button" :disabled="loading || page <= 1" @click="load(page - 1)">上一页</button>
        <button class="btn-secondary" type="button" :disabled="loading || page * pageSize >= total" @click="load(page + 1)">下一页</button>
      </div>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { authFetch } from '../../lib/api'

type PlanSummary = { plan: string; valid_accounts: number; new_today: number; expiring_3d: number }
type Customer = {
  id: number
  email: string
  plan: string
  upgrade_type: string
  activated_at: number
  subscription_expires_at: number
  expiry_estimated: boolean
  upstream_id: number
  card_id: number
  marketplace_order_id: string
  buyer_name: string
  buyer_email: string
  telegram_id: string
  telegram_username: string
  identity_source: string
}

const defaults = () => ({ q: '', plan: '', state: 'active', expiry_days: 0, timezone: 'Asia/Bangkok', segment: '' })
const filters = reactive(defaults())
const planCards = ref<PlanSummary[]>(['plus', 'go', 'pro_5x', 'pro_20x'].map((plan) => ({ plan, valid_accounts: 0, new_today: 0, expiring_3d: 0 })))
const totals = reactive({ valid_accounts: 0, new_today: 0, expiring_3d: 0 })
const dataQuality = reactive({ completed: 0, missing_email: 0, missing_dates: 0 })
const rows = ref<Customer[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(25)
const generatedAt = ref(0)
const appliedTimezone = ref('Asia/Bangkok')
const expiryNotice = ref('')
const identityNotice = ref('')
const error = ref('')
const loading = ref(false)
const exporting = ref(false)
const detailsOpen = ref(false)

const segmentLabels: Record<string, string> = { active: '有效账号', new_today: '今日新增', expiring_3d: '3天内到期' }

const detailTitle = computed(() => {
  const scope = filters.plan ? planLabel(filters.plan) : '全部套餐'
  return `${scope} · ${segmentLabels[filters.segment] || '客户名单'}`
})

function planLabel(plan: string) {
  return ({ plus: 'Plus', go: 'Go', pro_5x: 'Pro 5X', pro_20x: 'Pro 20X' } as Record<string, string>)[plan] || plan
}

function date(value: number) {
  if (!value) return '—'
  return new Intl.DateTimeFormat('zh-CN', { timeZone: appliedTimezone.value, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false }).format(new Date(value * 1000))
}

function remaining(row: Customer) {
  const seconds = row.subscription_expires_at - (generatedAt.value || Math.floor(Date.now() / 1000))
  if (seconds <= 0) return `已到期 ${Math.max(1, Math.ceil(Math.abs(seconds) / 86400))} 天`
  const days = Math.ceil(seconds / 86400)
  return days <= 1 ? '24小时内到期' : `剩余 ${days} 天`
}

function remainingTone(row: Customer) {
  const seconds = row.subscription_expires_at - (generatedAt.value || Math.floor(Date.now() / 1000))
  if (seconds <= 0) return 'text-muted'
  return seconds <= 3 * 86400 ? 'text-warn font-semibold' : ''
}

function shortOrder(value: string) {
  if (!value) return '—'
  return value.length > 16 ? `${value.slice(0, 8)}…${value.slice(-6)}` : value
}

function telegramLabel(row: Customer) {
  return row.telegram_username ? `@${row.telegram_username.replace(/^@/, '')}` : `Telegram ID ${row.telegram_id}`
}

function telegramLink(row: Customer) {
  if (row.telegram_username) return `https://t.me/${row.telegram_username.replace(/^@/, '')}`
  return row.telegram_id ? `tg://user?id=${encodeURIComponent(row.telegram_id)}` : ''
}

function identityLabel(row: Customer) {
  return ({ marketplace_order: '商城订单确认', upgrade_email_match: '升级邮箱匹配商城账号', upgrade_email_inferred: '按升级邮箱推定' } as Record<string, string>)[row.identity_source] || ''
}

async function load(nextPage = 1) {
  if (loading.value) return
  loading.value = true
  error.value = ''
  try {
    const response = await authFetch('/api/v1/admin/operations/customers/search', {
      method: 'POST',
      body: JSON.stringify({ ...filters, page: nextPage, page_size: pageSize.value }),
    })
    const body = await response.json().catch(() => ({}))
    if (!response.ok) throw new Error(body.error || '客户数据读取失败')
    planCards.value = body.plans || []
    Object.assign(totals, body.totals || {})
    Object.assign(dataQuality, body.data_quality || {})
    rows.value = body.list || []
    total.value = Number(body.total || 0)
    page.value = Number(body.page || nextPage)
    generatedAt.value = Number(body.generated_at || 0)
    appliedTimezone.value = String(body.timezone || filters.timezone)
    expiryNotice.value = String(body.expiry_notice || '')
    identityNotice.value = String(body.identity_notice || '')
  } catch (cause: any) {
    error.value = cause?.message || '客户数据读取失败'
  } finally {
    loading.value = false
  }
}

function isSelected(plan: string, segment: string) {
  return detailsOpen.value && filters.plan === plan && filters.segment === segment
}

function collapse() {
  detailsOpen.value = false
}

function toggleSegment(plan: string, segment: string) {
  if (isSelected(plan, segment)) {
    collapse()
    return
  }
  filters.q = ''
  filters.plan = plan
  filters.state = 'active'
  filters.expiry_days = 0
  filters.segment = segment
  detailsOpen.value = true
  load(1)
}

function search() {
  filters.segment = ''
  detailsOpen.value = true
  load(1)
}

function reset() {
  Object.assign(filters, defaults())
  detailsOpen.value = true
  load(1)
}

function csvCell(value: unknown) {
  let text = String(value ?? '')
  if (/^[=+\-@]/.test(text)) text = `'${text}`
  return `"${text.replace(/"/g, '""')}"`
}

async function exportCurrent() {
  if (exporting.value) return
  exporting.value = true
  error.value = ''
  try {
    const response = await authFetch('/api/v1/admin/operations/customers/search', {
      method: 'POST',
      body: JSON.stringify({ ...filters, export: true, page: 1, page_size: 5000 }),
    })
    const body = await response.json().catch(() => ({}))
    if (!response.ok) throw new Error(body.error || '客户名单导出失败')
    const exportRows = (body.list || []) as Customer[]
    const header = ['升级邮箱', '购买人', '联系邮箱', '身份依据', 'Telegram用户名', 'Telegram ID', '升级类型', '开通时间', '到期时间', '到期类型', '剩余状态', '商城订单', '上游订单', '兑换记录']
    const lines = [header.map(csvCell).join(',')]
    for (const row of exportRows) {
      lines.push([
        row.email, row.buyer_name, row.buyer_email || row.email, identityLabel(row), row.telegram_username ? `@${row.telegram_username.replace(/^@/, '')}` : '', row.telegram_id,
        row.upgrade_type || planLabel(row.plan), date(row.activated_at), date(row.subscription_expires_at), row.expiry_estimated ? '预计到期' : '精确到期', remaining(row),
        row.marketplace_order_id, row.upstream_id || '', row.id,
      ].map(csvCell).join(','))
    }
    const blob = new Blob([`\uFEFF${lines.join('\r\n')}`], { type: 'text/csv;charset=utf-8' })
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    const planName = filters.plan ? planLabel(filters.plan) : '全部套餐'
    const segmentName = segmentLabels[filters.segment] || '筛选结果'
    link.href = url
    link.download = `客户跟踪-${planName}-${segmentName}-${new Date().toISOString().slice(0, 10)}.csv`
    document.body.appendChild(link)
    link.click()
    link.remove()
    URL.revokeObjectURL(url)
  } catch (cause: any) {
    error.value = cause?.message || '客户名单导出失败'
  } finally {
    exporting.value = false
  }
}

onMounted(() => load())
</script>

<style scoped>
.metric { display: flex; min-height: 76px; flex-direction: column; align-items: center; justify-content: center; gap: 4px; border-radius: 12px; background: var(--surface-2, var(--soft)); }
.metric strong { color: var(--primary); font-size: 1.65rem; line-height: 1; }
.metric span { color: var(--muted); font-size: .75rem; }
.metric:hover { outline: 1px solid var(--primary); }
.metric-active { outline: 2px solid var(--primary); background: color-mix(in srgb, var(--primary) 10%, var(--surface-2, var(--soft))); }
.summary-active { outline: 2px solid var(--primary); }
</style>
