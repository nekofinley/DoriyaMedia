<script setup>
import { ref, computed, onMounted, onBeforeUnmount, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from './AppIcon.vue'
import SearchOverlay from './SearchOverlay.vue'
import AccountDialog from './AccountDialog.vue'
import { api } from '../api.js'
import { logout as signOut } from '../auth.js'

const props = defineProps({
  me: { type: Object, default: null },
  overlay: { type: Boolean, default: false }, // page has a full-bleed hero under the bar
})

const route = useRoute()
const router = useRouter()
const libraries = ref([])
const scrolled = ref(false)
const menuOpen = ref(false)
const browseOpen = ref(false)
const searching = ref(false)
const acctOpen = ref(false)

const isAdmin = computed(() => props.me?.role === 'admin')
const movieLib = computed(() => libraries.value.find((l) => l.type === 'movie'))
const seriesLib = computed(() => libraries.value.find((l) => l.type === 'series'))
const solid = computed(() => !props.overlay || scrolled.value || menuOpen.value || browseOpen.value)

const nav = computed(() => {
  const out = [{ label: '首页', to: '/', icon: 'home' }]
  if (movieLib.value) out.push({ label: '电影', to: `/library/${movieLib.value.id}?type=movie`, icon: 'film' })
  if (seriesLib.value) out.push({ label: '剧集', to: `/library/${seriesLib.value.id}?type=show`, icon: 'tv' })
  out.push({ label: '追番', to: '/bangumi', icon: 'bookmark' })
  out.push({ label: '文件', to: '/browse', icon: 'folder' })
  return out
})

const tabs = computed(() => {
  const out = [{ label: '首页', to: '/', icon: 'home', exact: true }]
  if (movieLib.value) out.push({ label: '电影', to: `/library/${movieLib.value.id}?type=movie`, icon: 'film' })
  if (seriesLib.value) out.push({ label: '剧集', to: `/library/${seriesLib.value.id}?type=show`, icon: 'tv' })
  out.push({ label: '文件', to: '/browse', icon: 'folder' })
  // The settings backend is the admin's; a viewer gets no tab that would only
  // bounce them back to the home page.
  if (isAdmin.value) out.push({ label: '设置', to: '/settings', icon: 'settings' })
  return out
})

function onScroll() { scrolled.value = window.scrollY > 24 }

function onDocClick(e) {
  if (!e.target.closest?.('.menu-wrap')) { menuOpen.value = false; browseOpen.value = false }
}
function onKey(e) {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); searching.value = true }
  if (e.key === 'Escape') { menuOpen.value = false; browseOpen.value = false }
}

onMounted(async () => {
  window.addEventListener('scroll', onScroll, { passive: true })
  document.addEventListener('click', onDocClick)
  window.addEventListener('keydown', onKey)
  onScroll()
  try { libraries.value = (await api.get('/api/libraries')) || [] } catch {}
})
onBeforeUnmount(() => {
  window.removeEventListener('scroll', onScroll)
  document.removeEventListener('click', onDocClick)
  window.removeEventListener('keydown', onKey)
})

watch(() => route.fullPath, () => { menuOpen.value = false; browseOpen.value = false })

async function logout() {
  await signOut()
}

function pickLibrary(lib) {
  browseOpen.value = false
  router.push(`/library/${lib.id}${lib.type === 'series' ? '?type=show' : '?type=movie'}`)
}

defineExpose({ openSearch: () => { searching.value = true } })
</script>

