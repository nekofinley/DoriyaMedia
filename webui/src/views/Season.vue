<script setup>
import { ref, onMounted, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, imgUrl, artIf, fmtSize } from '../api.js'
import AppIcon from '../components/AppIcon.vue'
import BiliBadge from '../components/BiliBadge.vue'
import SmartImage from '../components/SmartImage.vue'
import EmptyState from '../components/EmptyState.vue'

const route = useRoute()
const router = useRouter()
const data = ref(null)
const show = ref(null)
const siblings = ref([])
const error = ref('')
const picked = ref({})
const hovered = ref(0)

const season = computed(() => data.value?.item)
const episodes = computed(() => data.value?.episodes || [])
const heroStill = computed(() => {
  const ep = episodes.value.find((e) => e.still_path || e.backdrop_path)
  if (!ep) return ''
  return ep.still_path ? imgUrl(ep.id, 'still') : artIf(ep, 'backdrop')
})

function versionsOf(ep) { return ep.versions || [] }
function fileOf(ep) {
  const vs = versionsOf(ep)
  return picked.value[ep.id] || (vs.length ? vs[0].id : 0) || ep.file_id || 0
}
function versionOf(ep) {
  const vs = versionsOf(ep)
  return vs.find((x) => x.id === picked.value[ep.id]) || vs[0] || null
}
function sizeOf(ep) {
  const v = versionOf(ep)
  return v?.size ? fmtSize(v.size) : ''
}
function playEp(ep) {
  const id = fileOf(ep)
  if (!id) {
    error.value = `「${ep.name || '第 ' + ep.episode + ' 集'}」没有关联的视频文件——请重新扫描媒体库。`
    return
  }
  router.push(`/play/${id}`)
}
function epNo(ep) { return ep.episode ? String(ep.episode) : '' }

async function load() {
  error.value = ''
  try {
    data.value = await api.get(`/api/items/${route.params.id}`)
    show.value = data.value.show || null
    const parentId = data.value.item?.parent_id
    if (!show.value && parentId) {
      try { show.value = (await api.get(`/api/items/${parentId}`)).item } catch {}
    }
    if (parentId) {
      try {
        const s = await api.get(`/api/items/${parentId}`)
        show.value = s.item || show.value
        siblings.value = s.seasons || []
      } catch {}
    }
    const sel = {}
    for (const ep of episodes.value) {
      const vs = ep.versions || []
      if (vs.length) sel[ep.id] = vs[0].id
      else if (ep.file_id) sel[ep.id] = ep.file_id
    }
    picked.value = sel
  } catch (e) {
    error.value = e.message
  }
}
onMounted(load)
</script>

