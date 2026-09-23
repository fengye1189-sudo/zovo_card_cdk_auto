<template>
  <LocalRedeemView v-if="localEntryCode" :initial-code="localEntryCode" />
  <div v-else class="redeem-page min-h-screen py-8 sm:py-12">
    <div class="max-w-3xl mx-auto px-4 sm:px-6">
      <header class="maple-hero mb-6 sm:mb-8">
        <div class="maple-hero-copy">
          <div class="maple-brand"><span class="maple-mark" aria-hidden="true">🍁</span> MaplePass</div>
          <p class="maple-kicker">CDK REDEMPTION CENTER</p>
          <h1>枫叶兑换站</h1>
          <p class="maple-summary">安全核验卡密，自动提交开通请求，全程可查询处理进度。</p>
        </div>
        <div class="flex items-center gap-3">
          <LanguageToggle />
          <ThemeToggle />
        </div>
      </header>

      <div class="maple-assurance mb-6" role="note">
        <span class="maple-assurance-icon" aria-hidden="true">✓</span>
        <span><b>兑换前请确认账号归属。</b> 卡密仅在验证通过后提交，处理中请勿重复兑换。</span>
      </div>

      <RedeemModeTabs />

      <!-- redeem-flow v2: no public fee reference -->
      <!-- steps -->
      <div class="maple-steps card mb-6">
        <div class="flex gap-2 text-sm flex-wrap">
          <span v-for="(s, i) in steps" :key="s" class="maple-step" :class="{ active: step === i + 1, done: step > i + 1 }">
            <b>{{ String(i + 1).padStart(2, '0') }}</b><span>{{ s }}</span>
          </span>
        </div>
      </div>

      <!-- 1 preview -->
      <div v-show="step === 1" class="card redeem-card space-y-4">
        <div class="redeem-card-heading">
          <span class="redeem-card-icon" aria-hidden="true">⌁</span>
          <div>
            <p class="redeem-card-overline">STEP 01</p>
            <h2 class="text-xl font-bold text-ink">输入兑换码</h2>
          </div>
        </div>
        <label class="sr-only" for="cdk-code">CDK 兑换码</label>
        <input id="cdk-code" v-model="code" class="input mono maple-code-input" placeholder="SXC-XXXX-XXXX-XXXX-XXXX" autocomplete="off" @keyup.enter="doPreview" />
        <p class="text-xs text-muted">请粘贴完整 CDK；验证不会立即扣除卡密。</p>
        <router-link class="app-link text-sm" to="/maple/redeem">更换未使用的本站卡密</router-link>
        <div v-if="error" class="alert alert-error">{{ error }}</div>
        <div v-if="previewInfo" class="rounded-xl bg-soft p-4 text-sm space-y-1">
          <div>套餐：<b>{{ previewInfo.plan || previewInfo.plan_type || '—' }}</b></div>
          <div class="text-muted">{{ previewInfo.message || '码有效，可继续' }}</div>
        </div>
        <button class="btn-primary w-full" :disabled="busy" @click="doPreview">{{ busy ? '校验中…' : '预览 / 下一步' }}</button>
      </div>

      <!-- 2 preflight -->
      <div v-show="step === 2" class="card space-y-4">
        <h2 class="text-xl font-bold text-ink">ChatGPT 凭证</h2>
        <div class="flex gap-2">
          <button type="button" class="btn-secondary !py-1" :class="{ 'ring-2': credMode === 'session' }" @click="credMode = 'session'">Session</button>
          <button type="button" class="btn-secondary !py-1" :class="{ 'ring-2': credMode === 'mailbox' }" @click="credMode = 'mailbox'">邮箱</button>
        </div>
        <template v-if="credMode === 'session'">
        <div style="margin-bottom: 12px; background: #fff7ed; border: 1px solid #ffedd5; border-radius: 8px; padding: 12px; font-size: 13px; color: #9a3412;">
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
            3. 回到本页粘贴到下方输入框（必须包含完整 JSON，已禁用纯 Access Token）。
          </div>
        </div>
          <textarea v-model="sessionRaw" class="input h-36 font-mono text-xs" placeholder='{"user":{...},"accessToken":"eyJ...","sessionToken":"eyJ...五段JWE..."}' />
        </template>
        <template v-else>
          <input v-model="email" class="input" placeholder="email@outlook.com" />
          <input v-model="password" class="input" type="password" placeholder="邮箱密码" />
        </template>
        <div v-if="error" class="alert alert-error">{{ error }}</div>
        <div class="flex gap-3">
          <button class="btn-secondary flex-1" @click="step = 1">上一步</button>
          <button class="btn-primary flex-1" :disabled="busy" @click="doPreflight">{{ busy ? '预检中…' : '预检' }}</button>
        </div>
      </div>

      <!-- 3 redeem -->
      <div v-show="step === 3" class="card space-y-4">
        <h2 class="text-xl font-bold text-ink">确认兑换</h2>
        <p class="text-sm text-muted">将提交兑换请求；结果不确定或 review 时请勿重复提交，请轮询结果。</p>

        <!-- 与卡台直充预检一致：邮箱 + 订阅事实 -->
        <div v-if="account.checked" class="rounded-xl border p-4 space-y-3" style="border-color: var(--brd); background: var(--surface-2, var(--soft))">
          <div class="flex flex-wrap items-center gap-2">
            <strong class="text-ink text-base">{{ account.email || '账号已验证' }}</strong>
            <el-tag size="small" :type="subscriptionStatusTag">{{ subscriptionStatusText }}</el-tag>
          </div>
          <dl class="account-facts">
            <div>
              <dt>目标套餐</dt>
              <dd>{{ planLabel(targetPlan) }}</dd>
            </div>
            <div>
              <dt>当前套餐</dt>
              <dd>{{ planLabel(account.currentPlan) }}</dd>
            </div>
            <div>
              <dt>订阅状态</dt>
              <dd>{{ subscriptionStatusText }}</dd>
            </div>
            <div>
              <dt>套餐到期</dt>
              <dd>{{ account.subscriptionActiveUntil ? fmtTime(account.subscriptionActiveUntil) : '上游未提供' }}</dd>
            </div>
            <div>
              <dt>剩余</dt>
              <dd>{{ account.subscriptionActiveUntil ? subscriptionRemaining : '上游未提供' }}</dd>
            </div>
            <div>
              <dt>自动续费</dt>
              <dd :class="account.subscriptionWillRenew === true ? 'text-warn' : account.subscriptionWillRenew === false ? 'text-good' : ''">
                {{ renewalStatusText }}
              </dd>
            </div>
            <div>
              <dt>最近付款时间</dt>
              <dd>{{ lastPaymentAtText }}</dd>
            </div>
            <div>
              <dt>最近付款</dt>
              <dd>{{ lastPaymentText }}</dd>
            </div>
            <div>
              <dt>支付方式</dt>
              <dd>{{ paymentMethodText }}</dd>
            </div>
          </dl>
        </div>
        <div v-else class="rounded-xl bg-soft p-3 text-sm text-muted">
          预检未返回账号摘要，仍可尝试兑换（以卡台校验为准）。
        </div>

        <div v-if="alreadySatisfied" class="alert" style="background: var(--warn-soft, #fef3c7); color: var(--warn, #b45309); border-color: var(--warn, #d97706)">
          {{ alreadySatisfiedHint }}
        </div>

        <div v-if="account.subscriptionIsDelinquent === true" class="alert" style="background: var(--warn-soft, #fef3c7); color: var(--warn, #b45309); border-color: var(--warn, #d97706)">
          该账号有未结清账单（欠费）。仍可兑换：卡台会先取消原订阅再重新开通，但成功率低于正常账号。
        </div>

        <div v-if="error" class="alert alert-error">{{ error }}</div>
        <div class="flex gap-3">
          <button class="btn-secondary flex-1" @click="step = 2">上一步</button>
          <button class="btn-primary flex-1" :disabled="busy || alreadySatisfied" @click="doRedeem">
            {{ busy ? '提交中…' : alreadySatisfied ? '当前套餐已满足' : '兑换' }}
          </button>
        </div>
      </div>

      <!-- 4 result -->
      <div v-show="step === 4" class="card space-y-4">
        <h2 class="text-xl font-bold text-ink">兑换进度</h2>

        <div class="flex flex-wrap items-center gap-2">
          <el-tag :type="statusTagType(resultStatus)" size="large">{{ resultStatus || '—' }}</el-tag>
          <span v-if="resultStage" class="text-sm text-muted mono">阶段 {{ resultStage }}</span>
          <span v-if="polling" class="text-sm text-muted">
            <span class="inline-block animate-pulse">●</span> 实时轮询中（约 3s）
          </span>
          <span v-else-if="isTerminal(resultStatus)" class="text-sm" :class="resultStatus === 'completed' ? 'text-good' : 'text-muted'">
            已结束
          </span>
        </div>

        <div class="rounded-xl bg-soft p-3 text-sm space-y-2">
          <div class="flex justify-between gap-3 items-center">
            <span class="text-muted shrink-0">兑换账号</span>
            <span class="mono text-ink text-right break-all">{{ displayResultEmail || '—' }}</span>
          </div>
          <div class="flex justify-between gap-3 items-center">
            <span class="text-muted shrink-0">银行卡</span>
            <span class="mono text-ink">
              <template v-if="resultCardLastFour">•••• {{ resultCardLastFour }}</template>
              <template v-else><span class="text-muted">开卡后显示尾号</span></template>
            </span>
          </div>
        </div>

        <div v-if="resultMessage" class="rounded-xl bg-soft p-3 text-sm text-ink">
          {{ resultMessage }}
        </div>

        <!-- 进度步骤条（由 stage / events 推导） -->
        <div class="grid grid-cols-4 gap-2 text-center text-xs">
          <div
            v-for="p in progressSteps"
            :key="p.key"
            class="rounded-lg border px-2 py-2"
            :class="p.active ? 'border-primary bg-primary/10 text-ink font-semibold' : 'border-brd text-muted'"
          >
            {{ p.label }}
          </div>
        </div>

        <!-- 时间线明细（卡台 events） -->
        <div v-if="timeline.length" class="space-y-0">
          <div class="text-sm font-medium text-ink mb-2">处理明细</div>
          <ol class="space-y-3 border-l-2 pl-4" style="border-color: var(--brd)">
            <li v-for="(ev, idx) in timeline" :key="ev.id || idx" class="relative">
              <span
                class="absolute -left-[1.35rem] top-1 h-2.5 w-2.5 rounded-full"
                :style="{ background: eventDot(ev.category) }"
              />
              <div class="flex flex-wrap items-baseline gap-2">
                <b class="text-sm text-ink">{{ stepLabel(ev.step) }}</b>
                <el-tag size="small" :type="categoryTag(ev.category)">{{ ev.category || 'pending' }}</el-tag>
                <span class="text-xs text-subtle">{{ fmtTime(ev.created_at) }}</span>
              </div>
              <p class="text-sm text-muted mt-0.5">
                {{ ev.public_message || ev.to_status || '—' }}
              </p>
            </li>
          </ol>
        </div>
        <div v-else-if="polling" class="text-sm text-muted">
          已提交，等待卡台返回步骤明细…
        </div>

        <details class="text-xs text-muted">
          <summary class="cursor-pointer select-none">原始响应（调试）</summary>
          <pre class="rounded-xl bg-soft p-4 overflow-auto max-h-48 mt-2">{{ resultPretty }}</pre>
        </details>

        <div v-if="error" class="alert alert-error">{{ error }}</div>
        <div v-if="isTerminal(resultStatus) && resultStatus !== 'completed'" class="alert alert-error">
          兑换未成功。若状态为 review/pending 请勿重复提交；可联系发码方或稍后用同一设备再查结果。
        </div>
        <div v-if="resultStatus === 'completed'" class="alert alert-success">开通完成，请到 ChatGPT 账号确认套餐。</div>
        <button class="btn-secondary" @click="resetAll">再兑一张</button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import LanguageToggle from '../../components/LanguageToggle.vue'
