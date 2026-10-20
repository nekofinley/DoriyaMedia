<script setup>
import { ref, onMounted, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, imgUrl, artIf, fmtSize, fmtDate, parseGenres, streamBadges } from '../api.js'
import AppIcon from '../components/AppIcon.vue'
import BiliBadge from '../components/BiliBadge.vue'
import SmartImage from '../components/SmartImage.vue'
import EmptyState from '../components/EmptyState.vue'
import { toast } from '../toast.js'

const route = useRoute()
const router = useRouter()
const data = ref(null)
const error = ref('')
const selectedFile = ref(null)
const busy = ref(false)
const expanded = ref(false)
const pinOpen = ref(false)
const pinBGM = ref('')

const item = computed(() => data.value?.item)
const library = computed(() => data.value?.library)
const isShow = computed(() => item.value?.type === 'show')
const isEpisode = computed(() => item.value?.type === 'episode')
const files = computed(() => data.value?.files || [])
const seasons = computed(() => data.value?.seasons || [])
const genres = computed(() => parseGenres(item.value))
const currentFile = computed(() => files.value.find((f) => f.id === selectedFile.value) || files.value[0] || null)
const badges = computed(() => streamBadges(currentFile.value?.media))
const audioTracks = computed(() => currentFile.value?.media?.audio || [])
const subtitles = computed(() => currentFile.value?.subtitles || [])
const matched = computed(() => {
  const it = item.value
  if (!it) return ''
  if (it.bgm_id) return `Bangumi #${it.bgm_id}`
  if (it.tmdb_id) return `TMDB #${it.tmdb_id}`
  return ''
})
const backTarget = computed(() => (item.value?.parent_id ? `/item/${item.value.parent_id}` : '/'))

async function load() {
  try {
    data.value = await api.get(`/api/items/${route.params.id}`)
    if (files.value.length) selectedFile.value = files.value[0].id
    if (route.query.play === '1' && files.value.length) play()
  } catch (e) {
    error.value = e.message
  }
}
onMounted(load)

function play() {
  const f = currentFile.value
  if (!f) return
  router.push(`/play/${f.id}`)
}

async function refresh() {
  busy.value = true
  try {
    await api.post(`/api/items/${route.params.id}/refresh`, {})
    await load()
    toast.ok('已重新识别')
  } catch (e) {
    toast.err(e.message)
  } finally {
    busy.value = false
  }
}

async function refreshBGM() {
  if (!pinBGM.value) return
  busy.value = true
  try {
    await api.post(`/api/items/${route.params.id}/refresh`, { bgm_id: Number(pinBGM.value) })
    await load()
    pinOpen.value = false
    toast.ok(`已按 Bangumi #${pinBGM.value} 匹配`)
  } catch (e) {
    toast.err(e.message)
  } finally {
    busy.value = false
  }
}

function seasonLabel(s) {
  return s.name || (s.season ? `第 ${s.season} 季` : '季')
}
function seasonBackdrop(s) {
  const ep = (s.episodes || []).find((e) => e.still_path || e.backdrop_path)
  if (!ep) return ''
  return ep.still_path ? imgUrl(ep.id, 'still') : artIf(ep, 'backdrop')
}
</script>