<template>
  <div class="season-page">
    <!-- ================= banner ================= -->
    <header class="banner">
      <div class="banner-art">
        <SmartImage v-if="heroStill" :src="heroStill" :alt="season?.name" :seed="season?.name" eager />
        <div v-else class="fallback-art" />
      </div>
      <div class="banner-scrim"></div>

      <div class="banner-inner page">
        <router-link v-if="show" :to="`/item/${show.id}`" class="poster-link">
          <div class="poster"><SmartImage :src="artIf(show, 'poster')" :alt="show.name" :seed="show.name" /></div>
        </router-link>
        <div class="copy">
          <div class="crumb">
            <router-link to="/" class="tiny">首页</router-link>
            <AppIcon name="chevronRight" :size="12" />
            <router-link v-if="show" :to="`/item/${show.id}`" class="tiny">{{ show.name }}</router-link>
          </div>
          <h1 class="display-1 title">{{ season?.name || '季' }}</h1>
          <div class="meta">
            <span v-if="show" class="badge hd">{{ show.name }}</span>
            <span class="badge plain">{{ episodes.length }} 集</span>
            <span v-if="show?.year" class="badge plain">{{ show.year }}</span>
            <span v-if="show?.rating" class="badge rating"><AppIcon name="star" :size="11" filled />{{ Number(show.rating).toFixed(1) }}</span>
          </div>
          <p v-if="show?.overview" class="ov clamp-3">{{ show.overview }}</p>
        </div>
      </div>
    </header>

    <div class="page body">
      <div v-if="error" class="flash err"><AppIcon name="alert" :size="16" /> {{ error }}</div>

      <!-- season switcher -->
      <div v-if="siblings.length > 1" class="season-switch">
        <span class="label">切换季</span>
        <div class="chips">
          <router-link
            v-for="s in siblings" :key="s.id" :to="`/season/${s.id}`"
            class="chip" :class="{ on: String(s.id) === String(route.params.id) }"
          >
            {{ s.name }}
          </router-link>
        </div>
      </div>

      <div class="section-head">
        <h2 class="section-title">剧集列表</h2>
        <span class="tiny">共 {{ episodes.length }} 集 · 点击任意一集开始播放</span>
      </div>

      <div v-if="episodes.length" class="eps stagger">
        <article
          v-for="(ep, i) in episodes" :key="ep.id"
          class="ep" :class="{ hot: hovered === ep.id }"
          :style="{ '--i': Math.min(i, 11) }"
          @mouseenter="hovered = ep.id" @mouseleave="hovered = 0"
        >
          <div class="ep-no">{{ epNo(ep) }}</div>

          <button class="ep-still" :aria-label="`播放 ${ep.name}`" @click="playEp(ep)">
            <SmartImage v-if="ep.still_path" :src="imgUrl(ep.id, 'still')" :alt="ep.name" :seed="ep.name" />
            <div v-else class="ph">
              <AppIcon name="film" :size="20" :stroke="1.5" />
              <span>{{ epNo(ep) ? `第 ${epNo(ep)} 集` : '剧集' }}</span>
            </div>
            <BiliBadge v-if="ep.source_type === 'bilibili'" />
            <span class="play-chip"><AppIcon name="play" :size="20" filled /></span>
          </button>

          <div class="ep-main">
            <div class="ep-head">
              <h3 class="ep-name">{{ ep.name || `第 ${ep.episode} 集` }}</h3>
              <span v-if="sizeOf(ep)" class="badge plain num">{{ sizeOf(ep) }}</span>
            </div>
            <div class="ep-sub tiny">
              <span v-if="ep.air_date"><AppIcon name="calendar" :size="12" /> {{ ep.air_date }}</span>
              <span v-if="versionsOf(ep).length > 1"><AppIcon name="layers" :size="12" /> {{ versionsOf(ep).length }} 个版本</span>
            </div>
            <p v-if="ep.overview" class="ep-ov clamp-2">{{ ep.overview }}</p>
          </div>

          <div class="ep-right">
            <div v-if="versionsOf(ep).length > 1" class="ver-picker">
              <button
                v-for="v in versionsOf(ep)" :key="v.id"
                class="chip sm" :class="{ on: picked[ep.id] === v.id }"
                @click="picked[ep.id] = v.id"
              >
                {{ v.label || '默认' }}
              </button>
            </div>
            <button class="btn play sm" @click="playEp(ep)">
              <AppIcon name="play" :size="14" filled /> 播放
            </button>
          </div>
        </article>
      </div>

      <EmptyState v-else-if="season" icon="tv" title="这一季还没有识别到剧集" text="扫描规则支持「第 1 季 / S01 / 纯数字」等目录结构,重新扫描试试。" />
    </div>
  </div>
</template>

<style scoped>
.banner { position: relative; min-height: min(52vh, 460px); display: flex; align-items: flex-end; overflow: hidden; }
.banner-art { position: absolute; inset: 0; }
.banner-art :deep(.smart-img) { width: 100%; height: 100%; }
.banner-art :deep(img) { filter: brightness(0.62) saturate(1.05); }
.fallback-art {
  width: 100%; height: 100%;
  background:
    radial-gradient(60% 80% at 25% 10%, rgba(240, 169, 46, 0.14), transparent 62%),
    var(--bg-1);
}
.banner-scrim {
  position: absolute; inset: 0;
  background:
    linear-gradient(to top, var(--bg-0) 2%, rgba(7, 8, 11, 0.72) 36%, rgba(7, 8, 11, 0.3) 76%, rgba(7, 8, 11, 0.55) 100%),
    linear-gradient(100deg, rgba(5, 6, 9, 0.86), transparent 66%);
}
.banner-inner {
  position: relative;
  width: 100%;
  display: flex;
  gap: clamp(18px, 2.6vw, 32px);
  align-items: flex-end;
  padding-top: calc(var(--header-h) + 50px);
  padding-bottom: clamp(20px, 3vw, 36px);
}
.poster-link { flex: none; }
.poster {
  width: clamp(104px, 11vw, 156px);
  aspect-ratio: 2/3;
  border-radius: var(--r-m);
  overflow: hidden;
  border: 1px solid var(--line-2);
  box-shadow: var(--sh-3);
  background: var(--bg-2);
}
.copy { flex: 1; min-width: 0; }
.crumb { display: flex; align-items: center; gap: 6px; color: var(--text-4); margin-bottom: 10px; }
.crumb a:hover { color: var(--text-1); }
.title { margin-bottom: 10px; text-shadow: 0 6px 30px rgba(0, 0, 0, 0.6); }
.meta { display: flex; align-items: center; gap: 7px; flex-wrap: wrap; margin-bottom: 12px; }
.ov { color: var(--text-2); font-size: var(--fs-14); line-height: 1.66; max-width: 72ch; margin: 0; }

