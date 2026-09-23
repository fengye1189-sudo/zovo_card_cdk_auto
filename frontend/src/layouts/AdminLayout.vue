<template>
  <div class="app-shell" :class="[`layout-${layout}`, `nav-${nav}`]">
    <!-- 侧栏 / 细轨（cyber、slate 等） -->
    <aside v-if="layout === 'sidebar' || layout === 'rail'" class="sidenav">
      <div class="side-brand" @click="router.push('/ops')">
        <span class="brand-icon">🍁</span>
        <span v-if="layout !== 'rail'" class="brand-text">{{ brand.name || '运营控制台' }}</span>
      </div>
      <nav class="side-pills">
        <router-link
          v-for="it in navItems"
          :key="it.path"
          :to="it.path"
          class="side-link"
          :class="{ active: isActive(it.path) }"
          :title="it.label"
        >
          <el-icon><component :is="it.icon" /></el-icon>
          <span v-if="layout !== 'rail'" class="side-label">{{ it.label }}</span>
        </router-link>
      </nav>
      <div class="side-foot">
        <span v-if="layout !== 'rail'" class="admin-name">{{ auth.username || 'admin' }}</span>
        <el-button v-if="layout !== 'rail'" size="small" @click="doLogout">退出</el-button>
        <el-button v-else size="small" circle @click="doLogout" title="退出">⎋</el-button>
      </div>
    </aside>

    <div class="main-col">
      <!-- 顶栏：top 布局始终显示；侧栏布局显示精简顶条 -->
      <header v-if="layout === 'top'" class="topnav">
        <div class="nav-inner">
          <a class="mall-return" href="https://maple1189ai.com/">← 返回主商城</a>
          <div class="brand" @click="router.push('/ops')">
            <span class="brand-icon">🍁</span>
            <span class="brand-text">{{ brand.name || '运营控制台' }}</span>
          </div>
          <nav class="nav-pills">
            <router-link
              v-for="it in navItems"
              :key="it.path"
              :to="it.path"
              class="pill"
              :class="{ active: isActive(it.path) }"
            >
              <el-icon><component :is="it.icon" /></el-icon><span>{{ it.label }}</span>
            </router-link>
          </nav>
          <div class="nav-actions">
            <span class="admin-name">{{ auth.username || 'admin' }}</span>
            <el-button size="small" round @click="doLogout">退出</el-button>
          </div>
        </div>
      </header>

      <header v-else class="subtop">
        <div class="subtop-inner">
          <a class="mall-return" href="https://maple1189ai.com/">← 返回主商城</a>
          <div class="subtop-title">{{ currentTitle }}</div>
          <div class="nav-actions">
            <span class="admin-name">{{ auth.username || 'admin' }}</span>
          </div>
        </div>
      </header>

      <main class="page"><router-view /></main>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '../stores/auth'
import { serverLogout } from '../lib/api'
import { siteBrand, currentSkinMeta, siteSkin } from '../theme'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const brand = siteBrand
const skin = siteSkin

const layout = computed(() => currentSkinMeta.value.layout)
const nav = computed(() => currentSkinMeta.value.nav)

const navItems = computed(() => [
  { path: '/ops/records', label: '查询与售后', icon: 'Search', permission: 'records.read' },
  { path: '/ops/customers', label: '客户到期', icon: 'UserFilled', permission: 'records.read' },
  { path: '/ops/cdkeys', label: '卡密管理', icon: 'Key', permission: 'cdks.issue' },
  { path: '/ops/products', label: '商品目录', icon: 'Goods', permission: 'catalog.read' },
  { path: '/ops/orders', label: '卡台订单', icon: 'Document', permission: 'system.manage' },
  { path: '/ops/automation', label: '自动化', icon: 'Setting', permission: 'system.manage' },
  { path: '/ops/finance', label: '财务核对', icon: 'Wallet', permission: 'system.manage' },
  { path: '/ops/health', label: '运行与备份', icon: 'Monitor', permission: 'system.manage' },
  { path: '/ops/messages', label: '通知与回复', icon: 'Bell', permission: 'records.read' },
  { path: '/ops/team', label: '团队权限', icon: 'User', permission: 'team.manage' },
  { path: '/ops/api-access', label: '商城接入', icon: 'Connection', permission: 'system.manage' },
  { path: '/ops/integration', label: '接入设置', icon: 'Link', permission: 'system.manage' },
  { path: '/ops/audit', label: '操作日志', icon: 'Tickets', permission: 'system.manage' },
].filter(item => auth.can(item.permission)))

const currentTitle = computed(() => {
  const hit = navItems.value.find((n) => isActive(n.path))
  return hit?.label || brand.value.name || '控制台'
})

function isActive(p: string) {
  return p === '/ops' ? route.path === '/ops' : route.path.startsWith(p)
}
async function doLogout() {
  await serverLogout()
  router.push('/ops/login')
}
</script>

<style scoped>
.app-shell {
  min-height: 100vh;
  display: flex;
  flex-direction: column;
  background: var(--bg);
  background-image: var(--bg-tint);
  background-attachment: fixed;
  color: var(--ink);
  transition: background-color .3s ease, color .2s ease;
}
.mall-return {
  display: inline-flex; align-items: center; min-height: 44px;
  padding: 8px 14px; border: 1px solid var(--brd);
  border-radius: var(--radius-pill, 999px); background: var(--primary-soft);
  color: var(--primary); font-size: 14px; font-weight: 600;
  text-decoration: none; white-space: nowrap; flex-shrink: 0;
}
.mall-return:focus-visible { outline: 2px solid var(--primary); outline-offset: 3px; }
.layout-sidebar, .layout-rail {
  flex-direction: row;
  align-items: stretch;
}
.main-col {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  min-height: 100vh;
}