<template>
  <header class="topbar" :class="{ solid, overlay }">
    <div class="inner">
      <router-link to="/" class="brand" aria-label="DoriyaMedia 首页">
        <span class="logo"><AppIcon name="play" :size="13" filled /></span>
        <span class="wordmark">Doriya<span>Media</span></span>
      </router-link>

      <nav class="nav desktop-only">
        <router-link
          v-for="n in nav" :key="n.to" :to="n.to"
          class="nav-link" exact-active-class="nav-active"
        >{{ n.label }}</router-link>

        <div class="menu-wrap">
          <button class="nav-link browse" :class="{ on: browseOpen }" @click="browseOpen = !browseOpen">
            媒体库
            <AppIcon name="chevronDown" :size="14" :class="{ flip: browseOpen }" />
          </button>
          <Transition name="fade">
            <div v-if="browseOpen" class="dropdown">
              <div class="dd-title">全部媒体库</div>
              <button v-for="l in libraries" :key="l.id" class="dd-item" @click="pickLibrary(l)">
                <AppIcon :name="l.type === 'series' ? 'tv' : 'film'" :size="16" />
                <span class="dd-name">{{ l.name }}</span>
                <span class="tiny">{{ l.type === 'series' ? '剧集' : '电影' }}</span>
              </button>
              <div v-if="!libraries.length" class="dd-empty tiny">还没有媒体库,先到设置里创建</div>
              <router-link v-if="isAdmin" to="/settings" class="dd-item accent" @click="browseOpen = false">
                <AppIcon name="plus" :size="16" /><span class="dd-name">管理媒体库</span>
              </router-link>
            </div>
          </Transition>
        </div>
      </nav>

      <div class="spacer"></div>

      <button class="icon-btn round" title="搜索 (Ctrl/⌘ + K)" aria-label="搜索" @click="searching = true">
        <AppIcon name="search" :size="19" />
      </button>

      <div class="menu-wrap">
        <button class="avatar" :aria-expanded="menuOpen" @click="menuOpen = !menuOpen">
          <span class="av">{{ (me?.name || 'D').slice(0, 1).toUpperCase() }}</span>
          <AppIcon name="chevronDown" :size="14" :class="{ flip: menuOpen }" />
        </button>
        <Transition name="fade">
          <div v-if="menuOpen" class="dropdown right">
            <div class="dd-user">
              <span class="av lg">{{ (me?.name || 'D').slice(0, 1).toUpperCase() }}</span>
              <div>
                <div class="dd-name">{{ me?.name || '未登录' }}</div>
                <div class="tiny">{{ isAdmin ? '管理员' : '用户' }}</div>
              </div>
            </div>
            <button class="dd-item" @click="menuOpen = false; acctOpen = true">
              <AppIcon name="user" :size="16" /><span class="dd-name">账号设置</span>
            </button>
            <router-link to="/bangumi" class="dd-item">
              <AppIcon name="bookmark" :size="16" /><span class="dd-name">Bangumi 追番</span>
            </router-link>
            <router-link v-if="isAdmin" to="/settings" class="dd-item">
              <AppIcon name="settings" :size="16" /><span class="dd-name">设置</span>
            </router-link>
            <button class="dd-item" @click="logout">
              <AppIcon name="logout" :size="16" /><span class="dd-name">退出登录</span>
            </button>
          </div>
        </Transition>
      </div>
    </div>
  </header>

  <SearchOverlay v-model:open="searching" :libraries="libraries" />
  <AccountDialog v-model:open="acctOpen" />

  <nav class="tabbar">
    <router-link
      v-for="t in tabs" :key="t.to" :to="t.to" class="tab"
      :class="{ exact: t.exact }"
    >
      <AppIcon :name="t.icon" :size="21" />
      <span>{{ t.label }}</span>
    </router-link>
  </nav>
</template>

