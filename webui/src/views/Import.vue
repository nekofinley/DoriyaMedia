<script setup>
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from '../components/AppIcon.vue'
import { api } from '../api.js'

/**
 * Search-to-import loading page.
 *
 * A catalogue hit is a season that is not in any library yet. Opening it starts
 * the import job and this page polls it: the walk over the series subtree, the
 * item rows, then the metadata lookup. It replaces itself with the show's detail
 * page when the job says where that show is.
 */
const route = useRoute()
const router = useRouter()

const title = ref(String(route.query.title || '番剧'))
const cover = computed(() => String(route.query.cover || '').replace(/^http:/, 'https:'))
const coverBroken = ref(false)
const job = ref(null)
const state = ref(null)
const error = ref('')
const elapsed = ref(0)

let pollTimer = null
let ticker = null
let stopped = false

const steps = [
  { key: 'walk', label: '读取季度与剧集', hint: '向 B 站核对这个系列的每一季' },
  { key: 'organize', label: '建立剧集条目', hint: '把每一集写进媒体库' },
  { key: 'meta', label: '匹配封面与简介', hint: 'Bangumi / TMDB 补全元数据' },
  { key: 'done', label: '完成', hint: '正在打开详情页' },
]
const order = { walk: 0, organize: 1, meta: 2, done: 3, error: 0 }

const stage = computed(() => {
  if (error.value || job.value?.phase === 'error') return 'error'
  if (job.value?.phase === 'done') return 'done'
  const st = String(state.value?.phase || '')
  if (st === 'done') return 'done'
  if (st === 'meta' || job.value?.phase === 'meta') return 'meta'
  if (st === 'organize') return 'organize'
  return 'walk'
})

function pretty(line) {
  return String(line)
    .replace(/\[(?:series|season|ep)\s*\d+\]/gi, '')
    .split('/')
    .map((s) => s.trim())
    .filter(Boolean)
    .join(' · ')
}

const detail = computed(() => {
  if (error.value) return error.value
  if (job.value?.phase === 'done') return '入库完成,正在打开详情页…'
  const st = state.value || {}
  if (st.current) return pretty(st.current)
  if (stage.value === 'meta') return '正在匹配封面与简介…'
  return '正在连接 B 站…'
})

const pct = computed(() => {
  const st = state.value || {}
  if (!st.total) return 0
  return Math.max(2, Math.min(100, Math.round((st.done / st.total) * 100)))
})

onMounted(start)
onBeforeUnmount(stop)

async function start() {
  const library = Number(route.query.library)
  if (!library) {
    fail('缺少媒体库信息,请从搜索结果重新打开')
    return
  }
  ticker = setInterval(() => { elapsed.value++ }, 1000)
  try {
    const r = await api.post('/api/catalog/import', {
      library_id: library,
      source_id: Number(route.params.sourceId),
      season_id: Number(route.params.seasonId),
    })
    if (r.title) title.value = r.title
    job.value = r
    poll(r.job_id)
  } catch (e) {
    fail(e.message)
  }
}

async function poll(id) {
  if (stopped) return
  try {
    const r = await api.get(`/api/import/${id}`)
    job.value = r
    state.value = r.state || null
    if (r.error) { fail(r.error); return }
    if (r.phase === 'done' && r.item_id) {
      stop()
      // Let the last step show for a beat, then hand over to the detail page.
      setTimeout(() => router.replace(`/item/${r.item_id}`), 420)
      return
    }
    if (r.phase === 'done') { fail('入库结束,但没有找到条目'); return }
    pollTimer = setTimeout(() => poll(id), 400)
  } catch (e) {
    fail(e.message)
  }
}

function fail(msg) {
  error.value = msg || '入库失败'
  stop()
}

function stop() {
  stopped = true
  clearTimeout(pollTimer)
  clearInterval(ticker)
}

function back() {
  if (window.history.length > 1) router.back()
  else router.replace('/')
}
</script>

