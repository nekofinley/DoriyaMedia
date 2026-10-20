<script setup>
import { ref, computed, onMounted } from 'vue'
import { api, imgUrl, artUrl, fmtRemaining, episodeLabel } from '../api.js'
import AppIcon from '../components/AppIcon.vue'
import BiliBadge from '../components/BiliBadge.vue'
import PosterCard from '../components/PosterCard.vue'
import MediaRail from '../components/MediaRail.vue'
import HeroBillboard from '../components/HeroBillboard.vue'
import SmartImage from '../components/SmartImage.vue'
import EmptyState from '../components/EmptyState.vue'
import SkeletonGrid from '../components/SkeletonGrid.vue'

const home = ref(null)
const error = ref('')
const loading = ref(true)

const movies = computed(() => home.value?.recent_movies || [])
const shows = computed(() => home.value?.recent_shows || [])
const continueList = computed(() => home.value?.continue_watching || [])
const libraries = computed(() => home.value?.libraries || [])
const movieLib = computed(() => libraries.value.find((l) => l.type === 'movie'))
const seriesLib = computed(() => libraries.value.find((l) => l.type === 'series'))

/** Billboard wants wide art — only titles that actually have a backdrop qualify. */
const featured = computed(() =>
  [...movies.value, ...shows.value]
    .filter((i) => i.backdrop_path)
    .slice()
    .sort((a, b) => (b.rating || 0) - (a.rating || 0))
    .slice(0, 5),
)

/** "评分最高" rail — a Top-10 style row with oversized numerals. */
const topRated = computed(() =>
  [...movies.value, ...shows.value]
    .filter((i) => i.rating > 0)
    .sort((a, b) => b.rating - a.rating)
    .slice(0, 10),
)

const empty = computed(() => !loading.value && !movies.value.length && !shows.value.length && !continueList.value.length)
const movieMore = computed(() => movieLib.value && (home.value?.movie_total || 0) > movies.value.length)
const showMore = computed(() => seriesLib.value && (home.value?.show_total || 0) > shows.value.length)