import ThemeToggle from '../../components/ThemeToggle.vue'
import RedeemModeTabs from '../../components/RedeemModeTabs.vue'
import LocalRedeemView from './LocalRedeemView.vue'
import { normalizeLocalCode, isLocalCode } from '../../lib/local-code.mjs'
const localEntryCode=ref('')

const { t } = useI18n({ useScope: 'global' })
const route = useRoute()
const router = useRouter()
const steps = ['预览', '凭证', '兑换', '结果']
const step = ref(1)
const busy = ref(false)
const error = ref('')
const code = ref('')
const previewInfo = ref<any>(null)
const redemptionToken = ref('')
const attemptToken = ref('')
const preflightToken = ref('')
const credMode = ref<'session' | 'mailbox'>('session')
const sessionRaw = ref('')
const email = ref('')
const password = ref('')
/** 预检成功后的账号/订阅摘要（与卡台 GPT 直充 preflight 字段对齐） */
const account = ref({
  checked: false,
  email: '',
  currentPlan: '',
  subscriptionHasActive: null as boolean | null,
  subscriptionActiveUntil: '',
  canPurchaseAt: '',
  subscriptionWillRenew: null as boolean | null,
  subscriptionIsDelinquent: null as boolean | null,
  lastPayment: null as any,
  paymentMethod: null as any,
})
const resultStatus = ref('')
const resultStage = ref('')
const resultMessage = ref('')
const resultEmail = ref('')
const resultCardLastFour = ref('')
const resultBody = ref<any>(null)
const timeline = ref<any[]>([])
const polling = ref(false)
let pollTimer: any = null
let missingPolls = 0
const displayResultEmail = computed(() => resultEmail.value || account.value.email || '')