<template>
  <div class="import-page">
    <div class="page stage">
      <div class="poster-side">
        <div class="poster" :class="{ live: stage !== 'error' }">
          <img v-if="cover && !coverBroken" :src="cover" :alt="title" referrerpolicy="no-referrer" @error="coverBroken = true" />
          <div v-else class="ph">{{ title.slice(0, 2) }}</div>
          <div class="sheen"></div>
        </div>
      </div>

      <div class="info">
        <span class="eyebrow"><AppIcon name="cloud" :size="13" /> B 站片库 · 搜索入库</span>
        <h1 class="display-2">{{ title }}</h1>
        <p class="lede">
          这一部还没有进媒体库。现在从 B 站把它的季度与剧集建立进来,入库完成后会自动打开详情页。
        </p>

        <div class="bar-wrap" :class="{ indet: stage !== 'done' && !pct }">
          <div class="bar"><i :style="{ width: (stage === 'done' ? 100 : pct) + '%' }"></i></div>
          <span class="pct tiny num">{{ stage === 'done' ? '100%' : (pct ? pct + '%' : '') }}</span>
        </div>

        <div class="steps">
          <div
            v-for="(s, i) in steps" :key="s.key"
            class="step" :class="{
              done: stage === 'done' ? true : order[stage] > order[s.key],
              now: stage !== 'done' && stage !== 'error' && order[stage] === order[s.key],
              failed: stage === 'error' && i === 0,
            }"
          >
            <span class="dot">
              <AppIcon v-if="stage === 'done' || order[stage] > order[s.key]" name="check" :size="12" />
              <AppIcon v-else-if="stage === 'error' && i === 0" name="alert" :size="12" />
              <span v-else class="pulse"></span>
            </span>
            <span class="txt">
              <span class="k">{{ s.label }}</span>
              <span class="h">{{ s.hint }}</span>
            </span>
          </div>
        </div>

        <div class="live-line">
          <AppIcon :name="stage === 'error' ? 'alert' : 'refresh'" :size="14" :class="{ spin: stage !== 'error' && stage !== 'done' }" />
          <span class="truncate">{{ detail }}</span>
          <span v-if="stage !== 'error' && stage !== 'done'" class="tiny elapsed num">{{ elapsed }}s</span>
        </div>

        <div v-if="stage === 'error'" class="err-box">
          <div class="err-title">没有入库成功</div>
          <div class="err-msg">{{ error || job?.error }}</div>
          <div class="err-actions">
            <button class="btn" @click="back"><AppIcon name="arrowLeft" :size="15" /> 返回搜索</button>
            <button class="btn primary" @click="$router.go(0)"><AppIcon name="refresh" :size="15" /> 重试</button>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.import-page {
  min-height: calc(100dvh - var(--header-h));
  display: flex;
  align-items: center;
  background:
    radial-gradient(60% 60% at 18% 20%, rgba(240, 169, 46, 0.10), transparent 65%),
    radial-gradient(50% 60% at 88% 10%, rgba(90, 120, 255, 0.10), transparent 62%),
    var(--bg-0);
}
.stage {
  display: grid;
  grid-template-columns: 220px minmax(0, 1fr);
  gap: clamp(24px, 4vw, 56px);
  align-items: center;
  padding-top: clamp(24px, 6vh, 72px);
  padding-bottom: clamp(40px, 8vh, 96px);
}
.poster-side { display: flex; justify-content: center; }
.poster {
  position: relative;
  width: min(220px, 46vw);
  aspect-ratio: 2/3;
  border-radius: var(--r-l);
  overflow: hidden;
  border: 1px solid var(--line-2);
  background: var(--bg-2);
  box-shadow: var(--sh-4);
}
.poster img { width: 100%; height: 100%; object-fit: cover; display: block; }
.poster .ph {
  display: grid; place-items: center; height: 100%;
  font-size: 40px; font-weight: 800; color: var(--text-4);
  background: linear-gradient(150deg, var(--bg-2), var(--bg-1));
}
.poster.live::after {
  content: '';
  position: absolute; inset: 0;
  border-radius: inherit;
  border: 2px solid transparent;
  background: linear-gradient(140deg, rgba(240, 169, 46, 0.7), transparent 40%, transparent 60%, rgba(240, 169, 46, 0.35)) border-box;
  -webkit-mask: linear-gradient(#000 0 0) padding-box, linear-gradient(#000 0 0);
  -webkit-mask-composite: xor;
  mask-composite: exclude;
  pointer-events: none;
  animation: breathe 2.4s ease-in-out infinite;
}
.sheen {
  position: absolute; inset: 0;
  background: linear-gradient(115deg, transparent 30%, rgba(255, 255, 255, 0.09) 46%, transparent 60%);
  transform: translateX(-120%);
  animation: sheen 2.8s ease-in-out infinite;
  pointer-events: none;
}
@keyframes breathe { 50% { opacity: 0.35; } }
@keyframes sheen { 55%, 100% { transform: translateX(120%); } }

.info { min-width: 0; }
.eyebrow {
  display: inline-flex; align-items: center; gap: 7px;
  font-size: var(--fs-12); font-weight: 700; letter-spacing: 0.08em;
  color: var(--brand); text-transform: uppercase;
}
.display-2 { margin: 10px 0 12px; }
.lede { max-width: 54ch; color: var(--text-2); line-height: 1.7; margin-bottom: 26px; }

.bar-wrap { display: flex; align-items: center; gap: 12px; margin-bottom: 26px; }
.bar {
  flex: 1; height: 6px; border-radius: 99px;
  background: var(--bg-3); overflow: hidden;
}
.bar i {
  display: block; height: 100%; border-radius: inherit;
  background: linear-gradient(90deg, var(--brand), #ffd27a);
  transition: width 380ms var(--ease-out);
}
.bar-wrap.indet .bar { position: relative; }
.bar-wrap.indet .bar::after {
  content: ''; position: absolute; top: 0; bottom: 0; width: 34%;
  border-radius: inherit;
  background: linear-gradient(90deg, transparent, var(--brand), transparent);
  animation: scan 1.25s ease-in-out infinite;
}
@keyframes scan { from { left: -40%; } to { left: 100%; } }
.pct { color: var(--text-3); min-width: 38px; text-align: right; }

.steps { display: grid; gap: 12px; margin-bottom: 26px; }
.step { display: flex; gap: 12px; align-items: flex-start; opacity: 0.45; transition: opacity var(--t-fast); }
.step.now, .step.done, .step.failed { opacity: 1; }
.dot {
  flex: none;
  display: grid; place-items: center;
  width: 22px; height: 22px; border-radius: 50%;
  border: 1px solid var(--line-3); color: var(--text-3);
  margin-top: 1px;
}
.step.done .dot { background: var(--brand); border-color: var(--brand); color: var(--brand-ink); }
.step.now .dot { border-color: var(--brand); color: var(--brand); }
.step.failed .dot { border-color: #e5484d; color: #e5484d; }
.pulse {
  width: 7px; height: 7px; border-radius: 50%;
  background: var(--brand);
  box-shadow: 0 0 0 0 rgba(240, 169, 46, 0.5);
  animation: pulse 1.4s ease-out infinite;
}
@keyframes pulse {
  70% { box-shadow: 0 0 0 7px rgba(240, 169, 46, 0); }
  100% { box-shadow: 0 0 0 0 rgba(240, 169, 46, 0); }
}
.txt { display: flex; flex-direction: column; gap: 1px; min-width: 0; }
.k { font-size: var(--fs-14); font-weight: 650; color: var(--text-1); }
.h { font-size: var(--fs-12); color: var(--text-4); }

.live-line {
  display: flex; align-items: center; gap: 8px;
  padding: 10px 12px;
  border-radius: var(--r-s);
  background: var(--bg-1);
  border: 1px solid var(--line-1);
  color: var(--text-3);
  font-family: var(--font-mono);
  font-size: var(--fs-12);
  min-height: 40px;
}
.live-line .truncate { flex: 1; }
.elapsed { color: var(--text-4); }
.spin { animation: spin 1s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }

.err-box {
  margin-top: 18px; padding: 16px;
  border-radius: var(--r-m);
  border: 1px solid rgba(229, 72, 77, 0.35);
  background: rgba(229, 72, 77, 0.07);
}
.err-title { font-weight: 700; color: #ff8a8d; margin-bottom: 4px; }
.err-msg { font-size: var(--fs-13); color: var(--text-2); line-height: 1.6; word-break: break-word; }
.err-actions { display: flex; gap: 10px; margin-top: 14px; flex-wrap: wrap; }

@media (max-width: 760px) {
  .stage { grid-template-columns: 1fr; gap: 22px; padding-top: calc(var(--header-h) + 20px); }
  .poster-side { justify-content: flex-start; }
  .poster { width: 132px; border-radius: var(--r-m); }
  .lede { margin-bottom: 20px; }
}
</style>
