<template>
  <div class="min-h-screen py-12">
    <div class="max-w-3xl mx-auto px-6 space-y-6">
      <div class="flex items-start justify-between gap-4">
        <div>
          <router-link to="/" class="app-link mb-4 inline-block text-sm">{{ t('billingCheck.back') }}</router-link>
          <h1 class="text-3xl font-bold text-ink">{{ t('billingCheck.title') }}</h1>
          <p class="text-sm text-muted mt-1">
            {{ t('billingCheck.subtitle') }}
          </p>
        </div>
        <div class="flex gap-2">
          <LanguageToggle />
          <ThemeToggle />
        </div>
      </div>

      <div class="card space-y-4">
        <div class="flex gap-2">
          <button
            type="button"
            class="btn-secondary !py-1.5"
            :class="{ 'ring-2 ring-offset-1': mode === 'cdk' }"
            style="--tw-ring-color: var(--primary)"
            @click="mode = 'cdk'"
          >{{ t('billingCheck.modeCdk') }}</button>
          <button
            type="button"
            class="btn-secondary !py-1.5"
            :class="{ 'ring-2 ring-offset-1': mode === 'session' }"
            style="--tw-ring-color: var(--primary)"
            @click="mode = 'session'"
          >{{ t('billingCheck.modeSession') }}</button>
        </div>

        <template v-if="mode === 'cdk'">
          <div class="rounded-xl bg-soft p-4 text-sm text-muted">
            {{ t('billingCheck.cdkHint') }}
          </div>
          <div class="form-group">
            <label>{{ t('billingCheck.cdkLabel') }}</label>
            <input
              v-model="cdkCode"
              class="input mono"
              :placeholder="t('billingCheck.cdkPlaceholder')"
              autocomplete="off"
              spellcheck="false"
              @keyup.enter="query"
            />
          </div>
        </template>

        <template v-else>
          <div class="rounded-xl bg-soft p-4 text-sm text-muted">
            {{ t('billingCheck.sessionHintBefore') }}
            <a class="app-link underline" href="https://chatgpt.com/api/auth/session" target="_blank" rel="noopener">
              chatgpt.com/api/auth/session
            </a>
            {{ t('billingCheck.sessionHintAfter') }}
          </div>
          <div class="form-group">
            <label>{{ t('billingCheck.sessionLabel') }}</label>
            <textarea
              v-model="tokenInput"
              class="input h-36 font-mono text-xs"
              :placeholder="t('billingCheck.sessionPlaceholder')"
              autocomplete="off"
              spellcheck="false"
            />
          </div>
        </template>

        <div v-if="error" class="alert alert-error">{{ error }}</div>
        <button
          class="btn-primary w-full"
          :disabled="loading || !canSubmit"
          @click="query"
        >
          {{ loading ? t('billingCheck.querying') : t('billingCheck.queryBtn') }}
        </button>
      </div>

      <div v-if="summary" class="card space-y-3">
        <div class="flex items-center justify-between">
          <h2 class="text-xl font-semibold text-ink">{{ t('billingCheck.summaryTitle') }}</h2>
          <el-tag v-if="resultMode" size="small" type="info">{{ sourceLabel }}</el-tag>
        </div>
        <div class="grid sm:grid-cols-2 gap-2 text-sm">
          <div class="flex justify-between gap-2 border-b bd py-2">
            <span class="text-muted">{{ t('billingCheck.plan') }}</span>
            <b class="text-ink">{{ planLabel }}</b>
          </div>
          <div class="flex justify-between gap-2 border-b bd py-2">
            <span class="text-muted">{{ t('billingCheck.activeSubscription') }}</span>
            <b>{{ summary.has_active_subscription == null ? '—' : (summary.has_active_subscription ? t('billingCheck.yes') : t('billingCheck.no')) }}</b>
          </div>
          <div class="flex justify-between gap-2 border-b bd py-2">
            <span class="text-muted">{{ t('billingCheck.autoRenew') }}</span>
            <b>{{ summary.will_renew == null ? '—' : (summary.will_renew ? t('billingCheck.enabled') : t('billingCheck.disabled')) }}</b>
          </div>
          <div class="flex justify-between gap-2 border-b bd py-2">
            <span class="text-muted">{{ t('billingCheck.billing') }}</span>
            <b class="mono">{{ summary.billing_currency || '—' }} {{ summary.billing_period || '' }}</b>
          </div>
          <div class="flex justify-between gap-2 border-b bd py-2 sm:col-span-2">
            <span class="text-muted">{{ t('billingCheck.expiryRenewal') }}</span>
            <b class="mono text-sm">{{ summary.active_until || summary.expires_at || summary.renews_at || '—' }}</b>
          </div>
          <div v-if="summary.email" class="flex justify-between gap-2 border-b bd py-2 sm:col-span-2">
            <span class="text-muted">{{ t('billingCheck.accountEmail') }}</span>
            <b class="mono text-sm">{{ summary.email }}</b>
          </div>
          <div v-if="summary.query_expires_at" class="flex justify-between gap-2 border-b bd py-2 sm:col-span-2">
            <span class="text-muted">{{ t('billingCheck.queryValidUntil') }}</span>
            <b class="mono text-sm">{{ new Date(summary.query_expires_at).toLocaleString() }}</b>
          </div>
        </div>
      </div>

      <div v-if="invoices" class="card space-y-3">
        <h2 class="text-xl font-semibold text-ink">{{ t('billingCheck.invoicesTitle', { n: invoices.length }) }}</h2>
        <p v-if="!invoices.length" class="text-sm text-muted">{{ t('billingCheck.noInvoices') }}</p>
        <div v-for="inv in invoices" :key="inv.id || inv.number" class="rounded-xl bg-soft p-4 text-sm space-y-2">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <b class="mono">{{ formatAmount(inv.total ?? inv.amount_paid, inv.currency) }}</b>
            <span class="text-muted">{{ inv.paid ? t('billingCheck.paid') : (inv.status || t('billingCheck.unpaid')) }} · {{ formatTs(inv.created) }}</span>
          </div>
          <div v-if="inv.description" class="text-subtle text-xs">{{ inv.description }}</div>
          <div class="flex gap-3">
            <a v-if="inv.hosted_invoice_url" class="app-link" :href="inv.hosted_invoice_url" target="_blank" rel="noopener">{{ t('billingCheck.invoiceLink') }}</a>
            <a v-if="inv.invoice_pdf" class="app-link" :href="inv.invoice_pdf" target="_blank" rel="noopener">{{ t('billingCheck.pdf') }}</a>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import LanguageToggle from '../../components/LanguageToggle.vue'