/* ── top nav ── */
.topnav {
  position: sticky; top: 0; z-index: 100;
  background: color-mix(in srgb, var(--surface) 88%, transparent);
  backdrop-filter: saturate(1.3) blur(12px);
  border-bottom: 1px solid var(--brd);
}
.nav-inner {
  max-width: var(--content-max, 1500px); margin: 0 auto;
  min-height: var(--nav-height, 60px); padding: 12px 20px;
  display: flex; align-items: center; gap: 14px; flex-wrap: wrap;
}
.brand, .side-brand {
  display: flex; align-items: center; gap: 8px; cursor: pointer; flex-shrink: 0;
}
.brand-icon { font-size: 20px; line-height: 1; }
.brand-text {
  font-size: 17px; font-weight: 700; color: var(--ink); white-space: nowrap;
  font-family: var(--font-display, var(--font-serif));
}
.nav-pills { display: flex; align-items: center; gap: 2px; flex-wrap: wrap; flex: 1; min-width: 0; }
.pill {
  display: inline-flex; align-items: center; gap: 5px;
  padding: 7px 12px; border-radius: var(--radius-pill, 999px); font-size: 14px;
  color: var(--ink-2); text-decoration: none; white-space: nowrap; transition: all .15s;
}
.pill:hover { background: var(--primary-soft); color: var(--ink); }
.pill.active { background: var(--primary-soft); color: var(--primary); font-weight: 600; }
.nav-actions { display: flex; align-items: center; gap: 12px; flex-shrink: 0; }
.hicon { cursor: pointer; color: var(--ink-2); display: flex; font-size: 18px; }
.admin-name { font-size: 13px; color: var(--ink-2); }
.page {
  max-width: var(--content-max, 1500px); width: 100%; margin: 0 auto;
  padding: var(--page-pad, 22px 20px); flex: 1;
}

/* ── sidebar ── */
.sidenav {
  width: 220px;
  flex-shrink: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 16px 12px;
  background: var(--surface);
  border-right: 1px solid var(--brd);
  position: sticky;
  top: 0;
  height: 100vh;
  z-index: 50;
}
.layout-rail .sidenav {
  width: 64px;
  align-items: center;
  padding: 12px 8px;
}
.side-brand {
  padding: 8px 10px 14px;
  border-bottom: 1px solid var(--brd);
  margin-bottom: 8px;
  width: 100%;
}
.layout-rail .side-brand { justify-content: center; padding: 8px 0 12px; }
.side-pills {
  display: flex; flex-direction: column; gap: 4px; flex: 1; width: 100%;
  overflow: auto;
}
.side-link {
  display: flex; align-items: center; gap: 10px;
  padding: 10px 12px;
  border-radius: var(--radius-md);
  color: var(--ink-2);
  text-decoration: none;
  font-size: 14px;
  border: 1px solid transparent;
  transition: .15s ease;
}
.layout-rail .side-link { justify-content: center; padding: 12px; }
.side-link:hover { background: var(--primary-soft); color: var(--ink); }
.side-link.active {
  background: var(--primary-soft);
  color: var(--primary);
  border-color: var(--brd-2);
  font-weight: 600;
  box-shadow: var(--shadow-sm);
}
.side-label { white-space: nowrap; }
.side-foot {
  display: flex; flex-direction: column; gap: 8px;
  padding-top: 12px; border-top: 1px solid var(--brd);
  width: 100%;
}
.layout-rail .side-foot { align-items: center; }
.side-tool {
  display: flex; align-items: center; justify-content: center;
  width: 100%; height: 36px; border-radius: var(--radius-md);
  border: 1px solid var(--brd); background: var(--surface-2);
  color: var(--ink-2); cursor: pointer;
}
.layout-rail .side-tool { width: 40px; }

.subtop {
  border-bottom: 1px solid var(--brd);
  background: color-mix(in srgb, var(--surface) 90%, transparent);
  backdrop-filter: blur(8px);
  position: sticky; top: 0; z-index: 40;
}
.subtop-inner {
  min-height: 56px; padding: 4px 20px; flex-wrap: wrap; gap: 8px;
  display: flex; align-items: center; justify-content: space-between;
}
.subtop-title {
  font-family: var(--font-display, var(--font-serif));
  font-weight: 700; font-size: 15px; color: var(--ink);
}

/* cyber 侧栏霓虹 */
:global(html[data-skin='cyber']) .sidenav {
  background: linear-gradient(180deg, #0d1524 0%, #0a101c 100%);
  border-right-color: rgba(34, 211, 238, 0.22);
  box-shadow: inset -1px 0 0 rgba(34, 211, 238, 0.08);
}
:global(html[data-skin='cyber']) .side-link.active {
  box-shadow: 0 0 0 1px rgba(34, 211, 238, 0.35), 0 0 20px rgba(34, 211, 238, 0.08);
}

@media (max-width: 900px) {
  .nav-inner { gap: 8px; padding: 10px 12px; }
  .nav-pills { order: 3; flex-basis: 100%; flex-wrap: nowrap; overflow-x: auto; padding-bottom: 6px; }
  .nav-actions { margin-left: auto; }
  .nav-actions .admin-name { max-width: 125px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .page { padding: 16px 12px; }
  .layout-sidebar .sidenav,
  .layout-rail .sidenav {
    width: 56px;
    padding: 10px 6px;
  }
  .side-label, .layout-sidebar .brand-text, .layout-sidebar .admin-name,
  .layout-sidebar .side-foot .el-button:not(.is-circle) {
    display: none !important;
  }
  .side-brand, .side-link, .side-foot { align-items: center; justify-content: center; }
}
</style>
