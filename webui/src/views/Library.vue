<script setup>
import { ref, onMounted, watch, computed } from 'vue'
import { useRoute } from 'vue-router'
import { api, imgUrl, artIf } from '../api.js'
import AppIcon from '../components/AppIcon.vue'
import PosterCard from '../components/PosterCard.vue'
import SmartImage from '../components/SmartImage.vue'
import SkeletonGrid from '../components/SkeletonGrid.vue'
import EmptyState from '../components/EmptyState.vue'

const route = useRoute()
const libId = computed(() => route.params.id)
const items = ref([])
const total = ref(0)
const libraries = ref([])
const search = ref('')
const type = ref(String(route.query.type || ''))
const sort = ref('added')
const view = ref(localStorage.getItem('dm:libview') || 'grid')
const loading = ref(false)
const error = ref('')

const lib = computed(() => libraries.value.find((l) => String(l.id) === String(libId.value)))
const banner = computed(() => items.value.find((i) => i.backdrop_path) || items.value[0] || null)
const isSeries = computed(() => lib.value?.type === 'series')
const activeFilters = computed(() => (type.value ? 1 : 0) + (search.value ? 1 : 0) + (sort.value !== 'added' ? 1 : 0))

watch(view, (v) => localStorage.setItem('dm:libview', v))

const sortOptions = [
  { value: 'added', label: '最近添加' },
  { value: 'name', label: '名称' },
  { value: 'year', label: '年份' },
  { value: 'rating', label: '评分' },
]

async function load() {
  loading.value = true
  error.value = ''
  try {
    const q = new URLSearchParams({
      library_id: libId.value, type: type.value, search: search.value, sort: sort.value, limit: '500',
    })
    // The shelf holds films and series. Seasons and episodes only join the list
    // when they are what was asked for — a type filter, or a search that has to
    // be able to find an episode by name.
    if (!type.value && !search.value) q.set('roots', '1')
    const r = await api.get(`/api/items?${q}`)
    items.value = r.items || []
    total.value = r.total || 0
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  try { libraries.value = (await api.get('/api/libraries')) || [] } catch {}
  await load()
})
watch([libId, sort, type], load)
let deb
watch(search, () => { clearTimeout(deb); deb = setTimeout(load, 320) })

function reset() { search.value = ''; type.value = isSeries.value ? 'show' : ''; sort.value = 'added' }
</script>

