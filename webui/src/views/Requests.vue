<script setup>
import { ref, computed, watch, nextTick, onMounted, onBeforeUnmount } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from '../components/AppIcon.vue'
import EmptyState from '../components/EmptyState.vue'
import RequestPosterCard from '../components/RequestPosterCard.vue'
import SmartImage from '../components/SmartImage.vue'
import { api } from '../api.js'
import { me } from '../auth.js'
import { takeMediaRequestDraft } from '../requestDraft.js'
import { toast } from '../toast.js'

const router = useRouter()
const route = useRoute()
const q = ref(String(route.query.q || ''))
const input = ref(null)
const loading = ref(false)
const searched = ref(false)
const results = ref([])
const warnings = ref([])
const searchError = ref('')
const selected = ref(null)
const note = ref('')
const submitting = ref(false)
const confirmPanel = ref(null)
const myRequests = ref([])
const loadingMine = ref(false)
const pendingBGMID = ref(Number(route.query.bgm) || 0)
let timer = null
let searchSeq = 0
let mineTimer = null
let mineFetching = false
let mineReload = false

const isAdmin = computed(() => me.value?.role === 'admin')
const biliHits = computed(() => results.value.filter((r) => r.provider === 'bilibili'))
const bgmHits = computed(() => results.value.filter((r) => r.provider === 'bangumi'))
const biliImportableCount = computed(() => biliHits.value.filter((r) => !r.in_library_item_id).length)
const myPending = computed(() => myRequests.value.filter((r) => r.status === 'pending').length)
const myDone = computed(() => myRequests.value.filter((r) => r.status === 'completed').length)
const canSubmit = computed(() => selected.value && !selected.value.in_library_item_id && !submitting.value)
const selectedMeta = computed(() => {
  const r = selected.value
  if (!r) return ''
  return [
    r.year || '',
    r.episodes ? `${r.episodes} 集` : '',
    r.rating ? `★ ${Number(r.rating).toFixed(1)}` : '',
  ].filter(Boolean).join(' · ')
})
const selectedProvider = computed(() => ({
  bilibili: 'B 站片库',
  bangumi: 'Bangumi 番剧资料',
  manual: '手动填写',
}[selected.value?.provider] || '求片'))

watch(q, (value) => {
  clearTimeout(timer)
  // Invalidate any request already in flight so a slower old result cannot
  // overwrite the next search while the debounce timer is waiting.
  searchSeq++
  searchError.value = ''
  const term = value.trim()
  if (!term) {
    results.value = []
    warnings.value = []
    searched.value = false
    loading.value = false
    return
  }
  if ([...term].length < 2) {
    results.value = []
    warnings.value = []
    searched.value = false
    loading.value = false
    return
  }
  loading.value = true
  timer = setTimeout(runSearch, 320)
})

watch(() => route.query.q, (value) => {
  const next = String(value || '')
  if (next !== q.value) q.value = next
})

onMounted(async () => {
  loadMine()
  mineTimer = setInterval(() => {
    if (!document.hidden) loadMine({ silent: true })
  }, 30000)

  const seededBGMID = Number(route.query.bgm) || 0
  if (seededBGMID) {
    const draft = takeMediaRequestDraft(seededBGMID)
    if (draft) {
      selected.value = { ...draft, provider: 'bangumi' }
      pendingBGMID.value = 0
      q.value = draft.title
      await nextTick()
      confirmPanel.value?.scrollIntoView({ behavior: 'smooth', block: 'center' })
    }
  }

  if ([...q.value.trim()].length >= 2) runSearch()
  if (window.matchMedia?.('(pointer: fine)').matches) nextTick(() => input.value?.focus())
})
onBeforeUnmount(() => {
  clearTimeout(timer)
  clearInterval(mineTimer)
})

async function runSearch() {
  const term = q.value.trim()
  if ([...term].length < 2) return
  const seq = ++searchSeq
  loading.value = true
  searchError.value = ''
  try {
    const out = await api.get(`/api/requests/search?q=${encodeURIComponent(term)}`)
    if (seq !== searchSeq) return
    results.value = out.items || []
    warnings.value = out.warnings || []
    searched.value = true
    await selectBGMSeed()
  } catch (e) {
    if (seq !== searchSeq) return
    results.value = []
    warnings.value = []
    searchError.value = e.message
    searched.value = true
    await selectBGMSeed()
  } finally {
    if (seq === searchSeq) loading.value = false
  }
}