<template>
  <div v-if="item" class="detail">
    <!-- ================= hero ================= -->
    <header class="hero">
      <div class="hero-art">
        <SmartImage
          :src="artIf(item, 'backdrop')" :alt="item.name" :seed="item.name" eager
        />
      </div>
      <div class="hero-scrim"></div>

      <div class="hero-inner page">
        <div class="poster-wrap">
          <div class="poster">
            <SmartImage :src="artIf(item, 'poster')" :alt="item.name" :seed="item.name" />
          </div>
        </div>

        <div class="head">
          <div class="crumb">
            <router-link :to="backTarget" class="tiny">
              <AppIcon name="arrowLeft" :size="13" /> {{ isEpisode ? '返回剧集' : '返回' }}
            </router-link>
          </div>

          <h1 class="display-1 title">{{ item.name }}</h1>
          <div v-if="item.original_title && item.original_title !== item.name" class="orig">{{ item.original_title }}</div>

          <div class="meta">
            <span v-if="item.rating" class="badge rating"><AppIcon name="star" :size="11" filled />{{ Number(item.rating).toFixed(1) }}</span>
            <span v-if="item.year" class="badge hd">{{ item.year }}</span>
            <span class="badge hd">{{ isShow ? '剧集' : isEpisode ? '剧集' : '电影' }}</span>
            <span v-if="isEpisode && item.season" class="badge">S{{ String(item.season).padStart(2, '0') }}E{{ String(item.episode || 0).padStart(2, '0') }}</span>
            <span v-if="item.air_date" class="badge plain">{{ item.air_date }}</span>
            <span v-for="g in genres" :key="g" class="badge plain">{{ g }}</span>
          </div>

          <p class="overview" :class="{ open: expanded }">{{ item.overview || '暂无简介。可以在右上角「重新识别」用 TMDB / Bangumi 补全信息。' }}</p>
          <button v-if="(item.overview || '').length > 120" class="more-link" @click="expanded = !expanded">
            {{ expanded ? '收起' : '展开全部' }}
            <AppIcon :name="expanded ? 'chevronUp' : 'chevronDown'" :size="13" />
          </button>

          <div class="actions">
            <button v-if="!isShow" class="btn play lg" :disabled="!currentFile" @click="play">
              <AppIcon name="play" :size="17" filled /> 播放
            </button>
            <router-link v-else-if="seasons.length" :to="`/season/${seasons[0].id}`" class="btn play lg">
              <AppIcon name="play" :size="17" filled /> 开始观看
            </router-link>

            <button class="btn lg" :disabled="busy" @click="refresh">
              <AppIcon name="refresh" :size="16" :class="{ spin: busy }" /> {{ busy ? '识别中…' : '重新识别' }}
            </button>

            <div class="pin-wrap">
              <button class="btn lg" :class="{ on: pinOpen }" @click="pinOpen = !pinOpen">
                <AppIcon name="wand" :size="16" /> 按 ID 匹配
              </button>
              <Transition name="fade">
                <div v-if="pinOpen" class="pin-pop">
                  <div class="label">Bangumi 条目 ID</div>
                  <div class="pin-row">
                    <input v-model="pinBGM" class="input" placeholder="如 328609" inputmode="numeric" @keyup.enter="refreshBGM" />
                    <button class="btn primary" :disabled="busy || !pinBGM" @click="refreshBGM">匹配</button>
                  </div>
                  <div class="tiny">番剧名字有歧义时,在 bgm.tv 找到条目后把 URL 里的数字填进来即可钉住这一部。</div>
                </div>
              </Transition>
            </div>
          </div>

          <div v-if="matched" class="matched tiny"><AppIcon name="link" :size="13" /> 已匹配 {{ matched }}</div>
        </div>
      </div>
    </header>

    <div class="page body">
      <div v-if="error" class="flash err"><AppIcon name="alert" :size="16" /> {{ error }}</div>

      <!-- ================= show: seasons ================= -->
      <template v-if="isShow">
        <section v-if="seasons.length" v-reveal.stagger class="section">
          <div class="section-head">
            <h2 class="section-title">季与版本</h2>
            <span class="tiny">共 {{ seasons.length }} 季</span>
          </div>
          <div class="season-grid">
            <router-link
              v-for="(s, i) in seasons" :key="s.id" :to="`/season/${s.id}`" class="season-card"
              :style="{ '--i': Math.min(i, 8) }"
            >
              <div class="still">
                <SmartImage v-if="seasonBackdrop(s)" :src="seasonBackdrop(s)" :alt="s.name" :seed="s.name" />
                <div v-else class="ph"><span>{{ seasonLabel(s) }}</span></div>
                <BiliBadge v-if="s.source_type === 'bilibili'" />
                <span class="play-chip"><AppIcon name="play" :size="15" filled /></span>
              </div>
              <div class="meta">
                <div class="n truncate">{{ seasonLabel(s) }}</div>
                <div class="tiny">{{ (s.episodes || []).length }} 集</div>
              </div>
            </router-link>
          </div>
        </section>
        <EmptyState v-else icon="tv" title="还没有识别到季" text="重新扫描媒体库,或检查目录命名是否符合「第 1 季 / S01」这类结构。" />
      </template>

      <!-- ================= movie / episode: files ================= -->
      <template v-else>
        <section v-if="files.length" v-reveal class="section files-section">
          <div class="section-head">
            <h2 class="section-title">文件与版本</h2>
            <span class="tiny">{{ files.length }} 个可播放文件</span>
          </div>

          <div class="files-layout">
            <div class="versions stagger">
              <button
                v-for="(f, i) in files" :key="f.id"
                class="version-card" :class="{ on: f.id === selectedFile }"
                :style="{ '--i': i }"
                @click="selectedFile = f.id"
              >
                <span class="check"><AppIcon name="check" :size="13" /></span>
                <span class="v-main">
                  <span class="v-label truncate">{{ f.label || '默认版本' }}</span>
                  <span class="tiny truncate">{{ f.path }}</span>
                </span>
                <span class="v-side">
                  <span class="tiny num">{{ fmtSize(f.size) }}</span>
                  <span v-if="f.media?.video?.height" class="badge hd">{{ f.media.video.height >= 1000 ? '1080p+' : f.media.video.height + 'p' }}</span>
                </span>
              </button>
            </div>

            <aside class="tech-card card">
              <div class="card-title"><AppIcon name="layers" :size="16" /> 技术规格</div>
              <div class="tech-badges">
                <span v-for="b in badges" :key="b" class="badge">{{ b }}</span>
                <span v-if="currentFile" class="badge">{{ fmtSize(currentFile.size) }}</span>
              </div>

              <div class="tech-list">
                <div v-if="audioTracks.length" class="tech-item">
                  <AppIcon name="audio" :size="15" />
                  <div>
                    <div class="k">音轨 {{ audioTracks.length }} 条</div>
                    <div class="tiny">
                      {{ audioTracks.map(a => [String(a.codec || '').toUpperCase(), a.lang, a.channels ? a.channels + 'ch' : ''].filter(Boolean).join(' ')).join(' / ') }}
                    </div>
                  </div>
                </div>
                <div class="tech-item">
                  <AppIcon name="subtitles" :size="15" />
                  <div>
                    <div class="k">字幕 {{ subtitles.length ? subtitles.length + ' 条' : '无' }}</div>
                    <div class="tiny clamp-2">
                      {{ subtitles.length ? subtitles.map(s => s.title || s.lang || s.format).join(' / ') : '未找到外挂字幕' }}
                    </div>
                  </div>
                </div>
                <div v-if="item.air_date" class="tech-item">
                  <AppIcon name="calendar" :size="15" />
                  <div><div class="k">播出日期</div><div class="tiny">{{ item.air_date }}</div></div>
                </div>
                <div class="tech-item">
                  <AppIcon name="clock" :size="15" />
                  <div><div class="k">加入媒体库</div><div class="tiny">{{ fmtDate(item.added_at) }}</div></div>
                </div>
                <div v-if="library" class="tech-item">
                  <AppIcon name="folder" :size="15" />
                  <div><div class="k">所属媒体库</div><div class="tiny">{{ library.name }}</div></div>
                </div>
              </div>

              <button class="btn play block mt-4" :disabled="!currentFile" @click="play">
                <AppIcon name="play" :size="16" filled /> 播放这个版本
              </button>

              <div class="play-hint tiny">
                <AppIcon name="sparkles" :size="12" />
                播放时自动判断 302 直连 / 代理 / HLS 转封装,无需手动选择。
              </div>
            </aside>
          </div>
        </section>

        <EmptyState
          v-else icon="file" title="没有找到视频文件"
          text="条目已识别,但没有关联的媒体文件。到设置里对该媒体库重新扫描一次。"
        />
      </template>
    </div>
  </div>

  <div v-else class="page">
    <div v-if="error" class="flash err"><AppIcon name="alert" :size="16" /> {{ error }}</div>
    <div v-else class="detail-skeleton">
      <div class="sk" style="height:360px;border-radius:var(--r-l)" />
      <div class="sk line mt-5" style="width:40%;height:22px" />
      <div class="sk line mt-3 w-70" />
    </div>
  </div>