const PROGRESS_KEY = 'cdk_redeem_progress_v1'
const progressCreatedAt = ref(Date.now())
const nowTick = ref(Date.now())
let nowTimer: any = null

const deviceId = (() => {
  const k = 'cdk_device_id'
  let v = localStorage.getItem(k)
  if (!v) {
    v = 'web-' + Math.random().toString(36).slice(2) + Date.now().toString(36)
    localStorage.setItem(k, v)
  }
  return v
})()

function saveProgress() {
  try {
    const payload = {
      step: step.value,
      code: code.value,
      redemptionToken: redemptionToken.value,
      attemptToken: attemptToken.value,
      preflightToken: preflightToken.value,
      previewInfo: previewInfo.value,
      account: account.value,
      resultStatus: resultStatus.value,
      resultStage: resultStage.value,
      resultMessage: resultMessage.value,
      resultEmail: resultEmail.value,
      resultCardLastFour: resultCardLastFour.value,
      resultBody: resultBody.value,
      timeline: timeline.value,
      createdAt: progressCreatedAt.value,
      savedAt: Date.now(),
    }
    sessionStorage.setItem(PROGRESS_KEY, JSON.stringify(payload))
  } catch {
    /* ignore quota */
  }
}

function loadProgress(): boolean {
  try {
    const raw = sessionStorage.getItem(PROGRESS_KEY)
    if (!raw) return false
    const p = JSON.parse(raw)
    if (!p || typeof p !== 'object') return false
    // The local recovery record has an immutable lifetime too. Polling may
    // update savedAt for crash recovery, but must never renew createdAt.
    const createdAt = Number(p.createdAt || p.savedAt || 0)
    if (createdAt && Date.now() - createdAt >= 7 * 24 * 3600 * 1000) {
      sessionStorage.removeItem(PROGRESS_KEY)
      return false
    }
    progressCreatedAt.value = createdAt || Date.now()
    if (p.code) code.value = String(p.code)
    if (p.redemptionToken) redemptionToken.value = String(p.redemptionToken)
    if (p.attemptToken) attemptToken.value = String(p.attemptToken)
    if (p.preflightToken) preflightToken.value = String(p.preflightToken)
    if (p.previewInfo) previewInfo.value = p.previewInfo
    if (p.account && typeof p.account === 'object') {
      account.value = {
        checked: !!p.account.checked,
        email: String(p.account.email || ''),
        currentPlan: String(p.account.currentPlan || ''),
        subscriptionHasActive:
          typeof p.account.subscriptionHasActive === 'boolean' ? p.account.subscriptionHasActive : null,
        subscriptionActiveUntil: String(p.account.subscriptionActiveUntil || ''),
        canPurchaseAt: String(p.account.canPurchaseAt || ''),
        subscriptionWillRenew:
          typeof p.account.subscriptionWillRenew === 'boolean' ? p.account.subscriptionWillRenew : null,
        lastPayment: p.account.lastPayment || null,
        paymentMethod: p.account.paymentMethod || null,
      }
    }
    if (p.resultStatus) resultStatus.value = String(p.resultStatus)
    if (p.resultStage) resultStage.value = String(p.resultStage)
    if (p.resultMessage) resultMessage.value = String(p.resultMessage)
    if (p.resultEmail) resultEmail.value = String(p.resultEmail)
    if (p.resultCardLastFour) resultCardLastFour.value = String(p.resultCardLastFour)
    if (p.resultBody) resultBody.value = p.resultBody
    if (Array.isArray(p.timeline)) timeline.value = p.timeline
    const s = Number(p.step) || 1
    // 已提交兑换：恢复结果页
    if (redemptionToken.value && s >= 4) {
      step.value = 4
      return true
    }
    // 预检完成：恢复确认页（含订阅摘要）
    if (preflightToken.value && s === 3) {
      step.value = 3
      return true
    }
    if (s >= 1 && s <= 4) {
      step.value = s
      return s > 1
    }
  } catch {
    /* ignore */
  }
  return false
}