async function selectBGMSeed() {
  const bgmID = pendingBGMID.value
  if (!bgmID) return
  pendingBGMID.value = 0
  const match = bgmHits.value.find((hit) => Number(hit.bgm_id) === bgmID)
  if (match) {
    selected.value = { ...match }
  } else {
    const title = String(route.query.q || q.value || '').trim()
    if (!title) return
    selected.value = {
      provider: 'bangumi',
      title,
      original_title: '',
      year: 0,
      cover: '',
      overview: '',
      episodes: 0,
      rating: 0,
      bgm_id: bgmID,
    }
  }
  await nextTick()
  confirmPanel.value?.scrollIntoView({ behavior: 'smooth', block: 'center' })
}

function onSearchEnter() {
  if (loading.value) return
  const first = [...biliHits.value, ...bgmHits.value].find((r) => !r.in_library_item_id)
  if (first) pickHit(first)
}

async function pickHit(hit) {
  if (hit.in_library_item_id) {
    toast.info('这部已经在媒体库里，可以直接打开观看')
    return
  }
  selected.value = { ...hit }
  pendingBGMID.value = 0
  note.value = ''
  await nextTick()
  confirmPanel.value?.scrollIntoView({ behavior: 'smooth', block: 'center' })
}

async function pickManual() {
  const title = q.value.trim()
  if (!title) return
  selected.value = {
    provider: 'manual',
    title,
    original_title: '',
    year: 0,
    cover: '',
    overview: '',
    episodes: 0,
    rating: 0,
  }
  pendingBGMID.value = 0
  note.value = ''
  await nextTick()
  confirmPanel.value?.scrollIntoView({ behavior: 'smooth', block: 'center' })
}

function clearSelection() {
  selected.value = null
  note.value = ''
}

async function submitRequest() {
  const r = selected.value
  if (!r || submitting.value) return
  submitting.value = true
  try {
    await api.post('/api/requests', {
      provider: r.provider,
      title: r.title,
      original_title: r.original_title || '',
      year: Number(r.year) || 0,
      cover_url: r.cover || '',
      overview: r.overview || '',
      rating: Number(r.rating) || 0,
      episodes: Number(r.episodes) || 0,
      source_id: Number(r.source_id) || 0,
      library_id: Number(r.library_id) || 0,
      season_id: Number(r.season_id) || 0,
      bgm_id: Number(r.bgm_id) || 0,
      note: note.value.trim(),
    })
    toast.ok('求片已提交，管理员会在后台看到它')
    clearSelection()
    q.value = ''
    results.value = []
    warnings.value = []
    searched.value = false
    await loadMine()
  } catch (e) {
    toast.err(e.message)
  } finally {
    submitting.value = false
  }
}

async function loadMine({ silent = false } = {}) {
  if (mineFetching) {
    if (!silent) mineReload = true
    return
  }
  do {
    mineReload = false
    mineFetching = true
    if (!silent) loadingMine.value = true
    try {
      myRequests.value = (await api.get('/api/requests')).items || []
    } catch (e) {
      if (!silent) toast.err(e.message)
    } finally {
      mineFetching = false
      if (!silent) loadingMine.value = false
    }
  } while (mineReload)
}

function openMine(req) {
  if (req.item_id) {
    router.push(`/item/${req.item_id}`)
    return
  }
  toast.info(req.status === 'pending' ? '管理员还在处理这部求片' : '这部已经完成处理')
}

function providerText(provider) {
  return provider === 'bilibili' ? 'B 站' : provider === 'bangumi' ? 'Bangumi' : '手动'
}
</script>