<template>
  <div class="lib-page">
    <!-- ================= banner ================= -->
    <header class="banner">
      <div class="banner-art">
        <SmartImage
          v-if="banner" :src="artIf(banner, 'backdrop')" :alt="lib?.name" :seed="lib?.name || 'library'"
          eager
        />
        <div v-else class="banner-fallback" />
      </div>
      <div class="banner-scrim"></div>
      <div class="banner-inner page">
        <div class="crumb">
          <router-link to="/" class="tiny">首页</router-link>
          <AppIcon name="chevronRight" :size="12" />
          <span class="tiny">媒体库</span>
        </div>
        <h1 class="display-1 lib-title">{{ lib?.name || '媒体库' }}</h1>
        <div class="meta">
          <span class="badge hd"><AppIcon :name="isSeries ? 'tv' : 'film'" :size="12" />{{ isSeries ? '剧集库' : '电影库' }}</span>
          <span class="badge plain">{{ total }} 个条目</span>
          <span v-if="lib?.path" class="badge plain">路径 {{ lib.path }}</span>
        </div>
      </div>
    </header>

    <!-- ================= filters ================= -->
    <div class="filter-bar">
      <div class="filter-inner">
        <div class="search-box">
          <AppIcon name="search" :size="16" />
          <input v-model="search" class="input" :placeholder="`在「${lib?.name || '媒体库'}」中搜索…`" />
          <button v-if="search" class="clear" aria-label="清空" @click="search = ''"><AppIcon name="close" :size="14" /></button>
        </div>

        <div class="segmented type-seg">
          <button :class="{ on: type === '' }" @click="type = ''">全部</button>
          <button :class="{ on: type === 'movie' }" @click="type = 'movie'">电影</button>
          <button :class="{ on: type === 'show' }" @click="type = 'show'">剧集</button>
        </div>

        <div class="right">
          <div class="select-wrap">
            <AppIcon name="sort" :size="15" class="select-icon" />
            <select v-model="sort" class="input sort-select">
              <option v-for="o in sortOptions" :key="o.value" :value="o.value">{{ o.label }}</option>
            </select>
          </div>
          <div class="segmented view-seg">
            <button :class="{ on: view === 'grid' }" title="网格视图" @click="view = 'grid'"><AppIcon name="grid" :size="15" /></button>
            <button :class="{ on: view === 'list' }" title="列表视图" @click="view = 'list'"><AppIcon name="list" :size="15" /></button>
          </div>
        </div>
      </div>
    </div>

    <div class="page results">
      <div v-if="error" class="flash err"><AppIcon name="alert" :size="16" /> {{ error }}</div>

      <div v-if="loading" class="skeletons"><SkeletonGrid :count="14" /></div>

      <template v-else-if="items.length">
        <div class="count-line tiny">
          显示 {{ items.length }} / {{ total }} 个条目
          <button v-if="activeFilters" class="reset" @click="reset">清除筛选</button>
        </div>

        <!-- grid -->
        <div v-if="view === 'grid'" v-reveal.stagger class="grid-auto">
          <PosterCard
            v-for="(it, i) in items" :key="it.id" :item="it" :eager="i < 8"
            :style="{ '--i': Math.min(i, 11) }"
          />
        </div>

        <!-- list -->
        <div v-else v-reveal.stagger class="list">
          <router-link
            v-for="(it, i) in items" :key="it.id" :to="`/item/${it.id}`" class="list-row"
            :style="{ '--i': Math.min(i, 11) }"
          >
            <div class="thumb">
              <SmartImage :src="artIf(it, 'poster')" :alt="it.name" :seed="it.name" />
            </div>
            <div class="main">
              <div class="title truncate">{{ it.name }}</div>
              <div class="tiny truncate" v-if="it.original_title && it.original_title !== it.name">{{ it.original_title }}</div>
              <div class="tiny clamp-2 sub" v-if="it.overview">{{ it.overview }}</div>
            </div>
            <div class="side">
              <span v-if="it.rating" class="badge rating"><AppIcon name="star" :size="11" filled />{{ Number(it.rating).toFixed(1) }}</span>
              <span v-if="it.year" class="badge">{{ it.year }}</span>
            </div>
            <AppIcon name="chevronRight" :size="16" class="go" />
          </router-link>
        </div>
      </template>

      <EmptyState
        v-else-if="search || activeFilters"
        icon="search"
        :title="`没有匹配的条目`"
        text="换个关键词,或清除筛选条件再看看。"
      >
        <button class="btn" @click="reset">清除筛选</button>
      </EmptyState>

      <EmptyState
        v-else
        icon="film"
        title="这个媒体库还没有内容"
        text="到 设置 → 媒体库 点「扫描」,扫描完成后海报就会出现在这里。"
      >
        <router-link to="/settings" class="btn primary"><AppIcon name="scan" :size="16" /> 去设置里扫描</router-link>
      </EmptyState>
    </div>
  </div>
</template>