function clearProgress() {
  try {
    sessionStorage.removeItem(PROGRESS_KEY)
  } catch {
    /* ignore */
  }
}

watch(
  [step, code, redemptionToken, attemptToken, preflightToken, previewInfo, account, resultStatus, resultStage, resultMessage, resultBody, timeline],
  () => saveProgress(),
  { deep: true },
)

const resultPretty = computed(() => JSON.stringify(resultBody.value, null, 2))

const targetPlan = computed(() =>
  String(previewInfo.value?.plan || previewInfo.value?.plan_type || '').toLowerCase(),
)

const subscriptionStatusText = computed(() => {
  if (account.value.subscriptionHasActive === true) return '有效'
  if (
    account.value.subscriptionHasActive === false
    || String(account.value.currentPlan).toLowerCase() === 'free'
  ) {
    return '已到期 / 免费版'
  }
  return '上游未提供'
})

const subscriptionStatusTag = computed(() => {
  if (account.value.subscriptionHasActive === true) return 'success'
  if (account.value.subscriptionHasActive === false) return 'info'
  return 'info'
})

const renewalStatusText = computed(() => {
  if (account.value.subscriptionWillRenew === true) return '到期后自动续费'
  if (account.value.subscriptionWillRenew === false) return '到期后不续费'
  return '上游未提供'
})

const subscriptionRemaining = computed(() =>
  remainingTime(account.value.subscriptionActiveUntil, nowTick.value),
)

const lastPaymentAtText = computed(() => {
  const lp = account.value.lastPayment
  if (!lp) return '上游未提供'
  const at = lp.paidAt || lp.paid_at || lp.created || lp.created_at
  return at ? fmtTime(at) : '上游未提供'
})

const lastPaymentText = computed(() => {
  const lp = account.value.lastPayment
  if (!lp) return '上游未提供'
  const amount = formatPaymentAmount(lp.amountMinor ?? lp.amount_minor, lp.currency)
  const status = String(lp.status || '').toLowerCase() === 'paid' ? '已支付' : (lp.status || '上游未提供')
  return amount ? `${amount} · ${status}` : status
})

const paymentMethodText = computed(() => {
  const method = account.value.paymentMethod
  if (!method) return '上游未提供'
  const brand = String(method.brand || method.type || '').toUpperCase()
  const last4 = method.last4 || method.last_4
  const label = [brand, last4 ? `**** ${last4}` : ''].filter(Boolean).join(' ')
  const expMonth = method.expMonth || method.exp_month
  const expYear = method.expYear || method.exp_year
  const expiry = expMonth && expYear
    ? `卡片到期 ${String(expMonth).padStart(2, '0')}/${expYear}`
    : ''
  const isDefault = method.isDefault ?? method.is_default
  return [label, expiry, isDefault ? '默认支付方式' : ''].filter(Boolean).join(' · ') || '上游未提供'
})

const alreadySatisfied = computed(() => {
  if (!account.value.checked || !targetPlan.value) return false
  if (account.value.subscriptionHasActive === false) return false
  return planSatisfied(account.value.currentPlan, targetPlan.value)
})

const alreadySatisfiedHint = computed(() => {
  const plan = planLabel(targetPlan.value)
  const until = account.value.canPurchaseAt || account.value.subscriptionActiveUntil
  if (until && account.value.subscriptionWillRenew === true) {
    return `账号已有 ${plan} 或更高套餐，当前周期至 ${fmtTime(until)} 并会自动续费，取消并到期前不能重复购买。`
  }
  if (until && account.value.subscriptionWillRenew === false) {
    return `账号已有 ${plan} 或更高套餐，最早可在 ${fmtTime(until)} 后再次购买。`
  }
  if (until) {
    return `账号已有 ${plan} 或更高套餐，当前周期至 ${fmtTime(until)}，暂不能重复购买。`
  }
  return `账号已有 ${plan} 或更高套餐，本次不能重复购买。`
})

function planLabel(value: string) {
  const n = String(value || 'free').toLowerCase()
  if (n.includes('prolite') || n.includes('5x') || n === 'pro_5x') return 'Pro 5x'
  if (n.includes('20x') || n === 'pro_20x' || n === 'pro' || n === 'chatgptpro' || n.includes('pro')) return 'Pro 20x'
  if (n.includes('plus')) return 'Plus'
  if (n.includes('team')) return 'Team'
  if (!n || n === 'free') return '免费版'
  return value
}

function planSatisfied(currentPlan: string, requestedPlan: string) {
  const current = String(currentPlan || '').toLowerCase()
  const req = String(requestedPlan || '').toLowerCase()
  const currentRank =
    current.includes('prolite') || current.includes('5x') || current === 'pro_5x'
      ? 2
      : current.includes('pro')
        ? 3
        : current.includes('plus')
          ? 1
          : 0
  const requestedRank =
    req === 'pro_20x' || req === 'pro' || req.includes('20x')
      ? 3
      : req === 'pro_5x' || req.includes('5x')
        ? 2
        : req === 'plus' || req.includes('plus')
          ? 1
          : 99
  return currentRank >= requestedRank
}