<template>
  <div class="requests-page">
    <section class="request-hero">
      <div class="hero-grid" aria-hidden="true"></div>
      <div class="hero-glow a" aria-hidden="true"></div>
      <div class="hero-glow b" aria-hidden="true"></div>

      <div class="page hero-inner">
        <div class="hero-copy">
          <span class="eyebrow"><AppIcon name="inbox" :size="13" /> Viewer request</span>
          <h1 class="display-1">下一部，<br /><em>由你点名。</em></h1>
          <p>
            输入番剧名称，我们会先找出封面和资料。确认是哪一部后提交，
            它就会进入管理员的待处理片墙；入库成功后会归到已处理。
          </p>
          <router-link v-if="isAdmin" to="/requests/manage" class="btn glass admin-link">
            <AppIcon name="layers" :size="16" /> 管理求片
          </router-link>
        </div>

        <div class="hero-stats" aria-label="我的求片与 B 站入库统计">
          <div class="stat">
            <span class="stat-n">{{ String(myPending).padStart(2, '0') }}</span>
            <span>待处理</span>
          </div>
          <i></i>
          <div class="stat">
            <span class="stat-n">{{ String(myDone).padStart(2, '0') }}</span>
            <span>已处理</span>
          </div>
          <i></i>
          <div class="stat" title="当前搜索结果中可直接进入入库流程的 B 站番剧">
            <span class="stat-n bili">{{ String(biliImportableCount).padStart(2, '0') }}</span>
            <span>B 站可入库</span>
          </div>
          <span class="meta">BILI + REQUESTS</span>
        </div>
      </div>

      <div class="page search-deck">
        <div class="search-box" :class="{ busy: loading }">
          <AppIcon name="search" :size="22" />
          <input
            ref="input" v-model="q" type="search"
            aria-label="搜索番剧名称"
            placeholder="输入番剧名称，例如：葬送的芙莉莲"
            autocomplete="off" spellcheck="false"
            @keydown.enter.prevent="onSearchEnter"
          />
          <span v-if="loading" class="searching"><i></i> 正在找</span>
          <button v-else-if="q" class="clear" aria-label="清空搜索" @click="q = ''">
            <AppIcon name="close" :size="17" />
          </button>
          <span v-else class="key">2 字起搜</span>
        </div>
        <div class="search-note">
          <span><AppIcon name="cloud" :size="13" /> B 站片库可直接入库</span>
          <span><AppIcon name="bookmark" :size="13" /> Bangumi 补全封面与资料</span>
          <span><AppIcon name="heart" :size="13" /> 每部待处理只提交一次</span>
        </div>
      </div>
    </section>

    <main class="page request-body">
      <Transition name="rise">
        <section v-if="q.trim()" class="search-results">
          <header class="results-head">
            <div>
              <span class="eyebrow">Search result</span>
              <h2>先确认是哪一部</h2>
            </div>
            <span v-if="searched && !loading" class="count">
              {{ results.length }} 个匹配结果
            </span>
          </header>

          <div v-if="warnings.length" class="warning-line">
            <AppIcon name="alert" :size="14" />
            <span>{{ warnings.join('；') }}</span>
          </div>
          <div v-if="searchError" class="warning-line err">
            <AppIcon name="alert" :size="14" />
            <span>搜索失败：{{ searchError }}</span>
          </div>

          <template v-if="biliHits.length">
            <div class="group-head">
              <span><AppIcon name="cloud" :size="15" /> B 站片库</span>
              <small>
                <b>{{ biliHits.length }}</b> 个结果 ·
                <b>{{ biliImportableCount }}</b> 部可入库
              </small>
            </div>
            <div class="result-grid">
              <button
                v-for="(hit, i) in biliHits" :key="`${hit.source_id}:${hit.season_id}`"
                class="result-card" :class="{ owned: hit.in_library_item_id }"
                :style="{ '--i': Math.min(i, 12) }" @click="pickHit(hit)"
              >
                <span class="result-art">
                  <SmartImage :src="hit.cover" :alt="hit.title" :seed="hit.title" :eager="i < 8" />
                  <span class="provider bili">B 站</span>
                  <span v-if="hit.in_library_item_id" class="owned-tag">
                    <AppIcon name="check" :size="11" /> 已在媒体库
                  </span>
                  <span v-else class="pick"><AppIcon name="plus" :size="14" /></span>
                </span>
                <span class="result-title">{{ hit.title }}</span>
                <span class="result-meta">
                  {{ [hit.year, hit.episodes ? hit.episodes + ' 集' : '', hit.source_name].filter(Boolean).join(' · ') }}
                </span>
              </button>
            </div>
          </template>

          <template v-if="bgmHits.length">
            <div class="group-head">
              <span><AppIcon name="bookmark" :size="15" /> Bangumi 番剧资料</span>
              <small>没有可入库的 B 站条目时，管理员可人工处理</small>
            </div>
            <div class="result-grid">
              <button
                v-for="(hit, i) in bgmHits" :key="`bgm:${hit.bgm_id}`"
                class="result-card" :class="{ owned: hit.in_library_item_id }"
                :style="{ '--i': Math.min(i, 12) }" @click="pickHit(hit)"
              >
                <span class="result-art">
                  <SmartImage :src="hit.cover" :alt="hit.title" :seed="hit.title" :eager="i < 8" />
                  <span class="provider bgm">Bangumi</span>
                  <span v-if="hit.in_library_item_id" class="owned-tag">
                    <AppIcon name="check" :size="11" /> 已在媒体库
                  </span>
                  <span v-else class="pick"><AppIcon name="plus" :size="14" /></span>
                </span>
                <span class="result-title">{{ hit.title }}</span>
                <span class="result-meta">
                  {{ [hit.year, hit.episodes ? hit.episodes + ' 集' : '', hit.rating ? '★ ' + Number(hit.rating).toFixed(1) : ''].filter(Boolean).join(' · ') }}
                </span>
              </button>
            </div>
          </template>

          <div v-if="!loading && searched" class="manual-row">
            <div>
              <strong>{{ results.length ? '都不是它？' : '没有找到？' }}</strong>
              <span>可以只提交片名，管理员会按名字人工查找。</span>
            </div>
            <button class="btn" @click="pickManual">
              提交「{{ q.trim() }}」 <AppIcon name="chevronRight" :size="15" />
            </button>
          </div>

          <div v-if="loading && !results.length" class="search-skeleton">
            <span v-for="i in 6" :key="i" class="sk-card"></span>
          </div>
        </section>
      </Transition>

      <Transition name="rise">
        <section v-if="selected" ref="confirmPanel" class="confirm-card">
          <div class="confirm-art">
            <SmartImage
              :src="selected.cover" :alt="selected.title" :seed="selected.title" eager
            />
            <span class="confirm-provider">{{ selectedProvider }}</span>
          </div>

          <div class="confirm-copy">
            <div class="confirm-top">
              <span class="eyebrow">Confirm request</span>
              <button class="icon-btn" aria-label="取消选择" @click="clearSelection">
                <AppIcon name="close" :size="18" />
              </button>
            </div>
            <h2>{{ selected.title }}</h2>
            <div v-if="selected.original_title && selected.original_title !== selected.title" class="original">
              {{ selected.original_title }}
            </div>
            <div class="chips">
              <span v-if="selectedMeta" class="info-chip">{{ selectedMeta }}</span>
              <span class="info-chip source">{{ providerText(selected.provider) }}</span>
              <router-link
                v-if="selected.in_library_item_id"
                :to="`/item/${selected.in_library_item_id}`" class="info-chip inlib"
              >已在媒体库 · 打开</router-link>
            </div>
            <p v-if="selected.overview" class="overview clamp-3">{{ selected.overview }}</p>
            <p v-else class="overview muted">还没有简介，管理员会按标题进一步确认。</p>

            <label class="note-field">
              <span>补充说明 <small>可选</small></span>
              <textarea
                v-model="note" maxlength="500" rows="3"
                placeholder="例如：想看第二季、希望有中文字幕、或者哪一年播出的那一部…"
              ></textarea>
              <i>{{ note.length }}/500</i>
            </label>

            <div class="confirm-actions">
              <button class="btn ghost" @click="clearSelection">换一部</button>
              <button class="btn primary lg" :disabled="!canSubmit" @click="submitRequest">
                <AppIcon :name="submitting ? 'refresh' : 'send'" :size="16" :class="{ spin: submitting }" />
                {{ submitting ? '正在提交…' : '提交求片' }}
              </button>
            </div>
          </div>
        </section>
      </Transition>

      <section class="my-board">
        <header class="section-head board-head">
          <div>
            <span class="eyebrow">Your board</span>
            <h2 class="section-title">我的求片</h2>
            <p>提交后会在这里显示进度，入库完成时也会自动更新。</p>
          </div>
          <div class="board-counts">
            <span><b>{{ myPending }}</b> 待处理</span>
            <span><b>{{ myDone }}</b> 已处理</span>
          </div>
        </header>

        <div v-if="loadingMine" class="my-grid">
          <span v-for="i in 5" :key="i" class="sk-card"></span>
        </div>
        <div v-else-if="myRequests.length" class="my-grid">
          <RequestPosterCard
            v-for="(req, i) in myRequests" :key="req.id" :request="req" :index="i"
            @open="openMine"
          />
        </div>
        <EmptyState
          v-else icon="inbox" title="还没有提交过求片"
          text="在上面的搜索框里找一部想看的番剧。确认封面与标题后，它就会出现在这里。"
        />
      </section>
    </main>
  </div>