onMounted(async () => {
  try {
    home.value = await api.get('/api/home')
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
})

function libCount(lib) {
  if (lib.type === 'movie') return `${home.value?.movie_total || 0} 部电影`
  return `${home.value?.show_total || 0} 部剧集`
}
function libPath(lib) {
  return lib.path && lib.path !== '/' ? `路径 ${lib.path}` : '全部目录'
}
</script>

<template>
  <div class="home">
    <!-- ================= hero ================= -->
    <div v-if="loading" class="hero-skeleton">
      <div class="hs-inner page">
        <div class="sk line" style="width:120px;height:12px" />
        <div class="sk mt-4" style="width:min(520px,70%);height:56px;border-radius:14px" />
        <div class="sk line mt-4" style="width:min(300px,50%);height:14px" />
        <div class="sk mt-5" style="width:min(460px,80%);height:44px;border-radius:10px" />
      </div>
    </div>
    <HeroBillboard v-else-if="featured.length" :items="featured" />
    <div v-else class="fallback-hero">
      <div class="fh-inner page">
        <span class="eyebrow">DoriyaMedia</span>
        <h1 class="display-1">你的私人影院</h1>
        <p class="muted">挂载网盘或本地目录,扫描媒体库,然后在任何设备上直接播放。</p>
      </div>
    </div>

    <div class="page home-body">
      <div v-if="error" class="flash err">
        <AppIcon name="alert" :size="16" /> {{ error }}
      </div>

      <!-- ================= loading ================= -->
      <template v-if="loading">
        <section class="section">
          <div class="sk line" style="width:180px;height:20px;margin-bottom:18px" />
          <SkeletonGrid :count="7" />
        </section>
      </template>

      <!-- ================= empty library ================= -->
      <EmptyState
        v-else-if="empty"
        icon="sparkles"
        title="媒体库还是空的"
        text="三步就能开始:添加存储源 → 创建媒体库 → 扫描。也可以直接用「文件」浏览网盘目录,点开即播。"
      >
        <div class="row-flex wrap" style="justify-content:center">
          <router-link to="/settings" class="btn primary"><AppIcon name="plus" :size="16" /> 添加存储源</router-link>
          <router-link to="/browse" class="btn"><AppIcon name="folder" :size="16" /> 直接浏览文件</router-link>
        </div>
      </EmptyState>

      <template v-else>
        <!-- ================= continue watching ================= -->
        <section v-if="continueList.length" v-reveal class="section">
          <div class="section-head">
            <h2 class="section-title"><AppIcon name="history" :size="18" /> 继续观看</h2>
          </div>
          <MediaRail>
            <router-link
              v-for="(c, i) in continueList" :key="c.item.id + '-' + c.file_id"
              :to="`/play/${c.file_id}`" class="wide-card"
              :style="{ '--i': Math.min(i, 8) }"
            >
              <div class="still">
                <SmartImage :src="artUrl(c.item)" :alt="c.item.name" :seed="c.item.name" />
                <BiliBadge v-if="c.item.source_type === 'bilibili'" mini />
              </div>
              <span class="quick"><AppIcon name="play" :size="19" filled /></span>
              <div class="body">
                <div class="t truncate">{{ c.item.name }}</div>
                <div class="s">
                  <span v-if="episodeLabel(c.item)" class="ep">{{ episodeLabel(c.item) }}</span>
                  {{ fmtRemaining(c.position, c.duration) }}
                </div>
              </div>
              <div class="progress">
                <i :style="{ width: Math.min(100, c.percent) + '%' }"></i>
              </div>
            </router-link>
          </MediaRail>
        </section>

        <!-- ================= movies ================= -->
        <section v-if="movies.length" v-reveal class="section">
          <div class="section-head">
            <h2 class="section-title">最近添加的电影</h2>
            <router-link v-if="movieMore" :to="`/library/${movieLib.id}?type=movie`" class="more">
              全部 {{ home.movie_total }} 部 <AppIcon name="chevronRight" :size="14" />
            </router-link>
          </div>
          <MediaRail>
            <PosterCard
              v-for="(m, i) in movies" :key="m.id" :item="m"
              :style="{ '--i': Math.min(i, 8) }"
            />
          </MediaRail>
        </section>

        <!-- ================= shows ================= -->
        <section v-if="shows.length" v-reveal class="section">
          <div class="section-head">
            <h2 class="section-title">最近添加的剧集</h2>
            <router-link v-if="showMore" :to="`/library/${seriesLib.id}?type=show`" class="more">
              全部 {{ home.show_total }} 部 <AppIcon name="chevronRight" :size="14" />
            </router-link>
          </div>
          <MediaRail>
            <PosterCard
              v-for="(s, i) in shows" :key="s.id" :item="s"
              :style="{ '--i': Math.min(i, 8) }"
            />
          </MediaRail>
        </section>

        <!-- ================= top rated ================= -->
        <section v-if="topRated.length >= 4" v-reveal class="section">
          <div class="section-head">
            <h2 class="section-title"><AppIcon name="sparkles" :size="18" /> 评分最高</h2>
            <span class="tiny">来自 TMDB / Bangumi 评分</span>
          </div>
          <MediaRail>
            <PosterCard
              v-for="(t, i) in topRated" :key="t.id" :item="t"
              class="ranked" :rank="i + 1"
              :style="{ '--i': Math.min(i, 8) }"
            />
          </MediaRail>
        </section>

        <!-- ================= libraries ================= -->
        <section v-if="libraries.length" v-reveal.stagger class="section">
          <div class="section-head">
            <h2 class="section-title">我的媒体库</h2>
            <router-link v-if="$route.path !== '/settings'" to="/settings" class="more">
              管理 <AppIcon name="chevronRight" :size="14" />
            </router-link>
          </div>
          <div class="lib-grid">
            <router-link
              v-for="(l, i) in libraries" :key="l.id"
              :to="`/library/${l.id}?type=${l.type === 'series' ? 'show' : 'movie'}`"
              class="lib-tile" :style="{ '--i': i }"
            >
              <span class="lib-icon"><AppIcon :name="l.type === 'series' ? 'tv' : 'film'" :size="20" /></span>
              <span class="lib-meta">
                <span class="lib-name">{{ l.name }}</span>
                <span class="tiny">{{ libCount(l) }} · {{ libPath(l) }}</span>
              </span>
              <AppIcon name="chevronRight" :size="16" class="lib-arrow" />
            </router-link>
          </div>
        </section>
      </template>
    </div>
  </div>
</template>

<style scoped>
.home-body { padding-top: clamp(24px, 4vw, 52px); }

.hero-skeleton {
  min-height: min(68vh, 620px);
  display: flex;
  align-items: flex-end;
  background: linear-gradient(160deg, var(--bg-2), var(--bg-0) 70%);
  margin-bottom: clamp(20px, 3vw, 40px);
}
.hs-inner {
  width: 100%;
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  padding-bottom: clamp(28px, 5vw, 64px);
  padding-top: calc(var(--header-h) + 60px);
}

.fallback-hero {
  padding: calc(var(--header-h) + clamp(48px, 12vh, 120px)) 0 clamp(24px, 6vh, 64px);
  background:
    radial-gradient(70% 90% at 78% 0%, rgba(240, 169, 46, 0.14), transparent 62%),
    radial-gradient(50% 70% at 12% 20%, rgba(90, 120, 255, 0.12), transparent 60%),
    var(--bg-0);
  border-bottom: 1px solid var(--line-1);
}
.fh-inner { display: flex; flex-direction: column; gap: 10px; }
.fh-inner p { max-width: 56ch; margin: 0; }

.wide-card .s .ep {
  display: inline-block;
  margin-right: 6px;
  padding: 0 6px;
  border-radius: 5px;
  background: rgba(255, 255, 255, 0.14);
  color: var(--text-1);
  font-weight: 650;
  font-size: var(--fs-11);
}

/* ranked row: numeral bleeding into the poster, Top-10 style */
.ranked { padding-left: 30px; }
.ranked :deep(.rank) { left: -4px; }

.lib-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(min(100%, 288px), 1fr));
  gap: var(--sp-4);
}
.lib-tile {
  display: flex;
  align-items: center;
  gap: 14px;
  padding: 16px 18px;
  border-radius: var(--r-m);
  border: 1px solid var(--line-1);
  background: linear-gradient(180deg, rgba(255, 255, 255, 0.035), rgba(255, 255, 255, 0)), var(--bg-1);
  transition: border-color var(--t-fast), transform var(--t-fast), background var(--t-fast);
}
.lib-tile:hover { border-color: var(--line-3); transform: translateY(-2px); background: var(--bg-2); }
.lib-icon {
  display: grid; place-items: center;
  width: 42px; height: 42px;
  flex: none;
  border-radius: var(--r-m);
  background: var(--brand-tint);
  color: var(--brand);
  border: 1px solid rgba(240, 169, 46, 0.24);
}
.lib-meta { display: flex; flex-direction: column; gap: 2px; min-width: 0; }
.lib-name { font-size: var(--fs-15); font-weight: 650; letter-spacing: -0.01em; }
.lib-arrow { color: var(--text-4); margin-left: auto; transition: transform var(--t-fast), color var(--t-fast); }
.lib-tile:hover .lib-arrow { color: var(--text-1); transform: translateX(3px); }
</style>