function remainingTime(value: string, timestamp = Date.now()) {
  const expiresAt = Date.parse(value || '')
  if (!Number.isFinite(expiresAt)) return '—'
  const minutes = Math.max(0, Math.ceil((expiresAt - timestamp) / 60000))
  if (minutes <= 0) return '已到期'
  const days = Math.floor(minutes / 1440)
  const hours = Math.floor((minutes % 1440) / 60)
  const restMinutes = minutes % 60
  if (days > 0) return `${days} 天 ${hours} 小时`
  if (hours > 0) return `${hours} 小时 ${restMinutes} 分钟`
  return `${restMinutes} 分钟`
}

function formatPaymentAmount(value: unknown, currency: unknown) {
  if (value === null || value === undefined || value === '') return ''
  const amount = Number(value)
  const code = String(currency || '').toUpperCase()
  if (!Number.isFinite(amount)) return ''
  let digits = 2
  if (code) {
    try {
      digits =
        new Intl.NumberFormat(undefined, { style: 'currency', currency: code }).resolvedOptions()
          .maximumFractionDigits ?? 2
    } catch {
      /* use two decimals */
    }
  }
  return `${code ? `${code} ` : ''}${(amount / 10 ** digits).toLocaleString(undefined, {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  })}`
}

function applyAccountFromPreflight(data: any) {
  const body = data?.data && typeof data.data === 'object' ? data.data : data || {}
  account.value = {
    checked: true,
    email: String(body.email || body.account_email || ''),
    currentPlan: String(body.currentPlan || body.current_plan || body.plan || 'free'),
    subscriptionHasActive:
      typeof body.subscription_has_active === 'boolean' ? body.subscription_has_active : null,
    subscriptionActiveUntil: String(body.subscription_active_until || ''),
    canPurchaseAt: String(body.can_purchase_at || body.subscription_active_until || ''),
    subscriptionWillRenew:
      typeof body.subscription_will_renew === 'boolean' ? body.subscription_will_renew : null,
    // 欠费只提示不拦截：卡台会先取消订阅再重开，多数可恢复
    subscriptionIsDelinquent:
      typeof body.subscription_is_delinquent === 'boolean' ? body.subscription_is_delinquent : null,
    lastPayment: body.last_payment || null,
    paymentMethod: body.payment_method || null,
  }
}

function clearAccount() {
  account.value = {
    checked: false,
    email: '',
    currentPlan: '',
    subscriptionHasActive: null,
    subscriptionActiveUntil: '',
    canPurchaseAt: '',
    subscriptionWillRenew: null,
    subscriptionIsDelinquent: null,
    lastPayment: null,
    paymentMethod: null,
  }
}

const TERMINAL = new Set(['completed', 'declined', 'failed_precharge', 'cancelled', 'failed', 'query_expired'])

function isTerminal(st: string) {
  return TERMINAL.has(String(st || '').toLowerCase())
}

function statusTagType(st: string) {
  const s = String(st || '').toLowerCase()
  if (s === 'completed') return 'success'
  if (['declined', 'failed_precharge', 'cancelled', 'failed'].includes(s)) return 'danger'
  if (['review', 'pending', 'card_open_review', 'card_recharge_review'].includes(s)) return 'warning'
  return 'info'
}

function categoryTag(c: string) {
  if (c === 'success' || c === 'completed') return 'success'
  if (c === 'failed' || c === 'error') return 'danger'
  if (c === 'warning') return 'warning'
  return 'info'
}

function eventDot(c: string) {
  if (c === 'success' || c === 'completed') return 'var(--good, #16a34a)'
  if (c === 'failed' || c === 'error') return 'var(--err, #dc2626)'
  if (c === 'warning') return 'var(--warn, #d97706)'
  return 'var(--primary, #2563eb)'
}

function stepLabel(stepKey: string) {
  const map: Record<string, string> = {
    queued: '排队受理',
    credential_check: '凭证校验',
    pricing: '计价',
    checkout: '开卡/绑卡',
    payment: '支付扣款',
    subscription: '订阅生效',
    invoice: '账单',
    renewal: '续费处理',
    reconcile: '对账确认',
    completed: '完成',
  }
  return map[stepKey] || stepKey || '处理中'
}

function fmtTime(v: any) {
  if (!v) return ''
  try {
    const d = new Date(v)
    if (Number.isNaN(d.getTime())) return String(v)
    return d.toLocaleString()
  } catch {
    return String(v)
  }
}

/** 粗粒度进度条：受理 → 开卡/资金 → 支付 → 开通 */
const progressSteps = computed(() => {
  const keys = [
    { key: 'accept', label: '受理' },
    { key: 'card', label: '开卡/资金' },
    { key: 'pay', label: '支付' },
    { key: 'done', label: '开通' },
  ]
  const st = String(resultStatus.value || '').toLowerCase()
  const stage = String(resultStage.value || '').toLowerCase()
  let idx = 0
  if (st === 'completed') idx = 3
  else if (['declined', 'failed_precharge', 'cancelled', 'failed'].includes(st)) {
    // 停在失败前最远一步
    if (stage.includes('pay') || stage.includes('checkout') || stage.includes('subscription')) idx = 2
    else if (stage.includes('card') || stage.includes('fund')) idx = 1
    else idx = 0
  } else if (stage.includes('subscription') || stage.includes('paid') || stage.includes('invoice')) idx = 2
  else if (stage.includes('dispatch') || stage.includes('payment') || stage.includes('checkout') || stage.includes('spend')) idx = 2
  else if (stage.includes('card') || stage.includes('fund') || stage.includes('await')) idx = 1
  else if (timeline.value.some((e) => ['payment', 'subscription', 'checkout'].includes(e.step))) idx = 2
  else if (timeline.value.length) idx = 1
  return keys.map((k, i) => ({ ...k, active: i <= idx }))
})

