<template>
  <div class="relative min-h-screen flex items-center justify-center px-6 py-10">
    <div class="absolute right-5 top-5 flex items-center gap-3">
      <LanguageToggle />
      <ThemeToggle />
    </div>
    <div class="w-full max-w-md animate-slideInUp">
      <div class="card space-y-6 !p-7">
        <div class="text-center">
          <router-link to="/" class="text-sm app-link">{{ t('common.backHome') }}</router-link>
          <h1 class="mt-4 text-3xl font-bold text-ink">{{ t('login.title') }}</h1>
          <p class="mt-2 text-sm text-muted">{{ t('login.subtitle') }}</p>
        </div>

        <a class="btn-primary block text-center" href="https://maple1189ai.com/api/integrations/cdk/sso?next=%2Fops%2Fautomation">🍁 商城统一邮箱登录</a>
        <p class="text-sm text-muted">已在商城完成邮箱验证时，无需再次输入验证码。</p>
        <div class="form-group">
          <label>{{ t('login.username') }}</label>
          <input v-model="form.username" class="input" :placeholder="t('login.usernamePlaceholder')" autocomplete="username" @keyup.enter="submit" />
        </div>

        <div class="form-group">
          <label>{{ t('login.password') }}</label>
          <input v-model="form.password" class="input" type="password" :placeholder="t('login.passwordPlaceholder')" autocomplete="current-password" @keyup.enter="submit" />
        </div>

        <div v-if="errorMessage" class="alert alert-error">
          {{ errorMessage }}
        </div>

        <button class="btn-primary w-full disabled:opacity-50" :disabled="loading" @click="submit">
          <span v-if="!loading">{{ t('login.loginBtn') }}</span>
          <span v-else class="flex items-center justify-center gap-2"><span class="spinner"></span>{{ t('login.loggingIn') }}</span>
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '../../stores/auth'
import ThemeToggle from '../../components/ThemeToggle.vue'
import LanguageToggle from '../../components/LanguageToggle.vue'

const { t } = useI18n({ useScope: 'global' })
const router = useRouter()
const route = useRoute()
const authStore = useAuthStore()

const form = reactive({
  username: sessionStorage.getItem('post_setup_user') || '',
  password: sessionStorage.getItem('post_setup_pass') || '',
})

const loading = ref(false)
const errorMessage = ref('')

const completeMarketplaceSso = async () => {
  if (route.query.sso !== '1') return

  loading.value = true
  errorMessage.value = ''
  try {
    // The signed assertion has already been exchanged server-side for an
    // HttpOnly cookie. This call only reads the authenticated identity so the
    // UI can restore its non-sensitive display metadata.
    const response = await fetch('/api/v1/auth/admin/me', { credentials: 'include' })
    const data = await response.json()
    if (!response.ok || !data?.is_admin || !data?.username) {
      throw new Error('invalid marketplace SSO session')
    }

    authStore.save({
      username: data.username,
      name: data.name || data.username,
      role: data.role,
      permissions: data.permissions,
      loginSource: data.login_source,
      // The server-side session cookie is authoritative; this only prevents
      // the client router from sending a valid SSO user back to the login UI.
      expiresAt: data.expires_at || new Date(Date.now() + 23 * 60 * 60 * 1000).toISOString(),
    })
    const redirect = typeof route.query.redirect === 'string' && route.query.redirect.startsWith('/ops')
      ? route.query.redirect
      : '/ops/automation'
    await router.replace(redirect)
  } catch {
    errorMessage.value = '商城单点登录已失效，请回到商城后台重新打开。'
  } finally {
    loading.value = false
  }
}

// 安装向导写过临时凭据时预填一次后清掉，避免长期留在 sessionStorage
if (form.username || form.password) {
  sessionStorage.removeItem('post_setup_user')
  sessionStorage.removeItem('post_setup_pass')
}

onMounted(() => { void completeMarketplaceSso() })

const submit = async () => {
  loading.value = true
  errorMessage.value = ''

  try {
    const payload = {
      username: form.username.trim(),
      password: form.password.trim(),
    }
    const response = await fetch('/api/v1/auth/admin/login', {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    })
    const data = await response.json()

    if (!response.ok) {
      errorMessage.value = data.error || t('login.errLoginFailed')
      return
    }

    // 认证靠 HttpOnly cookie；本地只存展示用元数据
    authStore.save({
      username: data.username,
      name: data.name,
      expiresAt: data.expires_at,
      token: data.token,
      role: data.role,
      permissions: data.permissions,
      loginSource: data.login_source,
    })

    const redirect = typeof route.query.redirect === 'string' ? route.query.redirect : '/ops'
    router.push(redirect)
  } catch (error) {
    errorMessage.value = t('login.errNetwork')
  }

  loading.value = false
}
</script>
