<template>
  <div class="space-y-6 pb-8">
    <div class="flex flex-wrap items-end justify-between gap-4">
      <div><h1 class="text-3xl font-bold text-ink">团队与权限</h1><p class="mt-2 text-muted">为每位同事设置独立账号，操作记录可追溯到人。</p></div>
      <button class="btn-secondary" :disabled="loading || saving" @click="load">刷新成员</button>
    </div>
    <div v-if="error" class="alert alert-error" role="alert">{{ error }}</div>
    <div v-if="message" class="alert alert-success" role="status">{{ message }}</div>
    <div class="grid gap-3 md:grid-cols-3">
      <div v-for="item in roles" :key="item.value" class="card !p-4"><h2 class="font-bold text-ink">{{ item.label }}</h2><p class="mt-2 text-sm text-muted">{{ item.description }}</p></div>
    </div>
    <form class="card space-y-5" @submit.prevent="save">
      <div class="flex flex-wrap items-center justify-between gap-3"><h2 class="text-xl font-bold text-ink">{{ editingId ? '编辑成员' : '新增成员' }}</h2><button v-if="editingId" type="button" class="app-link" :disabled="saving" @click="reset">取消编辑</button></div>
      <div class="grid gap-4 md:grid-cols-2">
        <label class="block"><span class="mb-2 block">登录用户名</span><input v-model="form.username" class="input" autocomplete="off" :disabled="!!editingId || saving" required minlength="3" maxlength="32" placeholder="字母、数字或下划线" /></label>
        <label class="block"><span class="mb-2 block">显示名称</span><input v-model="form.name" class="input" :disabled="saving" maxlength="120" placeholder="同事姓名或称呼" /></label>
        <label class="block"><span class="mb-2 block">角色</span><select v-model="form.role" class="input" :disabled="saving"><option v-for="item in roles" :key="item.value" :value="item.value">{{ item.label }}</option></select></label>
        <label v-if="editingSource !== 'marketplace'" class="block"><span class="mb-2 block">{{ editingId ? '新密码（留空不修改）' : '初始密码' }}</span><input v-model="form.password" type="password" class="input" autocomplete="new-password" :disabled="saving" :required="!editingId" minlength="12" maxlength="72" placeholder="至少 12 位，请使用独立密码" /></label>
        <p v-else class="self-center text-sm text-muted">此成员从商城进入，登录方式由商城管理。</p>
      </div>
      <label class="flex items-center gap-2"><input v-model="form.is_active" type="checkbox" :disabled="saving" />允许此成员登录</label>
      <p v-if="editingId" class="text-sm text-muted">变更角色、登录状态或密码后，该成员需要重新登录。系统会保留至少一位启用的老板。</p>
      <button type="submit" class="btn-primary" :disabled="saving || loading">{{ saving ? '保存中…' : editingId ? '保存成员设置' : '创建成员' }}</button>
    </form>
    <div class="card overflow-hidden !p-0"><div class="overflow-x-auto"><table class="data-table w-full">
      <thead><tr><th>成员</th><th>角色</th><th>登录方式</th><th>状态</th><th>操作</th></tr></thead>
      <tbody>
        <tr v-if="loading"><td colspan="5" class="py-8 text-center text-muted">正在读取团队…</td></tr>
        <tr v-else-if="!members.length"><td colspan="5" class="py-8 text-center text-muted">暂无成员</td></tr>
        <tr v-for="member in members" :key="member.id">
          <td><div class="font-medium text-ink">{{ member.name }} <span v-if="member.username === auth.username" class="text-xs text-muted">（当前账号）</span></div><div class="break-all text-sm text-muted">{{ member.username }}</div></td>
          <td>{{ roleLabel(member.role) }}</td><td>{{ member.login_source === 'marketplace' ? '商城登录' : '密码登录' }}</td><td>{{ member.is_active ? '已启用' : '已停用' }}</td>
          <td><div class="flex gap-3 whitespace-nowrap"><button class="app-link" :disabled="saving" @click="edit(member)">编辑</button><button v-if="member.is_active" class="app-link" :disabled="saving || lastOwner(member)" @click="deactivate(member)">{{ lastOwner(member) ? '保留老板' : '停用' }}</button></div></td>
        </tr>
      </tbody>
    </table></div></div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { authFetch } from '../../lib/api'