function extractCardLastFour(order: any): string {
  const last = String(order?.card_last_four || '').trim()
  if (/^\d{4}$/.test(last)) return last
  const n = String(order?.card_number || '').replace(/\D/g, '')
  return n.length >= 4 ? n.slice(-4) : ''
}

function applyResultPayload(data: any) {
  resultBody.value = data
  // 卡台公开结构：{ order: {status,stage,message,account_email,card_last_four}, events: [] }
  // 兼容顶层扁平 / data 包裹
  const order = data?.order || data?.data?.order || data?.data || data || {}
  const st =
    order.status ||
    data?.status ||
    data?.data?.status ||
    ''
  const stage = order.stage || data?.stage || data?.data?.stage || ''
  const message =
    order.message ||
    order.user_message ||
    data?.message ||
    data?.user_message ||
    data?.data?.message ||
    ''
  resultStatus.value = st
  resultStage.value = stage
  resultMessage.value = message
  const email = String(order.account_email || order.email || data?.account_email || '').trim()
  if (email) resultEmail.value = email
  const last4 = extractCardLastFour(order)
  if (last4) resultCardLastFour.value = last4

  let events = data?.events || data?.data?.events || order.events || []
  if (!Array.isArray(events)) events = []
  timeline.value = events.slice().sort((a: any, b: any) => {
    const ta = new Date(a.created_at || 0).getTime()
    const tb = new Date(b.created_at || 0).getTime()
    return ta - tb
  })
}

async function api(path: string, init: RequestInit = {}) {
  const headers = new Headers(init.headers || {})
  headers.set('X-Redemption-Device', deviceId)
  if (init.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
  const r = await fetch(path, { ...init, headers, credentials: 'include' })
  const text = await r.text()
  let data: any = null
  try { data = text ? JSON.parse(text) : null } catch { data = { raw: text } }
  return { r, data }
}

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
  if (nowTimer) clearInterval(nowTimer)
})

/** 完整 Session：必须含 sessionToken（JWE）；禁止纯 AT */
function extractSession(raw: string): string {
  const s = raw.trim()
  if (!s) return ''
  // 裸 JWE session-token
  if (!s.startsWith('{') && s.split('.').length >= 5) return s
  // 三段 JWT = 纯 AT → 拒绝
  if (!s.startsWith('{') && s.startsWith('eyJ') && s.split('.').length === 3) return ''
  if (s.startsWith('{')) {
    try {
      const o = JSON.parse(s)
      const st = String(o.sessionToken || o.session_token || o.token?.sessionToken || '').trim()
      if (!st) return ''
      // 回传完整 JSON，便于预检绑定 session
      return s
    } catch {
      return ''
    }
  }
  return s.length > 40 ? s : ''
}

async function tryResumeByCode(cdk: string): Promise<boolean> {
  const { r, data } = await api('/api/v1/public/cdk/result-by-code', {
    method: 'POST',
    body: JSON.stringify({ code: cdk }),
  })
  if (!r.ok) return false
  applyResultPayload(data)
  if (!resultStatus.value) resultStatus.value = data?.status || data?.order?.status || 'pending'
  step.value = 4
  startPoll()
  return true
}

async function doPreview() {
  if (busy.value) return
  error.value = ''
  previewInfo.value = null
  if (!code.value.trim()) {
    error.value = '请输入 CDK'
    return
  }
  busy.value = true
  try {
    const cdk = code.value.trim()
    // MAPLE- codes are issued and verified by this site. They must not be
    // sent to the separate upstream-CDK preview endpoint.
    if (isLocalCode(cdk)) {
      localEntryCode.value = normalizeLocalCode(cdk)
      return
    }
    const { r, data } = await api('/api/v1/public/cdk/preview', {
      method: 'POST',
      body: JSON.stringify({ code: cdk }),
    })
    if (!r.ok) {
      const msg = data?.error || data?.msg || data?.message || 'CDK 无效或不可用'
      // 已兑换：尝试用本站绑定恢复进度，而不是卡在第一步
      if (/已兑换|已使用|used|redeemed|consumed|已消耗/i.test(String(msg))) {
        const ok = await tryResumeByCode(cdk)
        if (ok) {
          error.value = ''
          return
        }
      }
      error.value = msg
      return
    }
    // 兼容多种返回结构
    redemptionToken.value = data.redemption_token || data.data?.redemption_token || data.token || ''
    attemptToken.value = data.attempt_token || ''
    previewInfo.value = data.data || data
    if (!redemptionToken.value || !attemptToken.value) {
      // 有的实现把 token 放在顶层其它字段
      error.value = '未返回 redemption_token，请检查卡台 Base 配置'
      return
    }
    step.value = 2
  } catch (e:any) {
    error.value = e?.message || '验证暂时失败，请刷新页面重试；卡密未被使用。'
  } finally {
    busy.value = false
  }
}