</template>

<style scoped>
.hero { position: relative; min-height: min(72vh, 680px); display: flex; align-items: flex-end; }
.hero-art { position: absolute; inset: 0; overflow: hidden; }
.hero-art :deep(.smart-img) { width: 100%; height: 100%; }
.hero-art :deep(img), .hero-art :deep(.fallback) { filter: brightness(0.8); }
.hero-scrim {
  position: absolute; inset: 0;
  background:
    linear-gradient(to top, var(--bg-0) 1%, rgba(7, 8, 11, 0.82) 30%, rgba(7, 8, 11, 0.35) 68%, rgba(5, 6, 9, 0.72) 100%),
    linear-gradient(105deg, rgba(5, 6, 9, 0.9) 4%, rgba(5, 6, 9, 0.45) 44%, transparent 78%);
}
.hero-inner {
  position: relative;
  width: 100%;
  display: flex;
  gap: clamp(20px, 3vw, 40px);
  align-items: flex-end;
  padding-top: calc(var(--header-h) + 60px);
  padding-bottom: clamp(24px, 4vw, 48px);
}
.poster-wrap { flex: none; }
.poster {
  width: clamp(150px, 15vw, 224px);
  aspect-ratio: 2/3;
  border-radius: var(--r-l);
  overflow: hidden;
  border: 1px solid var(--line-2);
  box-shadow: var(--sh-4);
  background: var(--bg-2);
}
.head { flex: 1; min-width: 0; padding-bottom: 4px; }
.crumb { margin-bottom: 12px; }
.crumb a { display: inline-flex; align-items: center; gap: 6px; color: var(--text-3); }
.crumb a:hover { color: var(--text-1); }
.title { margin-bottom: 8px; text-shadow: 0 6px 36px rgba(0, 0, 0, 0.62); }
.orig { color: var(--text-3); font-size: var(--fs-15); margin-bottom: 12px; }
.meta { display: flex; align-items: center; gap: 7px; flex-wrap: wrap; margin-bottom: 14px; }
.overview {
  color: var(--text-2);
  font-size: var(--fs-15);
  line-height: 1.68;
  max-width: 68ch;
  margin-bottom: 4px;
  display: -webkit-box;
  -webkit-line-clamp: 3;
  -webkit-box-orient: vertical;
  overflow: hidden;
  text-shadow: 0 2px 18px rgba(0, 0, 0, 0.5);
}
.overview.open { -webkit-line-clamp: unset; }
.more-link {
  display: inline-flex; align-items: center; gap: 4px;
  color: var(--brand); font-size: var(--fs-13); font-weight: 600;
  margin-bottom: 14px;
}
.more-link:hover { text-decoration: underline; }
.actions { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; margin-top: 8px; }
.pin-wrap { position: relative; }
.pin-wrap .btn.on { border-color: var(--line-3); background: var(--bg-3); }
.pin-pop {
  position: absolute;
  top: calc(100% + 10px);
  right: 0;
  z-index: 800;
  width: min(340px, 84vw);
  padding: 16px;
  border-radius: var(--r-m);
  background: rgba(16, 18, 23, 0.97);
  border: 1px solid var(--line-2);
  box-shadow: var(--sh-3);
  backdrop-filter: blur(18px);
}
.pin-row { display: flex; gap: 8px; margin: 8px 0; }
.pin-row .input { min-height: 38px; height: 38px; }
.matched { display: flex; align-items: center; gap: 6px; margin-top: 12px; color: var(--text-4); }