<style scoped>
.topbar {
  position: fixed;
  inset: 0 0 auto 0;
  z-index: 900;
  height: var(--header-h);
  display: flex;
  align-items: center;
  transition: background var(--t-mid), border-color var(--t-mid), backdrop-filter var(--t-mid);
  border-bottom: 1px solid transparent;
  background: transparent;
}
.topbar.overlay:not(.solid) { background: linear-gradient(to bottom, rgba(4, 5, 8, 0.82), rgba(4, 5, 8, 0)); }
.topbar.solid {
  background: rgba(9, 10, 14, 0.82);
  backdrop-filter: blur(18px) saturate(1.4);
  -webkit-backdrop-filter: blur(18px) saturate(1.4);
  border-bottom-color: var(--line-1);
}
.inner {
  width: 100%;
  max-width: var(--page-max);
  margin: 0 auto;
  padding: 0 var(--gutter);
  display: flex;
  align-items: center;
  gap: var(--sp-3);
}
.brand { display: flex; align-items: center; gap: 10px; margin-right: var(--sp-4); }
.logo {
  display: grid; place-items: center;
  width: 30px; height: 30px;
  border-radius: 10px;
  background: linear-gradient(150deg, var(--brand-2), #d98a12 70%);
  color: var(--brand-ink);
  box-shadow: 0 4px 16px rgba(240, 169, 46, 0.32), inset 0 1px 0 rgba(255, 255, 255, 0.5);
}
.wordmark {
  font-family: var(--font-display);
  font-size: 17px;
  font-weight: 800;
  letter-spacing: -0.035em;
}
.wordmark span { color: var(--text-3); font-weight: 600; }

.nav { display: flex; align-items: center; gap: 2px; }
.nav-link {
  position: relative;
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 8px 12px;
  border-radius: var(--r-s);
  font-size: var(--fs-14);
  font-weight: 550;
  color: var(--text-2);
  transition: color var(--t-fast), background var(--t-fast);
}
.nav-link:hover { color: #fff; background: rgba(255, 255, 255, 0.06); }
.nav-link.nav-active { color: #fff; }
.nav-link.nav-active::after {
  content: "";
  position: absolute;
  left: 12px; right: 12px; bottom: 2px;
  height: 2px;
  border-radius: 2px;
  background: var(--brand);
}
.flip { transform: rotate(180deg); transition: transform var(--t-fast); }

.icon-btn.round { color: var(--text-2); }
.topbar.overlay:not(.solid) .icon-btn.round:hover { background: rgba(255, 255, 255, 0.12); }

.avatar {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 4px 8px 4px 4px;
  border-radius: var(--r-pill);
  transition: background var(--t-fast);
}
.avatar:hover { background: rgba(255, 255, 255, 0.08); }
.av {
  display: grid; place-items: center;
  width: 30px; height: 30px;
  border-radius: 50%;
  background: linear-gradient(150deg, #3d4450, #22262e);
  border: 1px solid var(--line-2);
  font-size: var(--fs-13);
  font-weight: 700;
  color: var(--text-1);
}
.av.lg { width: 38px; height: 38px; font-size: var(--fs-15); }

.menu-wrap { position: relative; }
.dropdown {
  position: absolute;
  top: calc(100% + 10px);
  left: 0;
  min-width: 244px;
  padding: 7px;
  border-radius: var(--r-m);
  background: rgba(16, 18, 23, 0.96);
  border: 1px solid var(--line-2);
  box-shadow: var(--sh-3);
  backdrop-filter: blur(20px);
  -webkit-backdrop-filter: blur(20px);
  z-index: 950;
}
.dropdown.right { left: auto; right: 0; min-width: 232px; }
.dd-title {
  padding: 8px 10px 6px;
  font-size: var(--fs-11);
  font-weight: 700;
  letter-spacing: 0.12em;
  text-transform: uppercase;
  color: var(--text-4);
}
.dd-item {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 9px 10px;
  border-radius: var(--r-s);
  color: var(--text-2);
  font-size: var(--fs-13);
  text-align: left;
  transition: background var(--t-fast), color var(--t-fast);
}
.dd-item:hover { background: rgba(255, 255, 255, 0.07); color: #fff; }
.dd-item.accent { color: var(--brand); }
.dd-name { flex: 1; font-weight: 600; }
.dd-user { display: flex; align-items: center; gap: 11px; padding: 10px; border-bottom: 1px solid var(--line-1); margin-bottom: 6px; }
.dd-empty { padding: 10px; }

.tabbar { display: none; }

@media (max-width: 900px) {
  .desktop-only { display: none; }
  .tabbar {
    display: flex;
    position: fixed;
    inset: auto 0 0 0;
    z-index: 900;
    padding: 6px 4px calc(4px + env(safe-area-inset-bottom));
    background: rgba(9, 10, 14, 0.9);
    backdrop-filter: blur(20px) saturate(1.3);
    -webkit-backdrop-filter: blur(20px) saturate(1.3);
    border-top: 1px solid var(--line-1);
  }
  .tab {
    flex: 1;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 3px;
    padding: 6px 2px;
    border-radius: var(--r-s);
    color: var(--text-4);
    font-size: 10.5px;
    font-weight: 600;
    transition: color var(--t-fast);
  }
  .tab.router-link-active { color: var(--brand); }
  .tab.router-link-active svg { filter: drop-shadow(0 2px 8px rgba(240, 169, 46, 0.4)); }
  .brand { margin-right: 0; }
}
@media (max-width: 520px) {
  .wordmark span { display: none; }
}
</style>
