<script setup>
import { ref, computed, onMounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, fmtSize } from '../api.js'
import AppIcon from '../components/AppIcon.vue'
import EmptyState from '../components/EmptyState.vue'

const route = useRoute()
const router = useRouter()

const sources = ref([])
const data = ref(null)
const error = ref('')
const busyPath = ref('')
const loading = ref(false)
const filter = ref('')
const sort = ref('name')

const sourceId = computed(() => route.params.id || null)
const path = computed(() => route.query.path || '/')
const source = computed(() => sources.value.find((s) => String(s.id) === String(sourceId.value)))

const crumbs = computed(() => {
  const parts = (path.value || '/').split('/').filter(Boolean)
  const list = [{ name: '根目录', path: '/' }]
  let acc = ''
  for (const p of parts) {
    acc += '/' + p
    list.push({ name: p, path: acc })
  }
  return list
})

const rawEntries = computed(() => data.value?.entries || [])

const entries = computed(() => {
  const term = filter.value.trim().toLowerCase()
  let list = rawEntries.value
  if (term) list = list.filter((e) => e.name.toLowerCase().includes(term))
  const byName = (a, b) => a.name.localeCompare(b.name, 'zh-Hans-CN', { numeric: true })
  const sorter = {
    name: (a, b) => (b.is_dir - a.is_dir) || byName(a, b),
    size: (a, b) => (b.is_dir - a.is_dir) || (b.size || 0) - (a.size || 0),
    kind: (a, b) => (b.is_dir - a.is_dir) || (b.video - a.video) || byName(a, b),
  }[sort.value] || byName
  return [...list].sort(sorter)
})

const stats = computed(() => {
  const dirs = rawEntries.value.filter((e) => e.is_dir).length
  const vids = rawEntries.value.filter((e) => !e.is_dir && e.video).length
  return { dirs, vids, total: rawEntries.value.length }
})

const sortedSources = computed(() => [...sources.value].sort((a, b) => a.name.localeCompare(b.name, 'zh-CN')))

onMounted(async () => {
  try { sources.value = (await api.get('/api/sources')) || [] } catch (e) { error.value = e.message }
  if (sourceId.value) load()
})