<style scoped>
.banner { position: relative; min-height: min(46vh, 400px); display: flex; align-items: flex-end; overflow: hidden; }
.banner-art { position: absolute; inset: 0; }
.banner-art :deep(.smart-img) { width: 100%; height: 100%; }
.banner-art :deep(img), .banner-art :deep(.fallback) { filter: saturate(1.05) brightness(0.72); }
.banner-fallback {
  width: 100%; height: 100%;
  background:
    radial-gradient(60% 80% at 20% 10%, rgba(240, 169, 46, 0.16), transparent 60%),
    radial-gradient(50% 70% at 80% 30%, rgba(96, 130, 255, 0.14), transparent 62%),
    var(--bg-1);
}
.banner-scrim {
  position: absolute; inset: 0;
  background:
    linear-gradient(to top, var(--bg-0) 3%, rgba(7, 8, 11, 0.7) 34%, rgba(7, 8, 11, 0.25) 72%, rgba(7, 8, 11, 0.5) 100%),
    linear-gradient(100deg, rgba(5, 6, 9, 0.86), transparent 62%);
}
.banner-inner { position: relative; width: 100%; padding-top: calc(var(--header-h) + 40px); padding-bottom: clamp(20px, 3vw, 36px); }
.crumb { display: flex; align-items: center; gap: 6px; color: var(--text-4); margin-bottom: 10px; }
.crumb a:hover { color: var(--text-1); }
.lib-title { margin-bottom: 12px; text-shadow: 0 6px 30px rgba(0, 0, 0, 0.6); }
.meta { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }

.filter-bar {
  position: sticky;
  top: var(--header-h);
  z-index: 600;
  background: rgba(9, 10, 14, 0.86);
  backdrop-filter: blur(18px) saturate(1.4);
  -webkit-backdrop-filter: blur(18px) saturate(1.4);
  border-bottom: 1px solid var(--line-1);
}
.filter-inner {
  max-width: var(--page-max);
  margin: 0 auto;
  padding: 12px var(--gutter);
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  flex-wrap: wrap;
}
.search-box { flex: 1; min-width: 220px; max-width: 460px; }
.search-box .input { height: 40px; min-height: 40px; }
.search-box .clear {
  position: absolute; right: 8px;
  display: grid; place-items: center;
  width: 24px; height: 24px; border-radius: 50%;
  color: var(--text-4);
}
.search-box .clear:hover { background: rgba(255, 255, 255, 0.1); color: var(--text-1); }
.filter-inner .right { display: flex; align-items: center; gap: var(--sp-2); margin-left: auto; }
.select-wrap { position: relative; display: flex; align-items: center; }
.select-icon { position: absolute; left: 11px; color: var(--text-4); pointer-events: none; }
.sort-select { min-height: 34px; height: 34px; padding: 0 30px 0 32px; font-size: var(--fs-13); border-radius: var(--r-pill); width: auto; }
.view-seg button { width: 32px; padding: 0; display: grid; place-items: center; }

.results { padding-top: var(--sp-5); }
.skeletons { margin-top: var(--sp-4); }
.count-line { display: flex; align-items: center; gap: 12px; margin-bottom: var(--sp-4); }
.reset { color: var(--brand); font-size: var(--fs-12); font-weight: 600; }
.reset:hover { text-decoration: underline; }

.list { display: flex; flex-direction: column; gap: var(--sp-2); }
.list-row {
  display: flex;
  align-items: center;
  gap: var(--sp-4);
  padding: 10px 14px 10px 10px;
  border-radius: var(--r-m);
  border: 1px solid transparent;
  transition: background var(--t-fast), border-color var(--t-fast);
}
.list-row:hover { background: var(--bg-1); border-color: var(--line-1); }
.list-row .thumb { width: 52px; flex: none; aspect-ratio: 2/3; border-radius: var(--r-xs); overflow: hidden; background: var(--bg-2); }
.list-row .main { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.list-row .title { font-size: var(--fs-15); font-weight: 600; }
.list-row .sub { color: var(--text-4); max-width: 88ch; }
.list-row .side { display: flex; align-items: center; gap: 6px; flex: none; }
.list-row .go { color: var(--text-4); }
.list-row:hover .go { color: var(--text-1); }

@media (max-width: 720px) {
  .banner { min-height: 38vh; }
  .filter-inner { padding: 10px var(--gutter); gap: var(--sp-2); }
  .search-box { max-width: none; }
  .filter-inner .right { margin-left: 0; width: 100%; justify-content: space-between; }
  .type-seg { order: 3; width: 100%; }
  .type-seg button { flex: 1; }
  .list-row .sub { display: none; }
}
</style>