async function doPreflight() {
  error.value = ''
  clearAccount()
  preflightToken.value = ''
  busy.value = true
  try {
    let credential: any
    if (credMode.value === 'session') {
      const session = extractSession(sessionRaw.value)
      if (!session) {
        error.value = '请粘贴完整 Session JSON（必须含 sessionToken），不能只用 Access Token'
        return
      }
      credential = { mode: 'session', session }
    } else {
      if (!email.value || !password.value) {
        error.value = '请填写邮箱与密码'
        return
      }
      credential = { mode: 'mailbox', email: email.value.trim(), password: password.value }
    }
    const { r, data } = await api('/api/v1/public/cdk/preflight', {
      method: 'POST',
      body: JSON.stringify({
        code: code.value.trim(),
        redemption_token: redemptionToken.value,
        attempt_token: attemptToken.value,
        credential,
      }),
    })
    // 卡台 envelope: { code:0, msg, data:{ email, currentPlan, subscription_*, preflight_token } }
    if (!r.ok || (data && typeof data.code === 'number' && data.code !== 0)) {
      error.value = data?.error || data?.msg || data?.message || '预检失败'
      return
    }
    const body = data?.data && typeof data.data === 'object' ? data.data : data || {}
    preflightToken.value = body.preflight_token || data?.preflight_token || ''
    if (!preflightToken.value) {
      error.value = '未返回 preflight_token'
      return
    }
    applyAccountFromPreflight(data)
    step.value = 3
  } finally {
    busy.value = false
  }
}

async function doRedeem() {
  error.value = ''
  busy.value = true
  try {
    const client_request_id = 'web-' + deviceId.slice(0, 8) + '-' + Date.now()
    const { r, data } = await api('/api/v1/public/cdk/redeem', {
      method: 'POST',
      body: JSON.stringify({
        redemption_token: redemptionToken.value,
        attempt_token: attemptToken.value,
        preflight_token: preflightToken.value,
        client_request_id,
      }),
    })
    applyResultPayload(data)
    if (!r.ok && r.status !== 202) {
      error.value = data?.error || data?.msg || data?.message || '兑换被拒绝'
      if (r.headers.get('X-Maple-Submit-State') === 'not-submitted') {
        // The server proved the one-way claim was not consumed. Return to
        // account verification so the customer may retry safely later.
        resultStatus.value = ''
        resultMessage.value = ''
        step.value = 2
        return
      }
    }
    if (!resultStatus.value) {
      resultStatus.value = r.ok || r.status === 202 ? 'queued' : 'error'
    }
    step.value = 4
    startPoll()
  } catch {
    // The request may already have reached the server and consumed the
    // one-way submit latch. Never offer an automatic resubmit after a network
    // break; switch to read-only result polling instead.
    resultStatus.value = 'queued'
    resultMessage.value = '提交结果待确认，正在自动查询；请勿重复提交。'
    error.value = '网络连接中断，系统只会查询本次结果，不会重复提交付款。'
    step.value = 4
    startPoll()
  } finally {
    preflightToken.value = ''
    busy.value = false
  }
}

function startPoll() {
  if (pollTimer) clearInterval(pollTimer)
  if (!redemptionToken.value && !code.value.trim()) {
    polling.value = false
    return
  }
  missingPolls = 0
  polling.value = true
  const tick = async () => {
    try {
      let r: Response
      let data: any
      if (redemptionToken.value && attemptToken.value) {
        ;({ r, data } = await api('/api/v1/public/cdk/result', {
          method: 'POST',
          body: JSON.stringify({
            redemption_token: redemptionToken.value,
            attempt_token: attemptToken.value,
          }),
        }))
        // A stale browser token must not make the customer resubmit. The CDK
        // is the fixed seven-day recovery handle, so immediately fall back to
        // its current immutable attempt when the token is no longer found.
        if ((r.status === 404 || r.status === 409) && code.value.trim()) {
          ;({ r, data } = await api('/api/v1/public/cdk/result-by-code', {
            method: 'POST',
            body: JSON.stringify({ code: code.value.trim() }),
          }))
        }
      } else {
        ;({ r, data } = await api('/api/v1/public/cdk/result-by-code', {
          method: 'POST',
          body: JSON.stringify({ code: code.value.trim() }),
        }))
      }
      if (r.ok) {
        missingPolls = 0
        error.value = ''
        applyResultPayload(data)
        saveProgress()
        if (isTerminal(resultStatus.value)) {
          polling.value = false
          if (pollTimer) clearInterval(pollTimer)
        }
      } else if (r.status === 410 || data?.status === 'query_expired') {
        resultStatus.value = 'query_expired'
        resultMessage.value = data?.error || '7 天查询期已结束；如需售后，请联系客服。'
        redemptionToken.value = ''
        attemptToken.value = ''
        polling.value = false
        if (pollTimer) clearInterval(pollTimer)
        pollTimer = null
        saveProgress()
      } else if (r.status === 404) {
        missingPolls += 1
        if (missingPolls >= 20) {
          resultStatus.value = 'review'
          resultMessage.value = '暂未确认本次提交结果。系统已停止自动刷新，请稍后用同一张卡密查询；请勿重复提交。'
          error.value = resultMessage.value
          polling.value = false
          if (pollTimer) clearInterval(pollTimer)
          pollTimer = null
          saveProgress()
        }
      }
    } catch {
      /* ignore transient network */
    }
  }
  tick()
  pollTimer = setInterval(tick, 3000)
}

onMounted(() => {
  nowTimer = setInterval(() => {
    nowTick.value = Date.now()
  }, 30000)
  const fromMemory = String(sessionStorage.getItem('maple:pending-redeem-cdk') || '').trim()
  if (fromMemory) sessionStorage.removeItem('maple:pending-redeem-cdk')
  const fromLegacyURL = String(route.query.cdk || route.query.code || '').trim()
  const q = fromMemory || fromLegacyURL
  // Keep old links working, but immediately remove a legacy CDK from the
  // visible URL/history so later navigation or analytics cannot retain it.
  if (fromLegacyURL) router.replace({ path: route.path })
  if (loadProgress()) {
    if (q && code.value.trim() !== q) {
      resetAll()
      code.value = q
    } else if (step.value === 4 && (redemptionToken.value || code.value)) {
      startPoll()
    }
  } else if (q) {
    code.value = q
  }
})