import { dialog } from '../../lib/dialog'
import { useAuthStore } from '../../stores/auth'

interface Member { id: number; username: string; name: string; role: string; is_active: boolean; login_source: string }
const auth = useAuthStore()
const members = ref<Member[]>([])
const roles = ref([
  { value: 'owner', label: '老板', description: '管理资金、规则、系统和团队。' },
  { value: 'operator', label: '运营', description: '查看记录、生成卡密、处理售后；不能修改资金和接入设置。' },
  { value: 'viewer', label: '只读', description: '查看运营数据和记录，不得修改。' },
])
const editingId = ref<number | null>(null)
const editingSource = ref('password')
const form = reactive({ username: '', name: '', password: '', role: 'viewer', is_active: true })
const loading = ref(false), saving = ref(false), error = ref(''), message = ref('')
const endpoint = '/api/v1/admin/operations/team'
const roleLabel = (role: string) => roles.value.find(item => item.value === role)?.label || role
const lastOwner = (member: Member) => member.role === 'owner' && member.is_active && members.value.filter(item => item.role === 'owner' && item.is_active).length <= 1

async function load() {
  loading.value = true; error.value = ''
  try {
    const response = await authFetch(endpoint)
    const data = await response.json()
    if (!response.ok) throw new Error(data.error || '读取失败，请重试')
    members.value = data.members || []
    if (Array.isArray(data.roles)) roles.value = data.roles
  } catch (e) { error.value = e instanceof Error ? e.message : '暂时无法读取团队' }
  finally { loading.value = false }
}
function reset() { editingId.value = null; editingSource.value = 'password'; Object.assign(form, { username: '', name: '', password: '', role: 'viewer', is_active: true }) }
function edit(member: Member) { editingId.value = member.id; editingSource.value = member.login_source; Object.assign(form, { username: member.username, name: member.name, password: '', role: member.role, is_active: member.is_active }); error.value = ''; message.value = ''; window.scrollTo({ top: 0, behavior: 'smooth' }) }
async function save() {
  if (saving.value) return
  saving.value = true; error.value = ''; message.value = ''
  const ownAccount = form.username === auth.username
  try {
    const body = { ...form }
    const response = await authFetch(editingId.value ? `${endpoint}/${editingId.value}` : endpoint, { method: editingId.value ? 'PATCH' : 'POST', body: JSON.stringify(body) })
    const data = await response.json()
    if (!response.ok) throw new Error(data.error || '保存失败，请重试')
    message.value = data.message || '成员已保存'
    reset()
    if (ownAccount) { await auth.refreshIdentity(); if (!auth.isLoggedIn) { window.location.assign('/ops/login'); return } }
    await load()
  } catch (e) { error.value = e instanceof Error ? e.message : '保存失败，请重试' }
  finally { saving.value = false; form.password = '' }
}
async function deactivate(member: Member) {
  if (saving.value || lastOwner(member)) return
  if (!await dialog.confirm(`停用“${member.name}”后，其现有登录会立即失效。历史操作记录保留，可在此重新启用。`, { title: '停用成员', okText: '停用', danger: true })) return
  saving.value = true; error.value = ''; message.value = ''
  try {
    const response = await authFetch(`${endpoint}/${member.id}`, { method: 'DELETE' })
    const data = await response.json()
    if (!response.ok) throw new Error(data.error || '停用失败')
    message.value = '成员已停用'
    if (member.username === auth.username) { auth.logout(); window.location.assign('/ops/login'); return }
    await load()
  } catch (e) { error.value = e instanceof Error ? e.message : '停用失败' }
  finally { saving.value = false }
}
onMounted(load)
</script>