</template>

<style scoped>
.requests-page { min-height: calc(100dvh - var(--header-h)); }
.request-hero {
  position: relative;
  overflow: hidden;
  padding: clamp(42px, 7vw, 96px) 0 0;
  background:
    radial-gradient(70% 90% at 82% 8%, rgba(240, 169, 46, 0.16), transparent 62%),
    radial-gradient(55% 70% at 10% 100%, rgba(53, 111, 181, 0.14), transparent 66%),
    linear-gradient(180deg, #090b10, var(--bg-0));
  border-bottom: 1px solid var(--line-1);
}
.hero-grid {
  position: absolute;
  inset: 0;
  opacity: 0.2;
  background-image:
    linear-gradient(rgba(255, 255, 255, 0.07) 1px, transparent 1px),
    linear-gradient(90deg, rgba(255, 255, 255, 0.07) 1px, transparent 1px);
  background-size: 72px 72px;
  mask-image: linear-gradient(to bottom, #000, transparent 88%);
}
.hero-glow { position: absolute; border-radius: 50%; filter: blur(60px); pointer-events: none; }
.hero-glow.a { width: 360px; height: 360px; right: 5%; top: -180px; background: rgba(240, 169, 46, 0.15); }
.hero-glow.b { width: 260px; height: 260px; left: -80px; bottom: 40px; background: rgba(59, 130, 246, 0.12); }
.hero-inner {
  position: relative;
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: clamp(30px, 7vw, 100px);
  align-items: end;
  padding-top: 0;
  padding-bottom: var(--sp-7);
}
.hero-copy { max-width: 820px; }
.hero-copy .eyebrow { display: inline-flex; align-items: center; gap: 7px; }
.hero-copy h1 { margin: 15px 0 18px; }
.hero-copy h1 em {
  font-style: normal;
  color: var(--brand-2);
  text-shadow: 0 0 50px rgba(240, 169, 46, 0.24);
}
.hero-copy p {
  max-width: 58ch;
  margin: 0;
  color: var(--text-2);
  font-size: var(--fs-15);
  line-height: 1.8;
}
.admin-link { margin-top: var(--sp-5); }
.hero-stats {
  display: flex;
  align-items: flex-end;
  gap: 18px;
  padding: 20px 22px;
  border: 1px solid var(--line-2);
  border-radius: var(--r-l);
  background: rgba(8, 10, 14, 0.58);
  backdrop-filter: blur(20px);
  -webkit-backdrop-filter: blur(20px);
  box-shadow: var(--sh-2);
}
.hero-stats > i { width: 1px; height: 48px; background: var(--line-2); }
.stat { display: flex; flex-direction: column; gap: 2px; min-width: 56px; }
.stat-n {
  font-family: var(--font-display);
  font-size: 30px;
  font-weight: 800;
  line-height: 1;
  letter-spacing: -0.04em;
  color: var(--text-1);
}
.stat-n.bili { color: var(--brand-2); }
.stat span:last-child { color: var(--text-4); font-size: var(--fs-11); }
.meta {
  position: absolute;
  right: 22px;
  top: 18px;
  color: var(--text-4);
  font-family: var(--font-mono);
  font-size: 9px;
  letter-spacing: 0.14em;
}
.hero-stats { position: relative; padding-top: 30px; }

.search-deck {
  position: relative;
  padding-top: 0;
  padding-bottom: clamp(28px, 4vw, 50px);
}
.search-box {
  display: flex;
  align-items: center;
  gap: 14px;
  min-height: 66px;
  padding: 0 18px 0 22px;
  border-radius: var(--r-l);
  border: 1px solid rgba(240, 169, 46, 0.3);
  background: linear-gradient(180deg, rgba(25, 28, 35, 0.96), rgba(13, 15, 20, 0.98));
  box-shadow: 0 24px 70px rgba(0, 0, 0, 0.45), 0 0 0 1px rgba(240, 169, 46, 0.05) inset;
  color: var(--brand);
  transition: border-color var(--t-fast), box-shadow var(--t-fast);
}
.search-box:focus-within {
  border-color: rgba(240, 169, 46, 0.62);
  box-shadow: 0 24px 70px rgba(0, 0, 0, 0.5), 0 0 0 3px rgba(240, 169, 46, 0.08);
}
.search-box input {
  flex: 1;
  min-width: 0;
  border: none;
  outline: none;
  background: none;
  color: var(--text-1);
  font-size: clamp(17px, 1.8vw, 22px);
  font-weight: 600;
  letter-spacing: -0.015em;
}
.search-box input::placeholder { color: var(--text-4); font-weight: 500; }
.search-box input::-webkit-search-cancel-button { display: none; }
.searching {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  color: var(--text-3);
  font-size: var(--fs-12);
}
.searching i {
  width: 12px;
  height: 12px;
  border: 1.5px solid var(--line-3);
  border-top-color: var(--brand);
  border-radius: 50%;
  animation: spin 0.8s linear infinite;
}
.clear {
  display: grid;
  place-items: center;
  width: 34px;
  height: 34px;
  border-radius: 50%;
  color: var(--text-3);
}
.clear:hover { background: rgba(255, 255, 255, 0.07); color: var(--text-1); }
.key {
  padding: 4px 8px;
  border: 1px solid var(--line-2);
  border-radius: var(--r-xs);
  color: var(--text-4);
  font-family: var(--font-mono);
  font-size: 10px;
  white-space: nowrap;
}
.search-note {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px 22px;
  padding: 12px 4px 0;
  color: var(--text-4);
  font-size: var(--fs-11);
}
.search-note span { display: inline-flex; align-items: center; gap: 6px; }

.request-body { padding-top: clamp(28px, 4vw, 54px); }
.search-results { margin-bottom: clamp(34px, 5vw, 64px); }
.results-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 20px;
  margin-bottom: var(--sp-5);
}
.results-head h2 { margin-top: 6px; }
.count { color: var(--text-4); font-size: var(--fs-12); }
.warning-line {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 10px 12px;
  margin: -8px 0 var(--sp-5);
  border: 1px solid rgba(240, 169, 46, 0.2);
  border-radius: var(--r-s);
  background: rgba(240, 169, 46, 0.055);
  color: #d6b77f;
  font-size: var(--fs-12);
  line-height: 1.6;
}
.warning-line.err { border-color: rgba(255, 107, 107, 0.25); background: var(--danger-soft); color: #ffb3b3; }
.group-head {
  display: flex;
  align-items: center;
  gap: 12px;
  margin: var(--sp-5) 0 var(--sp-3);
}
.group-head span {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  color: var(--text-1);
  font-size: var(--fs-13);
  font-weight: 700;
}
.group-head span svg { color: var(--brand); }
.group-head small { color: var(--text-4); font-size: var(--fs-11); font-variant-numeric: tabular-nums; }
.group-head small b { color: var(--text-2); font-weight: 700; }
.result-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(min(100%, 132px), 1fr));
  gap: var(--sp-4) var(--sp-3);
}
.result-card {
  min-width: 0;
  text-align: left;
  animation: cardIn 420ms var(--ease-out) backwards;
  animation-delay: calc(var(--i) * 25ms);
}
.result-art {
  position: relative;
  display: block;
  aspect-ratio: 2 / 3;
  overflow: hidden;
  border: 1px solid var(--line-1);
  border-radius: var(--r-m);
  background: var(--bg-2);
  transition: transform var(--t-fast), border-color var(--t-fast), box-shadow var(--t-fast);
}
.result-art :deep(.smart-img) { position: absolute; inset: 0; }
.result-art :deep(img) { transition: transform var(--t-slow); }
.provider {
  position: absolute;
  left: 8px;
  top: 8px;
  padding: 3px 8px;
  border-radius: var(--r-pill);
  background: rgba(6, 7, 10, 0.8);
  border: 1px solid rgba(255, 255, 255, 0.14);
  color: var(--brand-2);
  font-size: 10px;
  font-weight: 700;
  backdrop-filter: blur(10px);
  -webkit-backdrop-filter: blur(10px);
}
.provider.bgm { color: #a8d6ff; }
.pick {
  position: absolute;
  right: 8px;
  bottom: 8px;
  display: grid;
  place-items: center;
  width: 28px;
  height: 28px;
  border-radius: 50%;
  background: var(--brand);
  color: var(--brand-ink);
  opacity: 0;
  transform: translateY(5px) scale(0.92);
  transition: all var(--t-fast);
}
.result-card:hover .result-art { transform: translateY(-4px); border-color: var(--line-3); box-shadow: var(--sh-3); }
.result-card:hover .result-art :deep(img) { transform: scale(1.055); }
.result-card:hover .pick { opacity: 1; transform: none; }
.result-card:focus-visible .result-art { border-color: var(--brand); box-shadow: 0 0 0 2px var(--brand), var(--sh-3); }
.result-card.owned .result-art { opacity: 0.62; }
.owned-tag {
  position: absolute;
  left: 8px;
  bottom: 8px;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 4px 8px;
  border-radius: var(--r-pill);
  background: rgba(17, 59, 34, 0.84);
  border: 1px solid rgba(74, 222, 128, 0.3);
  color: #a7f0c0;
  font-size: 10px;
  font-weight: 650;
}
.result-title {
  display: block;
  margin-top: 8px;
  overflow: hidden;
  color: var(--text-1);
  font-size: var(--fs-13);
  font-weight: 650;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.result-meta {
  display: block;
  margin-top: 2px;
  overflow: hidden;
  color: var(--text-4);
  font-size: var(--fs-11);
  text-overflow: ellipsis;
  white-space: nowrap;
}
.manual-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 18px;
  margin-top: var(--sp-5);
  padding: 16px 18px;
  border: 1px dashed var(--line-2);
  border-radius: var(--r-m);
  background: rgba(255, 255, 255, 0.018);
}
.manual-row > div { display: flex; flex-direction: column; gap: 2px; }
.manual-row strong { font-size: var(--fs-13); color: var(--text-2); }
.manual-row span { font-size: var(--fs-12); color: var(--text-4); }
.search-skeleton { display: grid; grid-template-columns: repeat(auto-fill, minmax(132px, 1fr)); gap: 16px 12px; }
.sk-card {
  display: block;
  aspect-ratio: 2 / 3;
  border-radius: var(--r-m);
  background: linear-gradient(100deg, var(--bg-2) 20%, var(--bg-3) 38%, var(--bg-2) 56%);
  background-size: 220% 100%;
  animation: shimmer 1.4s linear infinite;
}
.search-skeleton .sk-card { aspect-ratio: 2 / 3; }

.confirm-card {
  display: grid;
  grid-template-columns: minmax(160px, 210px) minmax(0, 1fr);
  gap: clamp(22px, 4vw, 48px);
  align-items: center;
  margin-bottom: clamp(40px, 6vw, 78px);
  padding: clamp(18px, 3vw, 30px);
  border-radius: var(--r-xl);
  border: 1px solid rgba(240, 169, 46, 0.24);
  background:
    radial-gradient(80% 100% at 12% 0%, rgba(240, 169, 46, 0.08), transparent 60%),
    linear-gradient(135deg, rgba(20, 23, 29, 0.98), rgba(10, 12, 16, 0.98));
  box-shadow: var(--sh-3);
}
.confirm-art {
  position: relative;
  aspect-ratio: 2 / 3;
  overflow: hidden;
  border-radius: var(--r-l);
  border: 1px solid var(--line-2);
  box-shadow: var(--sh-3);
}
.confirm-art :deep(.smart-img) { position: absolute; inset: 0; }
.confirm-provider {
  position: absolute;
  left: 10px;
  top: 10px;
  padding: 4px 9px;
  border-radius: var(--r-pill);
  background: rgba(6, 7, 10, 0.8);
  border: 1px solid rgba(255, 255, 255, 0.15);
  color: var(--brand-2);
  font-size: 10px;
  font-weight: 700;
  backdrop-filter: blur(10px);
  -webkit-backdrop-filter: blur(10px);
}
.confirm-copy { min-width: 0; }
.confirm-top { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.confirm-copy h2 { margin: 9px 0 4px; font-size: clamp(24px, 3vw, 36px); letter-spacing: -0.03em; }
.original { color: var(--text-3); font-size: var(--fs-13); }
.chips { display: flex; flex-wrap: wrap; gap: 7px; margin: 15px 0; }
.info-chip {
  display: inline-flex;
  align-items: center;
  height: 27px;
  padding: 0 10px;
  border: 1px solid var(--line-2);
  border-radius: var(--r-pill);
  color: var(--text-2);
  font-size: var(--fs-11);
  font-weight: 600;
}
.info-chip.source { color: var(--brand-2); border-color: rgba(240, 169, 46, 0.28); }
.info-chip.inlib { color: #a7f0c0; border-color: rgba(74, 222, 128, 0.3); }
.overview { max-width: 72ch; margin: 0 0 18px; color: var(--text-2); line-height: 1.75; font-size: var(--fs-13); }
.note-field { position: relative; display: block; margin-bottom: 18px; }
.note-field > span { display: flex; align-items: center; gap: 6px; margin-bottom: 7px; font-size: var(--fs-12); font-weight: 650; color: var(--text-2); }
.note-field small { color: var(--text-4); font-weight: 500; }
.note-field textarea {
  width: 100%;
  resize: vertical;
  min-height: 82px;
  padding: 11px 13px 22px;
  border: 1px solid var(--line-2);
  border-radius: var(--r-m);
  outline: none;
  background: rgba(5, 6, 9, 0.5);
  color: var(--text-1);
  font-size: var(--fs-13);
  line-height: 1.6;
  transition: border-color var(--t-fast);
}
.note-field textarea:focus { border-color: rgba(240, 169, 46, 0.62); }
.note-field textarea::placeholder { color: var(--text-4); }
.note-field i { position: absolute; right: 10px; bottom: 7px; color: var(--text-4); font-style: normal; font-size: 10px; }
.confirm-actions { display: flex; align-items: center; justify-content: flex-end; gap: 10px; flex-wrap: wrap; }
.spin { animation: spin 0.9s linear infinite; }

.my-board { padding-top: clamp(20px, 3vw, 36px); border-top: 1px solid var(--line-1); }
.board-head { align-items: flex-start; margin-bottom: var(--sp-5); }
.board-head h2 { margin: 4px 0 5px; }
.board-head p { margin: 0; color: var(--text-4); font-size: var(--fs-12); }
.board-counts { display: flex; gap: 8px; flex-wrap: wrap; }
.board-counts span {
  padding: 7px 11px;
  border: 1px solid var(--line-1);
  border-radius: var(--r-pill);
  color: var(--text-3);
  font-size: var(--fs-11);
  background: var(--bg-1);
}
.board-counts b { color: var(--text-1); font-variant-numeric: tabular-nums; }
.my-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(min(100%, 158px), 1fr));
  gap: var(--sp-5) var(--sp-4);
}
.my-grid .sk-card { aspect-ratio: 2 / 3; }

@keyframes shimmer { to { background-position: -120% 0; } }
@keyframes spin { to { transform: rotate(360deg); } }

@media (max-width: 880px) {
  .hero-inner { grid-template-columns: 1fr; align-items: start; }
  .hero-stats { justify-self: start; }
}
@media (max-width: 720px) {
  .request-hero { padding-top: 34px; }
  .hero-inner { gap: 26px; padding-bottom: 28px; }
  .hero-copy h1 { margin-top: 11px; }
  .hero-copy p { font-size: var(--fs-13); }
  .hero-stats { padding: 14px 16px; gap: 12px; }
  .stat-n { font-size: 24px; }
  .meta { right: 16px; top: 12px; }
  .search-box { min-height: 58px; padding: 0 13px 0 16px; gap: 10px; }
  .search-box input { font-size: var(--fs-16); }
  .key, .searching { display: none; }
  .search-note { gap: 8px 14px; }
  .search-note span:last-child { display: none; }
  .results-head { align-items: flex-start; }
  .count { padding-top: 18px; }
  .result-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 14px 9px; }
  .search-skeleton { grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 14px 9px; }
  .result-card .result-title { font-size: var(--fs-12); }
  .result-card .result-meta { font-size: 10px; }
  .owned-tag { font-size: 0; width: 24px; height: 24px; justify-content: center; padding: 0; }
  .manual-row { align-items: flex-start; flex-direction: column; }
  .manual-row .btn { width: 100%; }
  .confirm-card { grid-template-columns: 112px minmax(0, 1fr); align-items: start; padding: 14px; gap: 14px; }
  .confirm-copy h2 { font-size: var(--fs-20); }
  .confirm-copy .overview { display: none; }
  .confirm-top .eyebrow { font-size: 9px; }
  .confirm-art { position: sticky; top: calc(var(--header-h) + 12px); }
  .chips { margin: 10px 0; }
  .confirm-actions { justify-content: stretch; }
  .confirm-actions .btn { flex: 1; }
  .board-head { flex-direction: column; }
  .board-counts { margin-top: 4px; }
  .my-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 18px 9px; }
}
@media (max-width: 420px) {
  .confirm-card { grid-template-columns: 92px minmax(0, 1fr); }
  .note-field textarea { min-height: 76px; }
}
</style>