.body { padding-top: clamp(18px, 2.6vw, 32px); }
.season-switch { display: flex; align-items: center; gap: 14px; flex-wrap: wrap; margin-bottom: var(--sp-5); }
.season-switch .label { font-size: var(--fs-12); font-weight: 600; color: var(--text-4); }
.chips { display: flex; gap: 8px; flex-wrap: wrap; }

.eps { display: flex; flex-direction: column; gap: var(--sp-3); }
.ep {
  display: flex;
  align-items: center;
  gap: var(--sp-4);
  padding: var(--sp-4);
  border-radius: var(--r-m);
  border: 1px solid var(--line-1);
  background: var(--bg-1);
  transition: border-color var(--t-fast), background var(--t-fast), transform var(--t-fast);
}
.ep:hover { border-color: var(--line-3); background: var(--bg-2); transform: translateX(3px); }
.ep-no {
  flex: none;
  width: 34px;
  text-align: center;
  font-family: var(--font-display);
  font-size: var(--fs-24);
  font-weight: 800;
  color: var(--text-4);
  letter-spacing: -0.03em;
}
.ep:hover .ep-no { color: var(--brand); }
.ep-still {
  position: relative;
  flex: none;
  width: clamp(150px, 15vw, 196px);
  aspect-ratio: 16/9;
  border-radius: var(--r-s);
  overflow: hidden;
  background: var(--bg-3);
  display: block;
}
.ep-still :deep(img) { transition: transform var(--t-slow); }
.ep:hover .ep-still :deep(img) { transform: scale(1.06); }
.ep-still .ph {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 6px;
  height: 100%;
  color: var(--text-4);
  font-size: var(--fs-12);
  font-weight: 650;
  background: linear-gradient(150deg, var(--bg-2), var(--bg-1));
}
.play-chip {
  position: absolute; inset: 0;
  display: grid; place-items: center;
  background: rgba(4, 5, 8, 0.42);
  color: #fff;
  opacity: 0;
  transition: opacity var(--t-mid);
}
.ep:hover .play-chip { opacity: 1; }
.ep-main { flex: 1; min-width: 0; }
.ep-head { display: flex; align-items: center; gap: 10px; }
.ep-name { font-size: var(--fs-15); font-weight: 650; }
.ep-sub { display: flex; align-items: center; gap: 14px; margin-top: 5px; }
.ep-sub svg { display: inline-block; vertical-align: -2px; }
.ep-ov { color: var(--text-3); font-size: var(--fs-13); line-height: 1.6; margin: 8px 0 0; max-width: 82ch; }
.ep-right { display: flex; flex-direction: column; align-items: flex-end; gap: 10px; flex: none; }
.ver-picker { display: flex; gap: 6px; flex-wrap: wrap; justify-content: flex-end; max-width: 280px; }

@media (max-width: 980px) {
  .ep { flex-wrap: wrap; }
  .ep-right { width: 100%; flex-direction: row; justify-content: space-between; align-items: center; }
}
@media (max-width: 720px) {
  .banner { min-height: 40vh; }
  .banner-inner { flex-direction: column; align-items: flex-start; }
  .poster { width: 96px; }
  .ep { padding: var(--sp-3); gap: var(--sp-3); }
  .ep-no { width: 22px; font-size: var(--fs-18); }
  .ep-still { width: 118px; }
  .ep-ov { display: none; }
  .ver-picker { max-width: none; }
}
</style>