function resetAll() {
  if (pollTimer) clearInterval(pollTimer)
  pollTimer = null
  missingPolls = 0
  clearProgress()
  progressCreatedAt.value = Date.now()
  step.value = 1
  error.value = ''
  code.value = ''
  previewInfo.value = null
  redemptionToken.value = ''
  attemptToken.value = ''
  preflightToken.value = ''
  clearAccount()
  resultBody.value = null
  resultStatus.value = ''
  resultStage.value = ''
  resultMessage.value = ''
  resultEmail.value = ''
  resultCardLastFour.value = ''
  timeline.value = []
  polling.value = false
}
</script>

<style scoped>
.redeem-page {
  background:
    radial-gradient(720px 300px at 12% 0%, rgba(185, 54, 38, .12), transparent 62%),
    radial-gradient(620px 260px at 94% 18%, rgba(232, 138, 42, .10), transparent 65%);
}
.maple-hero {
  position: relative;
  overflow: hidden;
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 24px;
  min-height: 188px;
  padding: 30px 32px;
  border: 1px solid rgba(172, 58, 39, .24);
  border-radius: 24px;
  color: #fff9f2;
  background:
    linear-gradient(112deg, rgba(58, 22, 18, .98), rgba(120, 41, 28, .96) 58%, rgba(184, 70, 33, .92)),
    #74281f;
  box-shadow: 0 22px 48px rgba(118, 48, 30, .22);
}
.maple-hero::before,
.maple-hero::after {
  position: absolute;
  right: -12px;
  color: rgba(255, 221, 164, .12);
  font-size: 10rem;
  line-height: 1;
  pointer-events: none;
  content: '🍁';
  transform: rotate(16deg);
}
.maple-hero::before { top: -44px; }
.maple-hero::after { right: 104px; bottom: -92px; font-size: 7rem; opacity: .48; transform: rotate(-18deg); }
.maple-hero-copy,
.maple-hero > .flex { position: relative; z-index: 1; }
.maple-brand {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-size: 15px;
  font-weight: 800;
  letter-spacing: .02em;
}
.maple-mark { font-size: 22px; filter: drop-shadow(0 2px 8px rgba(0, 0, 0, .25)); }
.maple-kicker { margin-top: 22px; font-family: var(--font-mono); font-size: 10px; font-weight: 700; letter-spacing: .18em; color: rgba(255, 236, 201, .76); }
.maple-hero h1 { margin: 2px 0 6px; font-size: clamp(2rem, 5vw, 2.8rem); line-height: 1.1; color: #fffaf5; }
.maple-summary { max-width: 430px; color: rgba(255, 239, 218, .82); font-size: 14px; }
.maple-assurance {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 12px 15px;
  border: 1px solid color-mix(in srgb, #b83f2a 25%, var(--brd));
  border-radius: 14px;
  background: color-mix(in srgb, #b83f2a 7%, var(--surface));
  color: var(--ink-2);
  font-size: 13px;
}
.maple-assurance b { color: var(--ink); }
.maple-assurance-icon {
  display: grid;
  flex: 0 0 auto;
  place-items: center;
  width: 22px;
  height: 22px;
  border-radius: 50%;
  background: #b83f2a;
  color: #fff;
  font-weight: 800;
}
.maple-steps { padding: 10px; }
.maple-step {
  display: inline-flex;
  align-items: baseline;
  gap: 7px;
  padding: 8px 10px;
  border-radius: 10px;
  color: var(--ink-3);
  font-size: 12px;
  transition: background .18s ease, color .18s ease;
}
.maple-step b { font-family: var(--font-mono); font-size: 10px; letter-spacing: .04em; }
.maple-step.active { background: #b83f2a; color: #fff; box-shadow: 0 6px 16px rgba(184, 63, 42, .23); }
.maple-step.done { color: #2f8f6b; }
.redeem-card { border-top: 3px solid #b83f2a; }
.redeem-card-heading { display: flex; align-items: center; gap: 12px; }
.redeem-card-icon {
  display: grid;
  place-items: center;
  width: 42px;
  height: 42px;
  border-radius: 13px;
  background: #b83f2a;
  color: #fff;
  font-size: 24px;
  font-weight: 700;
}
.redeem-card-overline { margin-bottom: 1px; color: #b83f2a; font-family: var(--font-mono); font-size: 10px; font-weight: 700; letter-spacing: .12em; }
.maple-code-input { padding: 14px 16px; border-color: color-mix(in srgb, #b83f2a 35%, var(--brd)); font-size: 15px; letter-spacing: .045em; }
.maple-code-input:focus { border-color: #b83f2a; box-shadow: 0 0 0 4px rgba(184, 63, 42, .12); }
.text-good { color: var(--good, #16a34a); }
.text-warn { color: var(--warn, #d97706); }
.border-primary { border-color: var(--primary) !important; }
.bg-primary\/10 { background: color-mix(in srgb, var(--primary) 12%, transparent); }
.border-brd { border-color: var(--brd); }
.account-facts {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px 16px;
  margin: 0;
}
.account-facts > div {
  min-width: 0;
}
.account-facts dt {
  margin: 0;
  font-size: 12px;
  color: var(--muted, #6b7280);
}
.account-facts dd {
  margin: 2px 0 0;
  font-size: 14px;
  font-weight: 600;
  color: var(--ink, #111827);
  word-break: break-all;
}
@media (max-width: 640px) {
  .maple-hero { min-height: 0; padding: 24px 20px; border-radius: 20px; }
  .maple-hero > .flex { gap: 6px; }
  .maple-hero::after { display: none; }
  .maple-kicker { margin-top: 18px; }
  .maple-step { padding: 7px 8px; }
  .account-facts { grid-template-columns: 1fr; }
}
</style>