.body { padding-top: clamp(20px, 3vw, 36px); }

/* seasons */
.season-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 262px), 1fr)); gap: var(--sp-4); }
.season-card {
  display: block;
  border-radius: var(--r-m);
  overflow: hidden;
  border: 1px solid var(--line-1);
  background: var(--bg-1);
  transition: border-color var(--t-fast), transform var(--t-fast), box-shadow var(--t-fast);
}
.season-card:hover { border-color: var(--line-3); transform: translateY(-3px); box-shadow: var(--sh-3); }
.season-card .still { position: relative; aspect-ratio: 16/9; background: var(--bg-3); overflow: hidden; }
.season-card .still :deep(img) { transition: transform var(--t-slow); }
.season-card:hover .still :deep(img) { transform: scale(1.05); }
.season-card .still .ph {
  display: grid; place-items: center; height: 100%;
  color: var(--text-3); font-weight: 700;
  background: linear-gradient(150deg, var(--bg-2), var(--bg-1));
}
.play-chip {
  position: absolute; right: 10px; bottom: 10px;
  display: grid; place-items: center;
  width: 34px; height: 34px; border-radius: 50%;
  background: rgba(255, 255, 255, 0.92); color: #0a0b0f;
  opacity: 0; transform: translateY(6px);
  transition: all var(--t-mid);
}
.season-card:hover .play-chip { opacity: 1; transform: translateY(0); }
.season-card .meta { padding: 12px 14px; }
.season-card .meta .n { font-size: var(--fs-15); font-weight: 650; margin-bottom: 2px; }