async function load() {
  error.value = ''
  loading.value = true
  data.value = null
  try {
    data.value = await api.get(`/api/browse/${sourceId.value}?path=${encodeURIComponent(path.value)}`)
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

watch(sourceId, () => {
  filter.value = ''
  if (sourceId.value) load()
  else data.value = null
})
watch(path, () => { filter.value = ''; if (sourceId.value) load() })

function open(entry) {
  router.push({ path: `/browse/${sourceId.value}`, query: { path: entry.path } })
}

async function play(entry) {
  if (busyPath.value) return
  busyPath.value = entry.path
  error.value = ''
  try {
    const resp = await api.get(`/api/browseplay/${sourceId.value}?path=${encodeURIComponent(entry.path)}`)
    router.push(`/play/${resp.file_id}`)
  } catch (e) {
    error.value = e.message
    busyPath.value = ''
  }
}

function goCrumb(c) {
  router.push({ path: `/browse/${sourceId.value}`, query: { path: c.path } })
}

function up() {
  const parts = (path.value || '/').split('/').filter(Boolean)
  parts.pop()
  goCrumb({ path: '/' + parts.join('/') })
}

const srcIcon = (t) => (t === 'openlist' ? 'cloud' : t === 'webdav' ? 'globe' : t === 'bilibili' ? 'tv' : 'hdd')
function srcDetail(s) {
  if (s.type === 'bilibili') return s.config?.bili_cookie ? 'B站番剧 · 已配置账号' : 'B站番剧 · 未登录(480P)'
  return s.config?.base_url || s.config?.webdav_url || s.config?.root || ''
}
function ext(name) {
  const i = name.lastIndexOf('.')
  return i > 0 ? name.slice(i + 1).toLowerCase() : ''
}
</script>

<template>
  <div class="browse">
    <!-- ================= source picker ================= -->
    <template v-if="!sourceId">
      <div class="page">
        <div class="head">
          <div>
            <span class="eyebrow">文件浏览</span>
            <h1 class="display-1 small-display">直接播放,不必建库</h1>
            <p class="muted" style="max-width:62ch">
              浏览任意存储源里的目录,点开视频即播。播放同样经过决策引擎:
              该 302 直连就 302,iOS 播不了的自动转封装,并支持断点续播。
              B 站番剧的音视频是两条流,由服务端合流后播放(视频直拷,不重编码),并带上弹幕。
            </p>
          </div>
        </div>

        <div v-if="error" class="flash err"><AppIcon name="alert" :size="16" /> {{ error }}</div>

        <div v-if="sortedSources.length" class="src-grid">
          <router-link v-for="s in sortedSources" :key="s.id" :to="`/browse/${s.id}`" class="src-card">
            <span class="src-icon"><AppIcon :name="srcIcon(s.type)" :size="20" /></span>
            <span class="src-body">
              <span class="src-name">{{ s.name }}</span>
              <span class="tiny truncate">{{ s.type }}<template v-if="srcDetail(s)"> · {{ srcDetail(s) }}</template></span>
            </span>
            <AppIcon name="chevronRight" :size="16" class="arrow" />
          </router-link>
        </div>

        <EmptyState
          v-else icon="cloud" title="还没有存储源"
          text="先到设置里添加 OpenList / WebDAV 或本地目录,然后就能在这里浏览了。"
        >
          <router-link to="/settings" class="btn primary"><AppIcon name="plus" :size="16" /> 添加存储源</router-link>
        </EmptyState>
      </div>
    </template>

    <!-- ================= directory ================= -->
    <template v-else>
      <div class="dir-bar">
        <div class="page dir-bar-inner">
          <router-link to="/browse" class="btn ghost sm"><AppIcon name="arrowLeft" :size="15" /> 存储源</router-link>
          <div class="crumbs no-scrollbar">
            <template v-for="(c, i) in crumbs" :key="c.path">
              <AppIcon v-if="i" name="chevronRight" :size="13" class="sep" />
              <button class="crumb" :class="{ last: i === crumbs.length - 1 }" @click="goCrumb(c)">{{ c.name }}</button>
            </template>
          </div>
        </div>
      </div>

      <div class="page">
        <div v-if="error" class="flash err"><AppIcon name="alert" :size="16" /> {{ error }}</div>

        <div class="dir-tools">
          <div class="search-box">
            <AppIcon name="search" :size="16" />
            <input v-model="filter" class="input" placeholder="在当前目录中筛选…" />
          </div>
          <div class="segmented">
            <button :class="{ on: sort === 'name' }" @click="sort = 'name'">名称</button>
            <button :class="{ on: sort === 'kind' }" @click="sort = 'kind'">类型</button>
            <button :class="{ on: sort === 'size' }" @click="sort = 'size'">大小</button>
          </div>
          <button class="btn sm" :disabled="path === '/'" @click="up"><AppIcon name="chevronUp" :size="15" /> 上一级</button>
          <span class="spacer"></span>
          <span class="tiny">{{ stats.dirs }} 个目录 · {{ stats.vids }} 个视频</span>
        </div>

        <div v-if="loading" class="files card flat">
          <div v-for="i in 6" :key="i" class="sk line" style="height:44px;margin:8px 14px" />
        </div>

        <div v-else-if="entries.length" class="files">
          <div
            v-for="e in entries" :key="e.path"
            class="file-row" :class="{ dir: e.is_dir, video: !e.is_dir && e.video }"
          >
            <button v-if="e.is_dir" class="file-main" @click="open(e)">
              <span class="f-icon dir"><AppIcon name="folder" :size="18" /></span>
              <span class="f-name truncate">{{ e.name }}</span>
              <span class="f-tag tiny">目录</span>
              <AppIcon name="chevronRight" :size="15" class="f-go" />
            </button>

            <button v-else-if="e.video" class="file-main" @click="play(e)">
              <span class="f-icon vid"><AppIcon name="video" :size="18" /></span>
              <span class="f-name truncate">{{ e.name }}</span>
              <span v-if="ext(e.name)" class="badge plain ext">{{ ext(e.name).toUpperCase() }}</span>
              <span class="f-size tiny num">{{ fmtSize(e.size) }}</span>
              <span class="f-play" :class="{ busy: busyPath === e.path }">
                <template v-if="busyPath === e.path">
                  <span class="spinner" /> 准备中
                </template>
                <template v-else><AppIcon name="play" :size="13" filled /> 播放</template>
              </span>
            </button>

            <div v-else class="file-main dead">
              <span class="f-icon"><AppIcon name="file" :size="18" /></span>
              <span class="f-name truncate muted">{{ e.name }}</span>
              <span v-if="ext(e.name)" class="badge plain ext">{{ ext(e.name).toUpperCase() }}</span>
              <span class="f-size tiny num">{{ fmtSize(e.size) }}</span>
            </div>
          </div>
        </div>

        <EmptyState
          v-else-if="filter"
          icon="search" title="没有匹配的文件" text="换个关键词,或清空筛选。"
        >
          <button class="btn" @click="filter = ''">清空筛选</button>
        </EmptyState>

        <EmptyState v-else icon="folderOpen" title="这个目录是空的" text="换一个目录,或检查存储源里的路径是否正确。" />
      </div>
    </template>
  </div>
</template>

<style scoped>
.head { margin: var(--sp-6) 0 var(--sp-5); display: flex; flex-direction: column; gap: 6px; }
.small-display { font-size: clamp(28px, 4.2vw, 48px); letter-spacing: -0.03em; }
.head p { margin: 6px 0 0; }

.src-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 320px), 1fr)); gap: var(--sp-4); margin-top: var(--sp-5); }
.src-card {
  display: flex; align-items: center; gap: 14px;
  padding: 16px 18px;
  border-radius: var(--r-m);
  border: 1px solid var(--line-1);
  background: linear-gradient(180deg, rgba(255, 255, 255, 0.035), rgba(255, 255, 255, 0)), var(--bg-1);
  transition: all var(--t-fast);
}
.src-card:hover { border-color: var(--brand); transform: translateY(-2px); box-shadow: var(--sh-2); }
.src-icon {
  display: grid; place-items: center;
  width: 44px; height: 44px; flex: none;
  border-radius: var(--r-m);
  background: var(--brand-tint);
  color: var(--brand);
  border: 1px solid rgba(240, 169, 46, 0.24);
}
.src-body { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.src-name { font-size: var(--fs-15); font-weight: 650; }
.arrow { color: var(--text-4); }
.src-card:hover .arrow { color: var(--text-1); }

.dir-bar {
  position: sticky;
  top: var(--header-h);
  z-index: 600;
  background: rgba(9, 10, 14, 0.88);
  backdrop-filter: blur(18px) saturate(1.4);
  -webkit-backdrop-filter: blur(18px) saturate(1.4);
  border-bottom: 1px solid var(--line-1);
}
.dir-bar-inner { display: flex; align-items: center; gap: var(--sp-3); padding-top: 10px; padding-bottom: 10px; }
.crumbs { display: flex; align-items: center; gap: 2px; overflow-x: auto; scrollbar-width: none; }
.crumbs::-webkit-scrollbar { display: none; }
.sep { color: var(--text-4); flex: none; }
.crumb {
  padding: 5px 8px; border-radius: var(--r-xs);
  color: var(--text-3); font-size: var(--fs-13); font-weight: 550;
  transition: all var(--t-fast); white-space: nowrap;
}
.crumb:hover { background: rgba(255, 255, 255, 0.07); color: var(--text-1); }
.crumb.last { color: var(--text-1); font-weight: 650; }

.dir-tools { display: flex; align-items: center; gap: var(--sp-3); flex-wrap: wrap; padding: var(--sp-5) 0 var(--sp-4); }
.dir-tools .search-box { flex: 1; min-width: 200px; max-width: 360px; }
.dir-tools .search-box .input { height: 34px; min-height: 34px; font-size: var(--fs-13); }

.files { display: flex; flex-direction: column; gap: 2px; }
.file-row { border-radius: var(--r-s); }
.file-row + .file-row { border-top: 1px solid var(--line-1); border-radius: 0; }
.file-main {
  display: flex; align-items: center; gap: 12px;
  width: 100%; padding: 11px 14px;
  border-radius: var(--r-s);
  text-align: left;
  transition: background var(--t-fast);
}
.file-row.dir .file-main:hover, .file-row.video .file-main:hover { background: var(--bg-2); }
.file-main.dead { cursor: default; }
.f-icon {
  display: grid; place-items: center;
  width: 34px; height: 34px; flex: none;
  border-radius: var(--r-xs);
  background: rgba(255, 255, 255, 0.05);
  color: var(--text-3);
}
.f-icon.dir { color: var(--brand); background: var(--brand-tint); }
.f-icon.vid { color: #7cc4ff; background: rgba(88, 166, 255, 0.14); }
.f-name { flex: 1; min-width: 0; font-size: var(--fs-14); }
.f-tag, .ext { flex: none; }
.f-size { flex: none; width: 84px; text-align: right; }
.f-go { color: var(--text-4); flex: none; }
.file-row.dir:hover .f-go { color: var(--text-1); transform: translateX(2px); transition: transform var(--t-fast); }
.f-play {
  display: inline-flex; align-items: center; gap: 6px;
  flex: none;
  height: 30px; padding: 0 13px;
  border-radius: var(--r-pill);
  background: var(--brand); color: var(--brand-ink);
  font-size: var(--fs-12); font-weight: 700;
  opacity: 0; transform: translateX(6px);
  transition: all var(--t-fast);
}
.file-row.video:hover .f-play { opacity: 1; transform: translateX(0); }
.f-play.busy { opacity: 1; transform: none; background: var(--bg-3); color: var(--text-1); }
.spinner {
  width: 12px; height: 12px; border-radius: 50%;
  border: 2px solid rgba(255, 255, 255, 0.25);
  border-top-color: #fff;
  animation: spin 0.8s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }

@media (hover: none) {
  .f-play { opacity: 1; transform: none; }
}
@media (max-width: 720px) {
  .f-size { display: none; }
  .ext { display: none; }
  .dir-tools .search-box { max-width: none; }
  .dir-bar-inner { padding-top: 8px; padding-bottom: 8px; }
}
</style>