import ThemeToggle from '../../components/ThemeToggle.vue'

const route = useRoute()
const router = useRouter()
const { t } = useI18n({ useScope: 'global' })
const mode = ref<'cdk' | 'session'>('cdk')
const cdkCode = ref('')
const tokenInput = ref('')
const loading = ref(false)
const error = ref('')
const summary = ref<any>(null)
const invoices = ref<any[] | null>(null)
const authSource = ref('')
const billingProvider = ref('')
const resultMode = ref<'cdk' | 'session' | ''>('')

const canSubmit = computed(() =>
  mode.value === 'cdk' ? !!cdkCode.value.trim() : !!tokenInput.value.trim(),
)

const planLabel = computed(() => {
  const s = summary.value || {}
  const raw = (s.plan_type || s.subscription_plan || '').toString()
  if (!raw || raw === 'free') return t('billingCheck.planUnknown')
  return raw.replace('chatgpt', 'ChatGPT ').replace(/_/g, ' ')
})

const sourceLabel = computed(() => {
  const auth = authSource.value.toLowerCase()
  const provider = billingProvider.value.toLowerCase()
  const fromCDK = resultMode.value === 'cdk' || auth === 'cdk' || auth === 'accounthub' || provider === 'accounthub'
  return fromCDK ? t('billingCheck.sourceCdk') : t('billingCheck.sourceSession')
})

function formatAmount(total: any, currency: any) {
  const n = Number(total)
  if (!Number.isFinite(n)) return '—'
  const cur = (currency || 'usd').toString().toUpperCase()
  return `${(n / 100).toFixed(2)} ${cur}`
}
function formatTs(v: any) {
  const n = Number(v)
  try {
    const date = Number.isFinite(n) && n > 0
      ? new Date(n > 10_000_000_000 ? n : n * 1000)
      : new Date(String(v || ''))
    if (Number.isNaN(date.getTime())) return '—'
    return date.toLocaleString()
  } catch {
    return '—'
  }
}

onMounted(() => {
  const remembered = String(sessionStorage.getItem('maple:pending-billing-cdk') || '').trim()
  if (remembered) sessionStorage.removeItem('maple:pending-billing-cdk')
  const legacy = String(route.query.cdk || '').trim()
  const qcdk = remembered || legacy
  if (legacy) router.replace({ path: route.path })
  if (qcdk) {
    cdkCode.value = qcdk
    mode.value = 'cdk'
    query()
  }
})

async function query() {
  error.value = ''
  summary.value = null
  invoices.value = null
  authSource.value = ''
  billingProvider.value = ''
  resultMode.value = ''
  loading.value = true
  const requestedMode = mode.value
  try {
    const body =
      mode.value === 'cdk'
        ? { cdk_code: cdkCode.value.trim() }
        : { token_input: tokenInput.value }
    const r = await fetch('/api/v1/public/billing/check', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    const d = await r.json().catch(() => ({}))
    if (!r.ok) {
      error.value = r.status === 410
        ? t('billingCheck.errWindowExpired')
        : (d.error || d.message || t('billingCheck.errQuery'))
      return
    }
    summary.value = d.summary || {}
    invoices.value = d.invoices || []
    authSource.value = d.auth_source || ''
    billingProvider.value = d.billing_provider || ''
    resultMode.value = requestedMode
  } catch (e: any) {
    error.value = e?.message || t('billingCheck.errNetwork')
  } finally {
    loading.value = false
  }
}
</script>
