import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

const STORAGE_KEY = 'cdk-admin-auth'

interface StoredAuth {
  token?: string
  username: string
  name: string
  expiresAt: string
  role?: string
  permissions?: string[]
  loginSource?: string
}

export const useAuthStore = defineStore('auth', () => {
  const token = ref('')
  const username = ref('')
  const name = ref('')
  const expiresAt = ref('')
  const role = ref('viewer')
  const permissions = ref<string[]>([])
  const loginSource = ref('')
  const identityVerified = ref(false)
  let refreshing: Promise<boolean> | null = null
  const can = (permission: string) => permissions.value.includes(permission)

  // 认证状态由 HttpOnly cookie 维持；前端只用 username + 到期时间判断登录态（不依赖 token）
  const isLoggedIn = computed(() => {
    if (!username.value || !expiresAt.value) return false
    return new Date(expiresAt.value).getTime() > Date.now()
  })

  const restore = () => {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return

    try {
      const saved = JSON.parse(raw) as StoredAuth
      token.value = saved.token || ''
      username.value = saved.username
      name.value = saved.name
      expiresAt.value = saved.expiresAt
      // The server, not local storage, determines the active role on each visit.
      role.value = 'viewer'
      permissions.value = []
      identityVerified.value = false
      if (!isLoggedIn.value) logout()
    } catch (error) {
      logout()
    }
  }

  const save = (payload: StoredAuth) => {
    token.value = payload.token || ''
    username.value = payload.username
    name.value = payload.name
    expiresAt.value = payload.expiresAt
    role.value = ['owner', 'operator', 'viewer'].includes(payload.role || '') ? payload.role! : 'viewer'
    permissions.value = Array.isArray(payload.permissions) ? payload.permissions : []
    loginSource.value = payload.loginSource || ''
    identityVerified.value = !!payload.role
    localStorage.setItem(STORAGE_KEY, JSON.stringify(payload))
  }

  const refreshIdentity = (): Promise<boolean> => {
    if (refreshing) return refreshing
    refreshing = (async () => {
      const controller = new AbortController()
      const timeout = setTimeout(() => controller.abort(), 15000)
      try {
        const headers = new Headers()
        if (token.value) headers.set('Authorization', `Bearer ${token.value}`)
        const response = await fetch('/api/v1/auth/admin/me', { credentials: 'include', headers, cache: 'no-store', signal: controller.signal })
        if (response.status === 401) { logout(); return false }
        if (!response.ok) return false
        const data = await response.json()
        if (!data?.is_admin || !data?.username || !data?.role) { logout(); return false }
        save({ token: token.value, username: data.username, name: data.name || data.username,
          expiresAt: data.expires_at || expiresAt.value, role: data.role,
          permissions: data.permissions, loginSource: data.login_source })
        return true
      } catch { return false }
      finally { clearTimeout(timeout); refreshing = null }
    })()
    return refreshing
  }

  const logout = () => {
    token.value = ''
    username.value = ''
    name.value = ''
    expiresAt.value = ''
    role.value = 'viewer'
    permissions.value = []
    loginSource.value = ''
    identityVerified.value = false
    localStorage.removeItem(STORAGE_KEY)
  }

  return {
    token,
    username,
    name,
    expiresAt,
    role,
    permissions,
    loginSource,
    identityVerified,
    can,
    refreshIdentity,
    isLoggedIn,
    restore,
    save,
    logout,
  }
})