/* files */
.files-layout { display: grid; grid-template-columns: minmax(0, 1fr) 340px; gap: var(--sp-5); align-items: start; }
.versions { display: flex; flex-direction: column; gap: var(--sp-3); }
.version-card {
  position: relative;
  display: flex;
  align-items: center;
  gap: 14px;
  padding: 14px 16px;
  border-radius: var(--r-m);
  border: 1px solid var(--line-1);
  background: var(--bg-1);
  text-align: left;
  transition: all var(--t-fast);
}
.version-card:hover { border-color: var(--line-3); background: var(--bg-2); }
.version-card.on { border-color: var(--brand); background: var(--brand-tint); }
.version-card .check {
  display: grid; place-items: center;
  width: 22px; height: 22px; flex: none;
  border-radius: 50%;
  border: 1px solid var(--line-3);
  color: transparent;
  transition: all var(--t-fast);
}
.version-card.on .check { background: var(--brand); border-color: var(--brand); color: var(--brand-ink); }
.v-main { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.v-label { font-size: var(--fs-14); font-weight: 650; }
.v-side { display: flex; align-items: center; gap: 8px; flex: none; }

.tech-card { position: sticky; top: calc(var(--header-h) + 20px); }
.tech-badges { display: flex; flex-wrap: wrap; gap: 6px; margin: 6px 0 16px; }
.tech-list { display: flex; flex-direction: column; gap: 12px; }
.tech-item { display: flex; gap: 11px; align-items: flex-start; color: var(--text-3); }
.tech-item svg { margin-top: 2px; }
.tech-item .k { font-size: var(--fs-13); font-weight: 600; color: var(--text-2); }
.play-hint { display: flex; gap: 7px; align-items: flex-start; margin-top: 12px; line-height: 1.5; }
.spin { animation: spin 1s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }

.detail-skeleton { padding-top: calc(var(--header-h) + 80px); }

@media (max-width: 1080px) {
  .files-layout { grid-template-columns: 1fr; }
  .tech-card { position: static; }
}
@media (max-width: 720px) {
  /* the cover floats over the artwork; the text block stays bottom-aligned */
  .hero { min-height: min(56vh, 420px); }
  .hero-inner { flex-direction: column; align-items: stretch; gap: 0; }
  .poster-wrap {
    position: absolute;
    top: calc(var(--header-h) + 14px);
    right: var(--gutter);
    width: 104px;
  }
  .poster { width: 104px; border-radius: var(--r-m); }
  .head { padding-bottom: 0; }
  .title { font-size: clamp(24px, 7.5vw, 32px); }
  .overview { -webkit-line-clamp: 3; font-size: var(--fs-14); }
  .actions .btn { flex: 1; min-width: 128px; }
  .pin-pop { right: auto; left: 0; }
}
</style>
