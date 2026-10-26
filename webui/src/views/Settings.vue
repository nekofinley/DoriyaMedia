<script setup>
import { ref, onMounted, onBeforeUnmount, computed, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api, fmtDate, fmtRelative } from '../api.js'
import { me } from '../auth.js'
import AppIcon from '../components/AppIcon.vue'
import EmptyState from '../components/EmptyState.vue'
import ExcludeKeywords from '../components/ExcludeKeywords.vue'
import LiveProgress from '../components/LiveProgress.vue'
import { toast } from '../toast.js'
import { site, setToolbar, panelTab, publishedLabel, dirty } from '../site/store.js'

const router = useRouter()

const tabs = [
  { id: 'sources', label: '存储源', icon: 'cloud', desc: 'OpenList / WebDAV / 本地目录' },
  { id: 'libraries', label: '媒体库', icon: 'film', desc: '路径、类型与扫描' },
  { id: 'playback', label: '刮削与播放', icon: 'sparkles', desc: 'TMDB、Bangumi 与播放决策' },
  { id: 'users', label: '用户', icon: 'users', desc: '账号、角色与注册策略' },
  { id: 'mail', label: '邮件与验证', icon: 'link', desc: 'SMTP、邮箱验证与 Cloudflare 人机验证' },
  { id: 'site', label: '站点外观', icon: 'wand', desc: '登录页文案、布局与页脚' },
  { id: 'system', label: '系统', icon: 'server', desc: '工具路径与运行状态' },
]

// Open the editing toolbar, optionally somewhere else and on a given panel.
function openEditor(where, which) {
  setToolbar(true)
  panelTab.value = which
  if (where) router.push(where)
}

const tab = ref(localStorage.getItem('dm:settings-tab') || 'sources')
watch(tab, (v) => localStorage.setItem('dm:settings-tab', v))

const sources = ref([])
const libraries = ref([])
const scanStates = ref([])
const sys = ref(null)
const cfg = ref(null)
const error = ref('')

let pollTimer = null

// new source form
const srcType = ref('openlist')
const srcForm = ref({ name: '', root: '', webdav_url: '', webdav_user: '', webdav_pass: '', base_url: '', token: '', bili_cookie: '', bili_codec: 'avc', bili_proxy: '', bili_playable_only: true })
const testResult = ref(null)
const testing = ref(false)
const addingSource = ref(false)

// The source being edited, or null when the form is adding one. Editing exists
// for the fields that were added after a source was created — the bilibili
// proxy is exactly that: an install with 番剧 already configured would
// otherwise have to delete the source (and the 媒体库 rows pointing at it) to
// gain a proxy.
const editingSource = ref(null)

// The bilibili proxy is checked on its own, before the source exists: the whole
// point of the field is the region question, and an answer to it is worth
// having before anything is saved.
const proxyTesting = ref(false)
const proxyResult = ref(null)

function blankSrcForm() {
  return { name: '', root: '', webdav_url: '', webdav_user: '', webdav_pass: '', base_url: '', token: '', bili_cookie: '', bili_codec: 'avc', bili_proxy: '', bili_playable_only: true }
}

/** Load one source row into the form, so it can be changed in place. */
function startEditSource(s) {
  const c = s.config || {}
  srcType.value = s.type
  editingSource.value = s.id
  testResult.value = null
  proxyResult.value = null
  srcForm.value = {
    ...blankSrcForm(),
    name: s.name || '',
    root: c.root || '',
    webdav_url: c.webdav_url || '',
    webdav_user: c.webdav_user || '',
    webdav_pass: c.webdav_pass === '******' ? '' : (c.webdav_pass || ''),
    base_url: c.base_url || '',
    token: c.token === '******' ? '' : (c.token || ''),
    bili_cookie: c.bili_cookie === '******' ? '' : (c.bili_cookie || ''),
    bili_codec: c.bili_codec || 'avc',
    bili_proxy: c.bili_proxy && c.bili_proxy !== '******' ? c.bili_proxy : '',
    // A source from before this option existed has no field: keep its old
    // behaviour (everything listed) until an administrator ticks the box.
    bili_playable_only: !!c.bili_playable_only,
  }
  addingSource.value = true
}

function cancelEditSource() {
  editingSource.value = null
  srcForm.value = blankSrcForm()
  testResult.value = null
  proxyResult.value = null
  addingSource.value = false
}

async function testBiliProxy() {
  proxyTesting.value = true
  proxyResult.value = null
  try {
    proxyResult.value = await api.post('/api/sources/test-proxy', { proxy: srcForm.value.bili_proxy || '' })
  } catch (e) {
    proxyResult.value = { ok: false, error: e.message, message: e.message }
  } finally {
    proxyTesting.value = false
  }
}

// new library form
const libForm = ref({
  name: '', type: 'movie',
  // A library may scan several places: one show can be split across two drives,
  // and a 番剧 library is often a cloud source plus a folder on this machine.
  sources: [{ source_id: '', path: '/' }],
  providers: ['nfo', 'bgm', 'tmdb'],
  // What this library refuses to import. A new library follows the global list
  // until someone gives it its own.
  excludeMode: 'inherit',
  excludes: [],
})

// A library's exclusion list is stored as json text; a hand-written row may be
// plain text instead, and either way the form needs a list.
function parseKeywords(raw) {
  if (!raw) return []
  try {
    const list = JSON.parse(raw)
    if (Array.isArray(list)) return list.filter((w) => String(w).trim())
  } catch {}
  return String(raw).split(/[\n,，]/).map((w) => w.trim()).filter(Boolean)
}

function addLibSource() {
  const first = libForm.value.sources[0]
  libForm.value.sources.push({ source_id: first?.source_id || '', path: '/' })
}

function removeLibSource(i) {
  if (libForm.value.sources.length > 1) libForm.value.sources.splice(i, 1)
}
const addingLibrary = ref(false)

const providerLabels = { nfo: '本地 nfo', tmdb: 'TMDB', bgm: 'Bangumi' }
const sourceTypes = [
  { id: 'openlist', label: 'OpenList / AList', icon: 'cloud', hint: '可 302 直连,最省带宽' },
  { id: 'webdav', label: 'WebDAV', icon: 'globe', hint: '服务器代理播放,支持拖动' },
  { id: 'local', label: '本地目录', icon: 'hdd', hint: '服务器磁盘上的绝对路径' },
  { id: 'bilibili', label: 'B站番剧', icon: 'tv', hint: '正版番剧,服务端合流 + 弹幕' },
]

function buildSourceConfig() {
  const f = srcForm.value
  if (srcType.value === 'local') return { root: f.root }
  if (srcType.value === 'webdav') return { webdav_url: f.webdav_url, webdav_user: f.webdav_user, webdav_pass: f.webdav_pass }
  if (srcType.value === 'bilibili') {
    return {
      bili_cookie: f.bili_cookie,
      bili_codec: f.bili_codec || 'avc', bili_proxy: (f.bili_proxy || '').trim(),
      bili_playable_only: !!f.bili_playable_only,
    }
  }
  return { base_url: f.base_url, token: f.token }
}

async function loadAll() {
  try {
    sources.value = (await api.get('/api/sources')) || []
    libraries.value = (await api.get('/api/libraries')) || []
    scanStates.value = (await api.get('/api/scan/status')) || []
    sys.value = await api.get('/api/system/info')
    cfg.value = await api.get('/api/config')
    users.value = (await api.get('/api/admin/users')).users || []
    if (!libForm.value.sources[0].source_id && sources.value.length) {
      libForm.value.sources[0].source_id = sources.value[0].id
    }
  } catch (e) {
    error.value = e.message
  }
}

const editing = ref(null)
const editForm = ref({})

function startEdit(lib) {
  editing.value = lib.id
  let provs = []
  try { provs = JSON.parse(lib.providers || '[]') } catch {}
  if (!provs.length) provs = ['nfo', 'bgm', 'tmdb']
  // Older libraries carry a single source_id/path; newer ones a list.
  const places = (lib.sources && lib.sources.length)
    ? lib.sources.map((s) => ({ source_id: s.source_id, path: s.path }))
    : [{ source_id: lib.source_id, path: lib.path }]
  editForm.value = {
    name: lib.name, type: lib.type, sources: places, providers: provs,
    excludeMode: lib.exclude_mode === 'custom' ? 'custom' : 'inherit',
    excludes: parseKeywords(lib.excludes),
    poster_mode: lib.poster_mode === 'static' ? 'static' : 'auto',
  }
  posterMissing.value = false
  posterVersion.value++
}

// ---------- library cover ----------
// The cover is either dynamic (follows the newest item with cached artwork) or a
// fixed image. Picking or auto-fetching a file applies immediately — the two
// buttons sit outside the 保存 flow on purpose, so the stored file and the
// 动态/静态 choice cannot drift apart.
const posterVersion = ref(0)
const posterBusy = ref(false)
const posterMissing = ref(false)

function posterSrc(id) { return `/api/libraries/${id}/poster?v=${posterVersion.value}` }

async function applyLibraryPoster(id, call, okText) {
  posterBusy.value = true
  try {
    await call()
    editForm.value.poster_mode = 'static'
    posterMissing.value = false
    posterVersion.value++
    await loadAll()
    toast.ok(okText)
  } catch (e) { toast.err(e.message) } finally { posterBusy.value = false }
}

function pickLibraryPoster(id, event) {
  const file = event.target.files && event.target.files[0]
  event.target.value = ''
  if (!file) return
  applyLibraryPoster(id, () => {
    const fd = new FormData()
    fd.append('file', file)
    return api.upload(`/api/libraries/${id}/poster`, fd)
  }, '封面已保存')
}

function autoLibraryPoster(id) {
  applyLibraryPoster(id, () => api.post(`/api/libraries/${id}/poster/auto`), '已取该库最新入库条目的海报')
}

async function clearLibraryPoster(id) {
  posterBusy.value = true
  try {
    await api.del(`/api/libraries/${id}/poster`)
    posterVersion.value++
    posterMissing.value = false
    await loadAll()
    toast.ok('已恢复动态封面')
  } catch (e) { toast.err(e.message) } finally { posterBusy.value = false }
}
function toggleProvider(p) {
  const a = editForm.value.providers
  const i = a.indexOf(p)
  i >= 0 ? a.splice(i, 1) : a.push(p)
}
function addEditSource() {
  const first = editForm.value.sources[0]
  editForm.value.sources.push({ source_id: first?.source_id || '', path: '/' })
}
function removeEditSource(i) {
  if (editForm.value.sources.length > 1) editForm.value.sources.splice(i, 1)
}
async function saveEdit(id) {
  try {
    const f = editForm.value
    const picked = f.sources
      .filter((s) => s.source_id)
      .map((s) => ({ source_id: Number(s.source_id), path: s.path || '/' }))
    if (!picked.length) { toast.err('至少要选一个存储源'); return }
    await api.put(`/api/libraries/${id}`, {
      name: f.name, type: f.type,
      source_id: picked[0].source_id, path: picked[0].path,
      sources: picked,
      poster_mode: f.poster_mode === 'static' ? 'static' : 'auto',
      providers: JSON.stringify(f.providers.length ? f.providers : ['nfo', 'bgm', 'tmdb']),
      exclude_mode: f.excludeMode === 'custom' ? 'custom' : 'inherit',
      excludes: JSON.stringify(f.excludes || []),
    })
    editing.value = null
    await loadAll()
    toast.ok('媒体库已更新,改过源或路径记得重新扫描')
  } catch (e) { toast.err(e.message) }
}

async function stopScan(id) {
  try { await api.post(`/api/libraries/${id}/scan/stop`); toast.info('已请求停止扫描') } catch (e) { toast.err(e.message) }
}

function stFor(id) { return (scanStates.value || []).find((s) => s.library_id === id) }
/** One line per source card: what it points at, and for 番剧 whether a proxy is set. */
function sourceLine(s) {
  if (s.type === 'bilibili') {
    const account = s.config.bili_cookie ? '已配置账号' : '未登录(仅免费集 / 480P)'
    const proxy = s.config.bili_proxy ? `经代理 ${maskProxy(s.config.bili_proxy)}` : '直连(海外 IP 看番剧会被 -10403 拦)'
    const only = s.config.bili_playable_only ? ' · 只上架当前线路能播的番剧' : ''
    return `${account} · ${proxy}${only}`
  }
  return s.config.base_url || s.config.webdav_url || s.config.root || '-'
}

// ---------- B 站账号状态 ----------
//
// A pasted cookie is a copy of a browser session, and bilibili rotates those from
// the browser: the copy dies at the first rotation and nothing says so. What the
// viewer then sees is 480P and "this needs a 大会员" on an account that has one.
// The health line below is the answer to "why is my picture bad", and the QR
// dialog is the fix that does not expire.

const biliHealth = ref({})      // source id → BiliHealth
const biliHealthLoading = ref(0) // source id being checked, 0 = none

async function loadBiliHealth(s) {
  if (!s || s.type !== 'bilibili') return
  biliHealthLoading.value = s.id
  try {
    biliHealth.value = { ...biliHealth.value, [s.id]: await api.get(`/api/sources/${s.id}/bili-account`) }
  } catch (e) {
    biliHealth.value = {
      ...biliHealth.value,
      [s.id]: { configured: !!s.config?.bili_cookie, logged_in: false, message: e.message },
    }
  } finally {
    biliHealthLoading.value = 0
  }
}

/** The tone of a health line: good, warning, or unknown (not checked yet). */
function biliTone(h) {
  if (!h) return 'dim'
  if (h.error) return 'err'
  if (!h.configured) return 'dim'
  return h.logged_in ? 'ok' : 'err'
}

const qr = ref(null)        // { source, key, png, url, state, message }
const qrStarting = ref(false)
let qrTimer = null

function stopQR() {
  if (qrTimer) { clearInterval(qrTimer); qrTimer = null }
}

async function startQRLogin(s) {
  stopQR()
  qrStarting.value = true
  qr.value = { source: s, state: 'loading', message: '正在申请二维码…' }
  try {
    const out = await api.post(`/api/sources/${s.id}/bili-qr`)
    qr.value = { source: s, key: out.key, png: out.png, url: out.url, state: 'waiting', message: '请用 B 站手机客户端扫码' }
    // Poll every two seconds: fast enough to feel immediate, slow enough that a
    // forgotten dialog is not a request per second against bilibili.
    qrTimer = setInterval(() => pollQR(s), 2000)
  } catch (e) {
    qr.value = { source: s, state: 'error', message: e.message }
  } finally {
    qrStarting.value = false
  }
}

async function pollQR(s) {
  const cur = qr.value
  if (!cur || !cur.key) return
  try {
    const st = await api.post(`/api/sources/${s.id}/bili-qr/poll`, { key: cur.key })
    qr.value = { ...cur, state: st.state, message: st.message, account: st.account }
    if (st.state === 'confirmed') {
      stopQR()
      await loadAll()
      await loadBiliHealth(s)
      toast.ok(st.message || '登录成功')
    } else if (st.state === 'expired') {
      stopQR()
    }
  } catch (e) {
    // A transient failure (a proxy hiccup) must not kill the dialog; the next
    // tick tries again. Only a dead QR ends it.
    qr.value = { ...cur, message: e.message }
  }
}

function closeQR() {
  stopQR()
  qr.value = null
}

async function refreshBiliSession(s) {
  try {
    biliHealth.value = { ...biliHealth.value, [s.id]: await api.post(`/api/sources/${s.id}/bili-refresh`) }
    await loadAll()
    toast.ok('会话已续期')
  } catch (e) { toast.err(e.message) }
}

onBeforeUnmount(stopQR)
function fmtClock(ts) {
  if (!ts) return ''
  return new Date(ts * 1000).toLocaleTimeString('zh-CN', { hour12: false })
}
function sourceName(id) {
  return sources.value.find((s) => s.id === id)?.name || `源 #${id}`
}

/**
 * Hide the secret inside a proxy string for display. An administrator does get
 * the real value (the edit form has to round-trip it), but there is no reason
 * for a Shadowsocks password — which is the key itself — to sit on the settings
 * page in plain sight: a screenshot or a shoulder is enough to leak it.
 */
function maskProxy(proxy) {
  const s = String(proxy || '')
  if (!s) return ''
  // ss://METHOD:PASSWORD@host:port
  if (s.startsWith('ss://')) {
    const m = s.match(/^ss:\/\/([^:@/]+):([^@]*)@(.*)$/)
    if (m) return `ss://${m[1]}:******@${m[3]}`
    return 'ss://******'
  }
  // scheme://user:password@host:port
  const u = s.match(/^([a-z0-9+.-]+):\/\/([^:@/]+):([^@]*)@(.*)$/i)
  if (u) return `${u[1]}://${u[2]}:******@${u[4]}`
  return s
}
/** One library may scan several places; the card lists them all. */
function libSourcesLabel(lib) {
  const places = (lib.sources && lib.sources.length)
    ? lib.sources
    : [{ source_id: lib.source_id, path: lib.path }]
  return places.map((p) => `${sourceName(p.source_id)} ${p.path || '/'}`).join(' + ')
}
function providersOf(lib) {
  try {
    const p = JSON.parse(lib.providers || '[]')
    return p.length ? p : ['nfo', 'bgm', 'tmdb']
  } catch { return [] }
}

onMounted(async () => {
  await loadAll()
  await loadAudits()
  // The account state is the first thing that explains a bad picture, so it is
  // read for every bilibili source as soon as the page opens — not only when an
  // administrator opens the edit form.
  for (const s of sources.value) {
    if (s.type === 'bilibili') await loadBiliHealth(s)
  }
  pollTimer = setInterval(async () => {
    try { scanStates.value = (await api.get('/api/scan/status')) || [] } catch {}
    // 体检是长时间后台任务(要逐个探测网盘文件),轮询比扫描稍慢即可。
    if (audits.value.some((a) => a.state.running)) await loadAudits()
  }, 2000)
})
onBeforeUnmount(() => clearInterval(pollTimer))

async function addSource() {
  try {
    const id = editingSource.value
    if (id) {
      await api.put(`/api/sources/${id}`, {
        name: srcForm.value.name || '未命名源', config: buildSourceConfig(),
      })
    } else {
      await api.post('/api/sources', {
        name: srcForm.value.name || '未命名源', type: srcType.value, config: buildSourceConfig(),
      })
    }
    editingSource.value = null
    srcForm.value = blankSrcForm()
    proxyResult.value = null
    testResult.value = null
    addingSource.value = false
    await loadAll()
    toast.ok(id ? '存储源已更新' : '存储源已添加')
  } catch (e) { toast.err(e.message) }
}

async function testSource() {
  testing.value = true
  testResult.value = null
  try {
    testResult.value = await api.post('/api/sources/test', { type: srcType.value, config: buildSourceConfig() })
  } catch (e) {
    testResult.value = { ok: false, error: e.message }
  } finally {
    testing.value = false
  }
}

// TMDB 连通性测试
const tmdbTesting = ref(false)
const tmdbTest = ref(null)

async function testTMDB() {
  tmdbTesting.value = true
  tmdbTest.value = null
  try {
    tmdbTest.value = await api.post('/api/metadata/test', {
      api_key: cfg.value.tmdb.api_key,
      api_base: cfg.value.tmdb.api_base,
      img_base: cfg.value.tmdb.img_base,
      use_proxy: !!cfg.value.tmdb.use_proxy,
      proxy: cfg.value.tmdb.proxy,
    })
  } catch (e) {
    tmdbTest.value = { ok: false, message: e.message }
  } finally {
    tmdbTesting.value = false
  }
}

// Bangumi 连通性测试
const bgmTesting = ref(false)
const bgmTest = ref(null)
const bgmKeyword = ref('孤独摇滚')

async function testBGM() {
  bgmTesting.value = true
  bgmTest.value = null
  try {
    bgmTest.value = await api.post('/api/metadata/test', {
      provider: 'bgm',
      token: cfg.value.bgm.token,
      api_base: cfg.value.bgm.api_base,
      use_proxy: !!cfg.value.bgm.use_proxy,
      proxy: cfg.value.bgm.proxy,
      keyword: bgmKeyword.value,
    })
  } catch (e) {
    bgmTest.value = { ok: false, message: e.message }
  } finally {
    bgmTesting.value = false
  }
}

async function delSource(id) {
  if (!confirm('删除该存储源?其下媒体库也会被删除。')) return
  try { await api.del(`/api/sources/${id}`); await loadAll(); toast.ok('已删除') } catch (e) { toast.err(e.message) }
}

async function addLibrary() {
  try {
    const f = libForm.value
    const picked = f.sources
      .filter((s) => s.source_id)
      .map((s) => ({ source_id: Number(s.source_id), path: s.path || '/' }))
    if (!picked.length) { toast.err('至少要选一个存储源'); return }
    await api.post('/api/libraries', {
      name: f.name, type: f.type,
      // source_id/path keep naming the first place, for anything that only knows
      // about one source; the list is what the scan actually walks.
      source_id: picked[0].source_id, path: picked[0].path,
      sources: picked,
      providers: JSON.stringify(f.providers.length ? f.providers : ['nfo', 'bgm', 'tmdb']),
      exclude_mode: f.excludeMode === 'custom' ? 'custom' : 'inherit',
      excludes: JSON.stringify(f.excludes || []),
    })
    libForm.value = {
      name: '', type: 'movie',
      sources: [{ source_id: f.sources[0]?.source_id || '', path: '/' }],
      providers: ['nfo', 'bgm', 'tmdb'],
      excludeMode: 'inherit',
      excludes: [],
    }
    addingLibrary.value = false
    await loadAll()
    toast.ok('媒体库已创建,记得点扫描')
  } catch (e) { toast.err(e.message) }
}

async function scanLibrary(id) {
  try {
    await api.post(`/api/libraries/${id}/scan`)
    toast.info('扫描已开始(含刮削,后台进行)')
  } catch (e) { toast.err(e.message) }
}

// ---------- 字幕体检 ----------
// BDRip 才有意义:WebRip 的片子通常把字幕烧进画面了,没有文件也没有轨道可查,
// 报成"缺字幕"多半是错的。所以这一页只看 bdrip。
const audits = ref([])
const auditOpen = ref({}) // libraryID -> 结果面板是否展开

function auditFor(id) { return audits.value.find((a) => a.state.library_id === id) }

async function loadAudits() {
  try {
    audits.value = (await api.get('/api/subtitle-audit')).audits || []
  } catch {}
}

async function startAudit(id) {
  try {
    await api.post(`/api/libraries/${id}/subtitle-audit`)
    auditOpen.value[id] = true
    await loadAudits()
    toast.info('开始体检:先列出缺少外挂字幕的剧集,再后台逐个确认内嵌字幕')
  } catch (e) { toast.err(e.message) }
}

async function stopAudit(id) {
  try {
    await api.post(`/api/libraries/${id}/subtitle-audit/stop`)
    await loadAudits()
    toast.info('已停止,当前结果保留')
  } catch (e) { toast.err(e.message) }
}

// 结果按剧集聚合:一眼看出是哪几部番需要补字幕。
function auditGroups(items) {
  const byShow = new Map()
  for (const it of items) {
    const key = it.show_id || it.season_id || it.item_id
    if (!byShow.has(key)) {
      byShow.set(key, { key, show: it.show || '未知剧集', showId: it.show_id, seasons: new Map() })
    }
    const g = byShow.get(key)
    const sk = it.season_id || 0
    if (!g.seasons.has(sk)) g.seasons.set(sk, { seasonId: it.season_id, season: it.season, episodes: [] })
    g.seasons.get(sk).episodes.push(it)
  }
  const out = [...byShow.values()]
  for (const g of out) {
    g.seasons = [...g.seasons.values()].sort((a, b) => a.season - b.season)
    for (const s of g.seasons) s.episodes.sort((a, b) => a.episode - b.episode || a.path.localeCompare(b.path))
    g.count = g.seasons.reduce((n, s) => n + s.episodes.length, 0)
    g.checking = g.seasons.reduce((n, s) => n + s.episodes.filter((e) => e.checking).length, 0)
  }
  return out.sort((a, b) => b.count - a.count || a.show.localeCompare(b.show))
}
function auditEpisodeLabel(e) {
  if (!e.episode) return '特别篇'
  return `第 ${e.episode} 集`
}

async function delLibrary(id) {
  if (!confirm('删除该媒体库及其所有条目?')) return
  try { await api.del(`/api/libraries/${id}`); await loadAll(); toast.ok('已删除') } catch (e) { toast.err(e.message) }
}

async function saveConfig() {
  try {
    await api.put('/api/config', {
      ffmpeg: cfg.value.ffmpeg,
      ffprobe: cfg.value.ffprobe,
      scan_concurrency: Number(cfg.value.scan_concurrency),
      // Sent even when empty: an empty list is how the user turns an exclusion
      // off, and the server writes exactly what it is given.
      exclude_keywords: (cfg.value.exclude_keywords || []).map((w) => String(w).trim()).filter(Boolean),
      tmdb: cfg.value.tmdb,
      bgm: cfg.value.bgm,
      playback: cfg.value.playback,
      users: cfg.value.users,
      smtp: cfg.value.smtp,
      turnstile: cfg.value.turnstile,
    })
    toast.ok('配置已保存,立即生效')
    await loadAll()
  } catch (e) { toast.err(e.message) }
}

// ============================ users ======================================

const users = ref([])
const newUser = ref({ name: '', password: '', role: 'user' })
const addingUser = ref(false)
const pwTarget = ref(null) // 正在重置密码的用户
const pwValue = ref('')

// Signup policy. "open" is the default the server ships with; the other two are
// how an owner takes the welcome mat back in.
const regModes = [
  { id: 'open', label: '开放注册', hint: '任何人注册后立即可用' },
  { id: 'approval', label: '需要审核', hint: '注册后待管理员通过' },
  { id: 'closed', label: '关闭注册', hint: '只能由管理员创建账号' },
]

const pendingUsers = computed(() => users.value.filter((u) => u.status === 'pending'))
const adminCount = computed(() => users.value.filter((u) => u.role === 'admin' && u.status === 'active').length)

// The account you are signed in as cannot be disabled or deleted from here.
function isSelf(u) { return me.value && u.id === me.value.id }
// The server refuses to leave itself without an admin; mirror that so the
// buttons explain themselves instead of failing.
function lastAdmin(u) { return u.role === 'admin' && u.status === 'active' && adminCount.value <= 1 }

async function loadUsers() {
  try {
    users.value = (await api.get('/api/admin/users')).users || []
  } catch (e) {
    error.value = e.message
  }
}

async function createUser() {
  try {
    await api.post('/api/admin/users', {
      name: newUser.value.name,
      password: newUser.value.password,
      role: newUser.value.role,
    })
    newUser.value = { name: '', password: '', role: 'user' }
    addingUser.value = false
    await loadUsers()
    toast.ok('用户已创建')
  } catch (e) { toast.err(e.message) }
}

async function patchUser(u, patch, okMsg = '已更新') {
  try {
    await api.put(`/api/admin/users/${u.id}`, patch)
    await loadUsers()
    toast.ok(okMsg)
  } catch (e) { toast.err(e.message) }
}

async function deleteUser(u) {
  if (!confirm(`删除用户「${u.name}」?该账号的播放记录会一并删除,且无法恢复。`)) return
  try {
    await api.del(`/api/admin/users/${u.id}`)
    await loadUsers()
    toast.ok('用户已删除')
  } catch (e) { toast.err(e.message) }
}

async function savePassword() {
  const u = pwTarget.value
  if (!u) return
  try {
    await api.post(`/api/admin/users/${u.id}/password`, { password: pwValue.value })
    pwTarget.value = null
    pwValue.value = ''
    await loadUsers()
    toast.ok(`已重置「${u.name}」的密码,该账号其他设备已下线`)
  } catch (e) { toast.err(e.message) }
}

async function revokeSessions(u) {
  if (!confirm(`让「${u.name}」的所有设备退出登录?`)) return
  try {
    await api.post(`/api/admin/users/${u.id}/revoke`)
    toast.ok('已下线该账号的所有设备')
  } catch (e) { toast.err(e.message) }
}

async function saveRegistration() {
  try {
    await api.put('/api/config', { users: cfg.value.users })
    const m = regModes.find((x) => x.id === cfg.value.users.registration)
    toast.ok(`注册方式已改为「${m ? m.label : cfg.value.users.registration}」`)
    await loadUsers()
  } catch (e) {
    toast.err(e.message)
    await loadAll() // 拉回服务端的真实值,别让界面停在一个没保存成功的选项上
  }
}

function statusLabel(u) {
  if (u.status === 'pending') return '待审核'
  if (u.status === 'disabled') return '已禁用'
  return '正常'
}

// ---------- 邮箱：管理员代填与代为验证 ----------

async function setUserEmail(u) {
  const email = prompt(`为「${u.name}」填写邮箱地址(留空表示清除):`, u.email || '')
  if (email === null) return
  await patchUser(u, { email }, email.trim() ? '邮箱已保存,可点「标记已验证」直接放行' : '邮箱已清除')
}

async function verifyUserEmail(u, verified) {
  await patchUser(u, { email_verified: verified }, verified ? `已把「${u.name}」的邮箱标记为已验证` : `已取消「${u.name}」的邮箱验证`)
}

// ============================ 邮件与人机验证 ============================

const mailTesting = ref(false)
const mailResult = ref(null)
const mailTestTo = ref('')

const encryptions = [
  { id: 'starttls', label: 'STARTTLS', hint: '默认 587 端口,先明文连接再升级加密' },
  { id: 'tls', label: '隐式 TLS', hint: '默认 465 端口,从第一字节就加密(QQ/163 常用)' },
  { id: 'none', label: '不加密', hint: '仅本机中继,或服务器只监听 25 端口时' },
]

// 常见邮箱的服务器地址,省得去查;密码栏多数要填「授权码」而不是登录密码。
const presets = [
  { id: 'qq', label: 'QQ 邮箱', host: 'smtp.qq.com', port: 465, encryption: 'tls' },
  { id: '163', label: '163 邮箱', host: 'smtp.163.com', port: 465, encryption: 'tls' },
  { id: 'gmail', label: 'Gmail', host: 'smtp.gmail.com', port: 587, encryption: 'starttls' },
  { id: 'outlook', label: 'Outlook', host: 'smtp-mail.outlook.com', port: 587, encryption: 'starttls' },
]

function applyPreset(p) {
  cfg.value.smtp.host = p.host
  cfg.value.smtp.port = p.port
  cfg.value.smtp.encryption = p.encryption
}

async function testMail() {
  mailTesting.value = true
  mailResult.value = null
  try {
    // Send whatever is in the form, so a fix can be tried before saving it.
    await api.put('/api/config', { smtp: cfg.value.smtp })
    mailResult.value = await api.post('/api/admin/mail/test', { to: mailTestTo.value })
  } catch (e) {
    mailResult.value = { ok: false, error: e.message }
  } finally {
    mailTesting.value = false
  }
}

// 站点地址只用于邮件里的链接,留空就按访问者用的地址拼。
const mailReady = computed(() => !!(cfg.value?.smtp?.host || '').trim())
const verifyButNoMail = computed(() => !!cfg.value?.users?.require_email && !mailReady.value)

const anyScanning = computed(() => (scanStates.value || []).some((s) => s.running))
</script>

<template>
  <div class="page settings-page" v-if="cfg">
    <header class="set-head">
      <div>
        <span class="eyebrow">管理</span>
        <h1>设置</h1>
      </div>
      <div class="status-chips">
        <span v-if="sys" class="badge" :class="sys.ffmpeg_ok ? 'ok' : 'err'">
          <AppIcon :name="sys.ffmpeg_ok ? 'check' : 'alert'" :size="12" /> ffmpeg
        </span>
        <span v-if="sys" class="badge" :class="sys.ffprobe_ok ? 'ok' : 'err'">
          <AppIcon :name="sys.ffprobe_ok ? 'check' : 'alert'" :size="12" /> ffprobe
        </span>
        <span v-if="sys" class="badge" :class="sys.tmdb_ok ? 'ok' : ''">
          <AppIcon name="sparkles" :size="12" /> TMDB {{ sys.tmdb_ok ? '已配置' : '未配置' }}
        </span>
        <span v-if="anyScanning" class="badge warn"><AppIcon name="scan" :size="12" /> 扫描进行中</span>
      </div>
    </header>

    <div v-if="error" class="flash err"><AppIcon name="alert" :size="16" /> {{ error }}</div>

    <div class="set-layout">
      <!-- ================= nav ================= -->
      <aside class="set-nav">
        <button
          v-for="t in tabs" :key="t.id" class="nav-item" :class="{ on: tab === t.id }"
          @click="tab = t.id"
        >
          <span class="ni-icon"><AppIcon :name="t.icon" :size="17" /></span>
          <span class="ni-text">
            <span class="ni-label">{{ t.label }}</span>
            <span class="ni-desc">{{ t.desc }}</span>
          </span>
        </button>
      </aside>

      <!-- ================= content ================= -->
      <div class="set-body">
        <!-- ---------- sources ---------- -->
        <section v-show="tab === 'sources'" class="stack">
          <div class="card">
            <div class="card-head">
              <div>
                <div class="card-title"><AppIcon name="plus" :size="16" /> 添加存储源</div>
                <div class="tiny">支持 OpenList / AList、WebDAV、服务器本地目录,以及 B 站番剧(正版番剧,服务端合流 + 弹幕)。</div>
              </div>
              <button class="btn sm" @click="addingSource = !addingSource">
                {{ addingSource ? '收起' : '展开表单' }}
              </button>
            </div>

            <Transition name="fade">
              <div v-if="addingSource" class="add-panel">
                <div class="type-cards">
                  <button
                    v-for="t in sourceTypes" :key="t.id" class="type-card"
                    :class="{ on: srcType === t.id }" @click="srcType = t.id"
                  >
                    <AppIcon :name="t.icon" :size="18" />
                    <span class="tc-label">{{ t.label }}</span>
                    <span class="tiny">{{ t.hint }}</span>
                  </button>
                </div>

                <div class="form-grid">
                  <div class="field">
                    <label>名称</label>
                    <input v-model="srcForm.name" placeholder="我的天翼云盘" />
                  </div>
                  <template v-if="srcType === 'openlist'">
                    <div class="field">
                      <label>OpenList 地址</label>
                      <input v-model="srcForm.base_url" placeholder="http://127.0.0.1:5244" />
                    </div>
                    <div class="field">
                      <label>Token(可选,建议填管理员 token)</label>
                      <input v-model="srcForm.token" placeholder="alist-xxxx" />
                    </div>
                  </template>
                  <template v-else-if="srcType === 'webdav'">
                    <div class="field">
                      <label>WebDAV 地址</label>
                      <input v-model="srcForm.webdav_url" placeholder="http://127.0.0.1:5244/dav" />
                    </div>
                    <div class="field"><label>用户名</label><input v-model="srcForm.webdav_user" /></div>
                    <div class="field"><label>密码</label><input v-model="srcForm.webdav_pass" type="password" /></div>
                  </template>
                  <template v-else-if="srcType === 'bilibili'">
                    <div class="field wide">
                      <label>账号 Cookie(留空只看免费集,且最高 480P)</label>
                      <textarea
                        v-model="srcForm.bili_cookie" rows="3"
                        placeholder="SESSDATA=xxx; bili_jct=xxx; buvid3=xxx;  —— 浏览器 F12 → 应用 → Cookie → bilibili.com 整条复制"
                      ></textarea>
                      <div class="tiny">
                        这一条等同于你的 B 站账号(含大会员),只有管理员能读到;普通用户拿到的是掩码。
                        想省事可以只填 <code>SESSDATA</code>,但填全更不容易被风控。
                      </div>
                    </div>
                    <div class="field">
                      <label>编码偏好</label>
                      <select v-model="srcForm.bili_codec">
                        <option value="avc">AVC / H.264(兼容最好,推荐)</option>
                        <option value="hevc">HEVC / H.265(更省带宽)</option>
                        <option value="av1">AV1(最小,但能播的设备最少)</option>
                        <option value="auto">不限,由清晰度决定</option>
                      </select>
                      <div class="tiny">同一清晰度 B 站会同时给几种编码;AVC 是唯一所有客户端都能解、且不需要额外封装的。</div>
                    </div>
                    <div class="field wide">
                      <div class="tiny">
                        清晰度不在这里固定:播放时用播放器控制栏的「清晰度」按钮随时切换,
                        默认是「自动」——取当前账号能给的最高一档(SDR 优先)。切换会从当前位置接着播。
                      </div>
                    </div>

                    <!-- Region: the reason a server abroad needs this source to
                         have a way out of its own. -->
                    <div class="field wide">
                      <label>代理(可选 —— 服务器在海外时用,番剧才会解锁)</label>
                      <input
                        v-model="srcForm.bili_proxy"
                        placeholder="ss://aes-256-gcm:密码@host:8388  /  socks5://127.0.0.1:1080  /  http://127.0.0.1:7890"
                      />
                      <div class="tiny">
                        B 站按 IP 判断地区,海外 IP 看番剧会报 <code>-10403</code>。填一个落地在<b>中国大陆</b>的代理,
                        大陆版权的番剧就都能看;落地在<b>香港 / 澳门 / 台湾</b>则能看港澳台版权的番剧(大陆独家的看不了)。
                        这里的流量(接口、CDN 流、服务端合流用的 ffmpeg)都会走它。
                        支持 <b>Shadowsocks</b>(<code>ss://</code>,aes-128/256-gcm、chacha20-ietf-poly1305)、
                        <b>SOCKS5</b>(<code>socks5://</code>,可带用户名密码)和 <b>HTTP</b> 代理(<code>http://</code>)。
                        也可以直接从客户端配置里粘贴 <code>ss://</code> 分享链接或 JSON。
                      </div>
                      <div class="btn-row" style="margin-top:10px">
                        <button class="btn sm" :disabled="proxyTesting" @click="testBiliProxy">
                          <AppIcon name="wifi" :size="14" /> {{ proxyTesting ? '测试中…' : '测试这个代理' }}
                        </button>
                        <span class="tiny">会经这个代理访问 B 站接口,告诉你它看到的地区</span>
                      </div>
                      <div v-if="proxyResult" class="flash" :class="proxyResult.ok && (proxyResult.mainland || proxyResult.hmt) ? 'ok' : 'err'" style="margin:10px 0 0">
                        <AppIcon :name="proxyResult.ok && (proxyResult.mainland || proxyResult.hmt) ? 'check' : 'alert'" :size="16" />
                        <span>{{ proxyResult.message || proxyResult.error }}</span>
                      </div>
                    </div>

                    <div class="field wide">
                      <label class="check-line">
                        <input type="checkbox" v-model="srcForm.bili_playable_only" />
                        只上架当前线路能播放的番剧
                      </label>
                      <div class="tiny">
                        扫描媒体库时逐季请求一次 B 站播放接口:当前地区看不了的季度(<code>-10403</code>)按空目录处理,
                        不会进媒体库;之前扫描留下的这类剧集,重扫时也会被移除。
                        网络失败、风控或账号问题不会误删任何东西(只有明确的地区限制才跳过)。
                        代理在港澳台时建议保持开启 —— 它正好留下港澳台限定、跳过大陆限定。
                      </div>
                    </div>
                  </template>
                  <template v-else>
                    <div class="field">
                      <label>服务器上的绝对路径</label>
                      <input v-model="srcForm.root" placeholder="D:\media 或 /mnt/media" />
                    </div>
                  </template>
                </div>

                <div class="btn-row">
                  <button class="btn" :disabled="testing" @click="testSource">
                    <AppIcon name="wifi" :size="15" /> {{ testing ? '测试中…' : '测试连接' }}
                  </button>
                  <button class="btn primary" @click="addSource">
                    <AppIcon name="check" :size="15" /> {{ editingSource ? '保存修改' : '添加' }}
                  </button>
                  <button v-if="editingSource" class="btn sm" @click="cancelEditSource">取消编辑</button>
                </div>
                <div v-if="editingSource" class="tiny" style="margin-top:8px">
                  正在编辑「{{ srcForm.name || '未命名源' }}」。保存会用表单里的值替换整份配置,
                  <b>清空的字段就是清空</b>(代理想去掉就清掉它);填错的占位符 <code>******</code> 不会被写回去。
                </div>

                <Transition name="fade">
                  <div v-if="testResult" class="flash" :class="testResult.ok ? 'ok' : 'err'" style="margin:14px 0 0">
                    <AppIcon :name="testResult.ok ? 'check' : 'alert'" :size="16" />
                    <span v-if="testResult.ok">
                      连接成功,根目录 {{ testResult.count }} 项:{{ (testResult.entries || []).slice(0, 6).join(', ') }}
                    </span>
                    <span v-else>{{ testResult.error }}</span>
                  </div>
                </Transition>
                <!-- The region answer rides along with the connection test: it
                     is the fact that decides whether 番剧 will play at all. -->
                <Transition name="fade">
                  <div v-if="testResult && testResult.region" class="flash" :class="testResult.region.ok && (testResult.region.mainland || testResult.region.hmt) ? 'ok' : 'err'" style="margin:10px 0 0">
                    <AppIcon :name="testResult.region.ok && (testResult.region.mainland || testResult.region.hmt) ? 'check' : 'alert'" :size="16" />
                    <span>{{ testResult.region.message || testResult.region.error }}</span>
                  </div>
                </Transition>
              </div>
            </Transition>
          </div>

          <div v-if="sources.length" class="list">
            <div v-for="s in sources" :key="s.id" class="row-card">
              <span class="r-icon"><AppIcon :name="s.type === 'openlist' ? 'cloud' : s.type === 'webdav' ? 'globe' : s.type === 'bilibili' ? 'tv' : 'hdd'" :size="18" /></span>
              <div class="r-main">
                <div class="r-title">{{ s.name }} <span class="badge">{{ s.type }}</span></div>
                <div class="tiny truncate">{{ sourceLine(s) }}</div>
                <!-- The account verdict. A pasted cookie dies when the browser
                     rotates it, and the only symptom is a bad picture — so the
                     answer to "why is this blurry" belongs on the card, not
                     behind an edit form. -->
                <template v-if="s.type === 'bilibili'">
                  <div v-if="biliHealthLoading === s.id" class="tiny dim" style="margin-top:4px">正在核对账号…</div>
                  <div v-else-if="biliHealth[s.id]" class="bili-health" :class="biliTone(biliHealth[s.id])">
                    <AppIcon :name="biliTone(biliHealth[s.id]) === 'ok' ? 'check' : biliTone(biliHealth[s.id]) === 'err' ? 'alert' : 'info'" :size="14" />
                    <span>{{ biliHealth[s.id].message }}</span>
                  </div>
                  <div v-if="biliHealth[s.id] && biliHealth[s.id].hint" class="tiny dim" style="margin-top:2px">
                    {{ biliHealth[s.id].hint }}
                  </div>
                  <div class="btn-row" style="margin-top:8px">
                    <button class="btn sm" @click="startQRLogin(s)">
                      <AppIcon name="qr" :size="14" /> {{ biliHealth[s.id] && biliHealth[s.id].logged_in ? '重新扫码登录' : '扫码登录' }}
                    </button>
                    <button
                      v-if="biliHealth[s.id] && biliHealth[s.id].refreshable"
                      class="btn sm" title="用 refresh_token 换一个新的 Cookie,不用重新扫码"
                      @click="refreshBiliSession(s)"
                    >
                      <AppIcon name="refresh" :size="14" /> 续期
                    </button>
                    <button class="btn sm" :disabled="biliHealthLoading === s.id" @click="loadBiliHealth(s)">
                      <AppIcon name="refresh" :size="14" /> 重新检查
                    </button>
                  </div>
                </template>
              </div>
              <button class="btn sm" title="编辑这个源(不用删了重加)" @click="startEditSource(s)"><AppIcon name="wand" :size="14" /> 编辑</button>
              <button class="btn sm danger" @click="delSource(s.id)"><AppIcon name="trash" :size="14" /> 删除</button>
            </div>
          </div>

          <EmptyState v-else icon="cloud" title="还没有存储源" text="点上面的「展开表单」添加第一个存储源。" />
        </section>

        <!-- ---------- libraries ---------- -->
        <section v-show="tab === 'libraries'" class="stack">
          <div class="card">
            <div class="card-head">
              <div>
                <div class="card-title"><AppIcon name="plus" :size="16" /> 创建媒体库</div>
                <div class="tiny">选择存储源与目录,扫描后会自动整理剧集结构并刮削元数据。</div>
              </div>
              <button class="btn sm" @click="addingLibrary = !addingLibrary">{{ addingLibrary ? '收起' : '展开表单' }}</button>
            </div>

            <Transition name="fade">
              <div v-if="addingLibrary" class="add-panel">
                <div v-if="!sources.length" class="flash err">
                  <AppIcon name="alert" :size="16" /> 请先在「存储源」里添加一个源。
                </div>
                <template v-else>
                  <div class="form-grid">
                    <div class="field"><label>名称</label><input v-model="libForm.name" placeholder="电影" /></div>
                    <div class="field">
                      <label>类型</label>
                      <select v-model="libForm.type">
                        <option value="movie">电影</option>
                        <option value="series">剧集</option>
                      </select>
                    </div>
                    <div class="field wide">
                      <label>存储源与路径(可以加多个,扫描时一起扫)</label>
                      <div class="src-rows">
                        <div v-for="(row, i) in libForm.sources" :key="i" class="src-row">
                          <select v-model="row.source_id">
                            <option v-for="s in sources" :key="s.id" :value="s.id">{{ s.name }}</option>
                          </select>
                          <input v-model="row.path" placeholder="/movies" />
                          <button
                            class="btn sm danger" :disabled="libForm.sources.length <= 1"
                            title="删掉这个源" @click="removeLibSource(i)"
                          ><AppIcon name="trash" :size="14" /></button>
                        </div>
                        <button class="btn sm" @click="addLibSource">
                          <AppIcon name="plus" :size="14" /> 再加一个源
                        </button>
                      </div>
                      <div class="tiny">同一个库里的多个源会一起扫描、合并成同一批剧集;某部剧拆在两个盘上也没关系。</div>
                    </div>
                  </div>
                  <div class="field">
                    <label>刮削来源(按优先级)</label>
                    <div class="type-picker">
                      <button
                        v-for="p in ['nfo', 'tmdb', 'bgm']" :key="p" class="chip"
                        :class="{ on: libForm.providers.includes(p) }"
                        @click="libForm.providers.includes(p) ? libForm.providers.splice(libForm.providers.indexOf(p), 1) : libForm.providers.push(p)"
                      >{{ providerLabels[p] }}</button>
                    </div>
                  </div>
                  <div class="field">
                    <label>扫描排除</label>
                    <ExcludeKeywords
                      v-model="libForm.excludes"
                      v-model:mode="libForm.excludeMode"
                      :inherited="cfg?.exclude_keywords || []"
                    />
                  </div>
                  <button class="btn primary" :disabled="!libForm.sources.some(s => s.source_id) || !libForm.name" @click="addLibrary">
                    <AppIcon name="check" :size="15" /> 创建媒体库
                  </button>
                </template>
              </div>
            </Transition>
          </div>

          <div v-if="libraries.length" class="list">
            <div v-for="lib in libraries" :key="lib.id" class="card lib-card">
              <template v-if="editing === lib.id">
                <div class="card-title"><AppIcon name="settings" :size="16" /> 编辑「{{ lib.name }}」</div>
                <div class="form-grid">
                  <div class="field"><label>名称</label><input v-model="editForm.name" /></div>
                  <div class="field">
                    <label>类型</label>
                    <select v-model="editForm.type">
                      <option value="movie">电影</option>
                      <option value="series">剧集</option>
                    </select>
                  </div>
                  <div class="field wide">
                    <label>存储源与路径(可以加多个)</label>
                    <div class="src-rows">
                      <div v-for="(row, i) in editForm.sources" :key="i" class="src-row">
                        <select v-model="row.source_id">
                          <option v-for="s in sources" :key="s.id" :value="s.id">{{ s.name }}</option>
                        </select>
                        <input v-model="row.path" placeholder="/movies" />
                        <button
                          class="btn sm danger" :disabled="editForm.sources.length <= 1"
                          title="删掉这个源" @click="removeEditSource(i)"
                        ><AppIcon name="trash" :size="14" /></button>
                      </div>
                      <button class="btn sm" @click="addEditSource">
                        <AppIcon name="plus" :size="14" /> 再加一个源
                      </button>
                    </div>
                  </div>
                </div>
                <div class="field">
                  <label>刮削来源(按优先级)</label>
                  <div class="type-picker">
                    <button v-for="p in ['nfo', 'tmdb', 'bgm']" :key="p" class="chip" :class="{ on: editForm.providers.includes(p) }" @click="toggleProvider(p)">
                      {{ providerLabels[p] }}
                    </button>
                  </div>
                </div>
                <div class="field">
                  <label>扫描排除</label>
                  <ExcludeKeywords
                    v-model="editForm.excludes"
                    v-model:mode="editForm.excludeMode"
                    :inherited="cfg?.exclude_keywords || []"
                  />
                </div>
                <div class="field">
                  <label>封面(Emby 里的媒体库图标)</label>
                  <div class="type-picker">
                    <button
                      class="chip" :class="{ on: editForm.poster_mode !== 'static' }"
                      @click="editForm.poster_mode = 'auto'"
                    >动态(跟随最新入库)</button>
                    <button
                      class="chip" :class="{ on: editForm.poster_mode === 'static' }"
                      @click="editForm.poster_mode = 'static'"
                    >静态(固定一张)</button>
                  </div>
                  <div v-if="editForm.poster_mode === 'static'" class="lib-poster">
                    <img
                      v-if="!posterMissing"
                      :src="posterSrc(lib.id)" class="lib-poster-thumb" alt="封面预览"
                      @error="posterMissing = true"
                    />
                    <div v-else class="lib-poster-thumb lib-poster-empty tiny dim">还没有封面</div>
                    <div class="lib-poster-actions">
                      <div class="btn-row">
                        <label class="btn sm" :class="{ disabled: posterBusy }">
                          <AppIcon name="image" :size="14" /> 选取图片
                          <input
                            type="file" hidden accept="image/png,image/jpeg,image/webp"
                            :disabled="posterBusy" @change="pickLibraryPoster(lib.id, $event)"
                          />
                        </label>
                        <button class="btn sm" :disabled="posterBusy" @click="autoLibraryPoster(lib.id)">
                          <AppIcon name="download" :size="14" /> 自动获取
                        </button>
                        <button class="btn sm danger" :disabled="posterBusy" @click="clearLibraryPoster(lib.id)">
                          <AppIcon name="trash" :size="14" /> 恢复动态
                        </button>
                      </div>
                      <div class="tiny dim">
                        这两个按钮改完立即生效,不用点保存。自动获取取该媒体库最近入库、且已缓存海报的条目。
                      </div>
                    </div>
                  </div>
                  <div v-else class="tiny dim">
                    动态封面由该库最近入库、且已缓存海报的条目决定,新片入库后会跟着变;切到静态可以自己选一张。
                  </div>
                </div>
                <div class="btn-row">
                  <button class="btn primary" @click="saveEdit(lib.id)"><AppIcon name="check" :size="15" /> 保存</button>
                  <button class="btn" @click="editing = null">取消</button>
                </div>
              </template>

              <template v-else>
                <div class="lib-line">
                  <span class="r-icon"><AppIcon :name="lib.type === 'series' ? 'tv' : 'film'" :size="18" /></span>
                  <div class="r-main">
                    <div class="r-title">
                      {{ lib.name }}
                      <span class="badge">{{ lib.type === 'movie' ? '电影' : '剧集' }}</span>
                    </div>
                    <div class="tiny truncate">
                      {{ libSourcesLabel(lib) }}
                      · 刮削 {{ providersOf(lib).map(p => providerLabels[p] || p).join(' / ') }}
                    </div>
                  </div>
                  <div class="lib-actions">
                    <button v-if="stFor(lib.id)?.running" class="btn sm" @click="stopScan(lib.id)">
                      <AppIcon name="close" :size="14" /> 停止
                    </button>
                    <button v-else class="btn sm primary" @click="scanLibrary(lib.id)">
                      <AppIcon name="scan" :size="14" /> 扫描
                    </button>
                    <button
                      v-if="auditFor(lib.id)?.state.running"
                      class="btn sm" @click="stopAudit(lib.id)"
                      title="停止字幕体检,已查到的结果保留"
                    >
                      <AppIcon name="close" :size="14" /> 停止体检
                    </button>
                    <button
                      v-else
                      class="btn sm" @click="startAudit(lib.id)"
                      title="只查 bdrip:WebRip 的字幕通常烧在画面里,查不出来也说明不了什么"
                    >
                      <AppIcon name="subtitles" :size="14" /> 查缺字幕
                    </button>
                    <button class="btn sm" @click="startEdit(lib)"><AppIcon name="settings" :size="14" /> 编辑</button>
                    <button class="btn sm danger" @click="delLibrary(lib.id)"><AppIcon name="trash" :size="14" /></button>
                  </div>
                </div>

                <!-- 媒体库扫描:两级进度条 + 滚动日志 -->
                <LiveProgress
                  v-if="stFor(lib.id)?.running"
                  :state="stFor(lib.id)"
                  @stop="stopScan(lib.id)"
                />
                <div v-else-if="stFor(lib.id)?.phase === 'done'" class="tiny scan-done">
                  <AppIcon name="check" :size="13" />
                  上次扫描完成:{{ stFor(lib.id).total }} 个文件 · {{ fmtClock(stFor(lib.id).finished_at) }}
                  <span v-if="stFor(lib.id).error" class="scan-warn"> · {{ stFor(lib.id).error }}</span>
                </div>
                <div v-else-if="stFor(lib.id)?.phase === 'error'" class="scan-err">
                  <AppIcon name="alert" :size="14" />
                  扫描失败:{{ stFor(lib.id).error || stFor(lib.id).first_error || '未知错误' }}
                </div>

                <!-- 字幕体检 -->
                <template v-if="auditFor(lib.id) && auditFor(lib.id).state.phase !== 'idle'">
                  <LiveProgress
                    v-if="auditFor(lib.id).state.running"
                    :state="auditFor(lib.id).state"
                    @stop="stopAudit(lib.id)"
                  />
                  <div v-else-if="auditFor(lib.id).state.phase === 'error'" class="scan-err">
                    <AppIcon name="alert" :size="14" /> 字幕体检失败:{{ auditFor(lib.id).state.error }}
                  </div>

                  <div class="audit-sum">
                    <div class="audit-sum-head">
                      <span class="card-title" style="margin:0"><AppIcon name="subtitles" :size="15" /> 字幕体检(bdrip)</span>
                      <button class="btn sm" @click="auditOpen[lib.id] = !auditOpen[lib.id]">
                        {{ auditOpen[lib.id] ? '收起' : '展开' }}结果
                      </button>
                    </div>
                    <div class="audit-tiles">
                      <span class="tiny"><b class="num">{{ auditFor(lib.id).state.total }}</b> 个 bdrip 文件</span>
                      <span class="tiny"><b class="num">{{ auditFor(lib.id).state.with_subs }}</b> 有外挂字幕</span>
                      <span class="tiny"><b class="num">{{ auditFor(lib.id).state.embedded }}</b> 有内嵌字幕</span>
                      <span class="tiny"><b class="num warn">{{ auditFor(lib.id).state.missing }}</b> 缺字幕</span>
                      <span v-if="auditFor(lib.id).state.skipped" class="tiny dim">跳过 {{ auditFor(lib.id).state.skipped }} 个非 bdrip</span>
                      <span v-if="auditFor(lib.id).state.unverified" class="tiny dim" :title="`${auditFor(lib.id).state.list_errors} 个目录读取失败(源上已不存在或无权限)`">
                        {{ auditFor(lib.id).state.unverified }} 个无法判断
                      </span>
                      <span class="tiny dim">{{ fmtClock(auditFor(lib.id).state.finished_at || auditFor(lib.id).state.started_at) }}</span>
                    </div>

                    <div v-if="auditOpen[lib.id]" class="audit-body">
                      <EmptyState
                        v-if="!auditFor(lib.id).items.length"
                        icon="check" title="没有缺字幕的剧集"
                        :text="auditFor(lib.id).state.running ? '外挂字幕已经查完,正在后台确认内嵌字幕。' : '这个库里每个 bdrip 文件都能找到字幕。'"
                      />
                      <div v-else class="audit-groups">
                        <div v-for="g in auditGroups(auditFor(lib.id).items)" :key="g.key" class="audit-group">
                          <div class="audit-group-head">
                            <router-link v-if="g.showId" :to="`/item/${g.showId}`" class="audit-show">{{ g.show }}</router-link>
                            <span v-else class="audit-show">{{ g.show }}</span>
                            <span class="badge warn">{{ g.count }} 集</span>
                            <span v-if="g.checking" class="tiny dim">· {{ g.checking }} 集待确认</span>
                          </div>
                          <div v-for="s in g.seasons" :key="s.seasonId" class="audit-season">
                            <div class="audit-season-head tiny">
                              <router-link v-if="s.seasonId" :to="`/season/${s.seasonId}`">第 {{ s.season }} 季</router-link>
                              <span v-else>第 {{ s.season }} 季</span>
                            </div>
                            <div class="audit-eps">
                              <router-link
                                v-for="e in s.episodes" :key="e.file_id"
                                class="audit-ep" :class="{ pending: e.checking, failed: e.error }"
                                :to="e.season_id ? `/season/${e.season_id}` : `/item/${e.item_id}`"
                                :title="e.path + (e.error ? '\n探测失败:' + e.error : '')"
                              >
                                {{ auditEpisodeLabel(e) }}
                                <span v-if="e.checking" class="dot-pending">?</span>
                                <span v-else-if="e.error" class="dot-failed">!</span>
                              </router-link>
                            </div>
                          </div>
                        </div>
                      </div>
                      <div class="tiny dim audit-note">
                        带 <b>?</b> 的还没确认完(正在探测内嵌字幕轨),可能随时从名单里消失;
                        带 <b>!</b> 的是探测失败,还没定论。这里只查 bdrip —— WebRip 的字幕多半烧在画面里,
                        没有文件也没有轨道可查,报了也不准。
                        <template v-if="auditFor(lib.id).state.unverified">
                          另有 {{ auditFor(lib.id).state.unverified }} 个文件因为所在目录读不出来(源上已删除或无权限)没有计入,
                          它们既不算缺字幕也不算有字幕。
                        </template>
                      </div>
                    </div>
                  </div>
                </template>
              </template>
            </div>
          </div>

          <EmptyState v-else icon="film" title="还没有媒体库" text="创建媒体库后扫描,首页就会出现海报墙。" />
        </section>

        <!-- ---------- playback / metadata ---------- -->
        <section v-show="tab === 'playback'" class="stack">
          <div class="card">
            <div class="card-title"><AppIcon name="film" :size="16" /> TMDB 刮削</div>
            <div class="form-grid">
              <div class="field wide">
                <label>API Key</label>
                <input v-model="cfg.tmdb.api_key" placeholder="API 密钥(v3)或 API 读访问令牌(v4)均可" />
                <span class="hint">保存后立即生效,无需重启;国内网络请开启下面的本地代理,或把地址换成镜像。</span>
              </div>
              <div class="field"><label>语言</label><input v-model="cfg.tmdb.language" placeholder="zh-CN" /></div>
              <div class="field">
                <label>API 地址(可填镜像/反代)</label>
                <input v-model="cfg.tmdb.api_base" placeholder="https://api.themoviedb.org/3" />
              </div>
              <div class="field">
                <label>图片地址(可填镜像/反代)</label>
                <input v-model="cfg.tmdb.img_base" placeholder="https://image.tmdb.org/t/p" />
              </div>
              <div class="field wide">
                <label>网络代理</label>
                <label class="check-line">
                  <input type="checkbox" v-model="cfg.tmdb.use_proxy" />
                  走本地代理访问 TMDB(国内直连不通时必开)
                </label>
                <input v-model="cfg.tmdb.proxy" placeholder="socks5://127.0.0.1:7890" :disabled="!cfg.tmdb.use_proxy" />
                <span class="hint">支持 socks5:// 与 http://(Clash / v2rayN 默认端口通常是 7890)。海报与背景图下载也走这个代理。</span>
                <div class="test-line">
                  <button class="btn" :disabled="tmdbTesting" @click="testTMDB">
                    <AppIcon name="wifi" :size="15" /> {{ tmdbTesting ? '测试中…' : '测试连接' }}
                  </button>
                  <span v-if="tmdbTest" class="tiny" :class="tmdbTest.ok ? 'ok-text' : 'err-text'">
                    <AppIcon :name="tmdbTest.ok ? 'check' : 'alert'" :size="13" /> {{ tmdbTest.message }}
                  </span>
                </div>
              </div>
            </div>
          </div>

          <div class="card">
            <div class="card-title"><AppIcon name="sparkles" :size="16" /> Bangumi 刮削(bgm.tv)</div>
            <div class="form-grid">
              <div class="field wide">
                <label>访问令牌(可选)</label>
                <input v-model="cfg.bgm.token" placeholder="留空即可,公开接口够用;填了可提高限流上限" />
                <span class="hint">Bangumi 是番剧信息最全的中文数据源:中文剧名、每集中文标题、播出日期、评分与标签。在媒体库里勾选 <b>Bangumi</b> 才启用。</span>
              </div>
              <div class="field">
                <label>API 地址(可填镜像/反代)</label>
                <input v-model="cfg.bgm.api_base" placeholder="https://api.bgm.tv" />
              </div>
              <div class="field wide">
                <label>网络代理</label>
                <label class="check-line">
                  <input type="checkbox" v-model="cfg.bgm.use_proxy" />
                  走本地代理访问 Bangumi(api.bgm.tv 常被 DNS 污染,建议保持开启)
                </label>
                <input v-model="cfg.bgm.proxy" placeholder="socks5://127.0.0.1:7890" :disabled="!cfg.bgm.use_proxy" />
                <div class="test-line">
                  <button class="btn" :disabled="bgmTesting" @click="testBGM">
                    <AppIcon name="wifi" :size="15" /> {{ bgmTesting ? '测试中…' : '测试连接' }}
                  </button>
                  <input v-model="bgmKeyword" class="input inline-input" placeholder="测试关键词,如:孤独摇滚" />
                  <span v-if="bgmTest" class="tiny" :class="bgmTest.ok ? 'ok-text' : 'err-text'">
                    <AppIcon :name="bgmTest.ok ? 'check' : 'alert'" :size="13" /> {{ bgmTest.message }}
                  </span>
                </div>
                <span v-if="bgmTest && bgmTest.hits && bgmTest.hits.length" class="hint">搜到:{{ bgmTest.hits.join(' / ') }}</span>
              </div>
            </div>
          </div>

          <div class="card">
            <div class="card-title"><AppIcon name="playCircle" :size="16" /> 播放</div>
            <div class="form-grid">
              <div class="field">
                <label>默认播放模式</label>
                <select v-model="cfg.playback.force_mode">
                  <option value="auto">自动决策(推荐)</option>
                  <option value="direct">强制 302 直连</option>
                  <option value="proxy">强制代理</option>
                  <option value="remux">强制 HLS 转封装</option>
                </select>
                <span class="hint">
                  自动决策:客户端能直接播的走 302/代理,不兼容的自动转封装(H.264/H.265 视频流直拷,CPU 几乎无消耗)。
                  失败后的备用顺序是 直连 → 代理 → 转封装,而且只会尝试这个客户端真的有可能播的方式。
                  <b>服务器不再转码</b>:本机解码不了的编码(如 AV1、10bit H.264)会明确提示无法播放,
                  不会偷偷起一个实时编码把 CPU 吃满。
                </span>
              </div>
              <div class="field"><label>HLS 分片时长(秒)</label><input v-model="cfg.playback.segment_seconds" type="number" step="1" min="2" max="10" /></div>
              <div class="field">
                <label>字幕烧录输出高度(像素)</label>
                <input v-model="cfg.playback.transcode_height" type="number" step="80" min="480" max="2160" />
                <span class="hint">只有「烧录图形字幕」这一条路会重新编码画面,这里限制它的输出高度(默认 1080)。普通播放永不转码。</span>
              </div>
              <div class="field">
                <label>探测读取上限(MB)</label>
                <input v-model="cfg.playback.probe_size_mb" type="number" step="1" min="1" max="256" />
                <span class="hint">决定起播前要从网盘读多少数据来识别容器/编码。默认 8MB,越小起播越快;读不出来时会自动用 50MB 深度探测兜底。</span>
              </div>
              <div class="field">
                <label>探测分析时长(秒)</label>
                <input v-model="cfg.playback.probe_analyze_sec" type="number" step="1" min="1" max="60" />
                <span class="hint">探测时分析多少秒的数据流,默认 4 秒。音轨/字幕流很靠后的片源可以调大。</span>
              </div>
              <div class="field wide">
                <label>扫描后预探测</label>
                <label class="check-line">
                  <input type="checkbox" v-model="cfg.playback.preprobe_after_scan" />
                  扫描完成后在后台预探测新文件(打开即播,不用等)
                </label>
                <span class="hint">只探测文件名未缓存过的文件,限流串行执行,不会与播放抢带宽。</span>
              </div>
              <div class="field">
                <label>预探测文件数上限</label>
                <input v-model="cfg.playback.preprobe_max_files" type="number" step="10" min="0" :disabled="!cfg.playback.preprobe_after_scan" />
                <span class="hint">每次扫描最多预探测多少个文件,0 表示不限。大库建议限量,避免把网盘读限速。</span>
              </div>
              <div class="field">
                <label>预探测并发数</label>
                <input v-model="cfg.playback.preprobe_concurrency" type="number" step="1" min="1" max="8" :disabled="!cfg.playback.preprobe_after_scan" />
                <span class="hint">默认 1(串行)。网盘容易限速时不要调大。</span>
              </div>
              <div class="field wide">
                <label>HEVC 直拷(iOS / Apple)</label>
                <label class="check-line">
                  <input type="checkbox" v-model="cfg.playback.apple_hevc_copy" />
                  允许 Apple 设备把 HEVC 直拷进 MPEG-TS
                </label>
                <span class="hint">
                  <b>建议保持关闭</b>：Apple 不解码 MPEG-TS 里的 HEVC,直拷会变成「有声音、没画面」。
                  关闭时这类片子会被判为无法播放 —— 想让它能播,请打开下面的「会话打包」,
                  那才是把 HEVC 直拷给 Apple 的正确形式(fMP4)。
                </span>
              </div>
              <div class="field wide">
                <label>会话打包(fMP4,Apple 直拷 HEVC)</label>
                <label class="check-line">
                  <input type="checkbox" v-model="cfg.playback.hls_session_packager" />
                  用长驻 ffmpeg 打包 fMP4,让 Apple 设备直拷 HEVC 而不转码
                </label>
                <span class="hint">
                  Apple 只接受 fMP4 里的 HEVC,这个模式会输出正确的 fMP4(带 #EXT-X-MAP 与 hvc1),
                  于是 HEVC 不再转码、CPU 几乎为零。每个分片只生成一次、不再逐片重启 ffmpeg,
                  也不再来回重读探测窗口。
                </span>
              </div>
              <div class="field">
                <label>分片缓存上限(MB)</label>
                <input v-model="cfg.playback.hls_cache_mb" type="number" step="256" min="0" />
                <span class="hint">
                  打包完成的产物留在 <code>data/cache/hls</code>,同一部片子**再次播放直接秒开**;
                  超出上限时按最近最少使用淘汰,未完成的残留会在启动时清掉。填 0 表示不留缓存
                  (播放结束即删)。默认 2048。
                </span>
              </div>
            </div>
          </div>

          <div class="save-bar">
            <span class="tiny">改动保存在 config.json,立即生效</span>
            <button class="btn primary" @click="saveConfig"><AppIcon name="check" :size="15" /> 保存配置</button>
          </div>
        </section>

        <!-- ---------- users ---------- -->
        <section v-show="tab === 'users'" class="stack">
          <!-- signup policy -->
          <div class="card">
            <div class="card-title"><AppIcon name="lock" :size="16" /> 注册与登录</div>
            <div class="tiny">决定别人能不能自己注册账号。改完立刻生效,不需要重启。</div>

            <div class="reg-modes">
              <button
                v-for="m in regModes" :key="m.id" class="type-card"
                :class="{ on: cfg.users.registration === m.id }"
                @click="cfg.users.registration = m.id; saveRegistration()"
              >
                <AppIcon :name="m.id === 'open' ? 'lockOpen' : m.id === 'approval' ? 'clock' : 'lock'" :size="18" />
                <span class="tc-label">{{ m.label }}</span>
                <span class="tiny">{{ m.hint }}</span>
              </button>
            </div>

            <div class="form-grid mt-4">
              <div class="field">
                <label>密码最短长度</label>
                <input v-model.number="cfg.users.min_password" type="number" min="4" max="128" step="1" />
                <span class="hint">注册、管理员建号与改密都按这个长度校验,至少 4 位。</span>
              </div>
              <div class="field wide">
                <label class="check-line">
                  <input type="checkbox" v-model="cfg.users.require_email" />
                  注册需要邮箱验证(注册后必须点邮件里的链接才能登录)
                </label>
                <span v-if="verifyButNoMail" class="hint warn-text">
                  还没填 SMTP 服务器,邮箱验证不会生效,注册也会被拒绝 —— 请先到「邮件与验证」里配好。
                </span>
                <span v-else class="hint">
                  只影响自助注册的新账号;<b>已有账号不受影响</b>(它们没有邮箱,不会被挡在门外)。
                  管理员建的账号、以及用账号设置自己绑邮箱的人,都由你或他们自己验证。
                </span>
              </div>
            </div>
            <div class="save-bar flat">
              <span class="tiny">改完记得保存</span>
              <button class="btn primary" @click="saveConfig"><AppIcon name="check" :size="15" /> 保存配置</button>
            </div>
          </div>

          <!-- pending approvals -->
          <div v-if="pendingUsers.length" class="card pending-card">
            <div class="card-title"><AppIcon name="clock" :size="16" /> 待审核注册({{ pendingUsers.length }})</div>
            <div class="list">
              <div v-for="u in pendingUsers" :key="u.id" class="row-card">
                <span class="r-icon"><AppIcon name="user" :size="18" /></span>
                <div class="r-main">
                  <div class="r-title">{{ u.name }} <span class="badge warn">待审核</span></div>
                  <div class="tiny">申请时间 {{ fmtDate(u.created_at) }}</div>
                </div>
                <button class="btn sm primary" @click="patchUser(u, { status: 'active' }, `已通过「${u.name}」`)">
                  <AppIcon name="check" :size="14" /> 通过
                </button>
                <button class="btn sm danger" @click="deleteUser(u)"><AppIcon name="close" :size="14" /> 拒绝</button>
              </div>
            </div>
          </div>

          <!-- create -->
          <div class="card">
            <div class="card-head">
              <div>
                <div class="card-title"><AppIcon name="plus" :size="16" /> 添加用户</div>
                <div class="tiny">注册关闭时也能用这里直接建号;管理员可以进后台。</div>
              </div>
              <button class="btn sm" @click="addingUser = !addingUser">{{ addingUser ? '收起' : '展开表单' }}</button>
            </div>

            <Transition name="fade">
              <div v-if="addingUser" class="add-panel">
                <div class="form-grid">
                  <div class="field"><label>用户名</label><input v-model="newUser.name" placeholder="2-32 个字符" /></div>
                  <div class="field"><label>密码</label><input v-model="newUser.password" type="password" placeholder="至少 4 位" /></div>
                  <div class="field">
                    <label>角色</label>
                    <select v-model="newUser.role">
                      <option value="user">普通用户(只能浏览播放)</option>
                      <option value="admin">管理员(可进后台)</option>
                    </select>
                  </div>
                </div>
                <button class="btn primary" :disabled="!newUser.name || !newUser.password" @click="createUser">
                  <AppIcon name="check" :size="15" /> 创建用户
                </button>
              </div>
            </Transition>
          </div>

          <!-- list -->
          <div v-if="users.length" class="list">
            <div v-for="u in users" :key="u.id" class="card user-card" :class="{ off: u.status !== 'active' }">
              <div class="u-head">
                <span class="av">{{ (u.name || '?').slice(0, 1).toUpperCase() }}</span>
                <div class="u-main">
                  <div class="r-title">
                    {{ u.name }}
                    <span class="badge" :class="u.role === 'admin' ? 'rating' : ''">
                      {{ u.role === 'admin' ? '管理员' : '用户' }}
                    </span>
                    <span v-if="u.status !== 'active'" class="badge" :class="u.status === 'pending' ? 'warn' : 'err'">
                      {{ statusLabel(u) }}
                    </span>
                    <span v-if="isSelf(u)" class="badge hd">当前账号</span>
                  </div>
                  <div class="tiny">
                    创建于 {{ fmtDate(u.created_at) }} ·
                    {{ u.last_login_at ? '上次登录 ' + fmtRelative(u.last_login_at) : '从未登录' }}
                  </div>
                  <div v-if="u.email" class="tiny email-line">
                    <AppIcon name="link" :size="12" />
                    <span class="truncate">{{ u.email }}</span>
                    <span class="badge" :class="u.email_verified ? 'ok' : 'warn'">
                      {{ u.email_verified ? '已验证' : '未验证' }}
                    </span>
                  </div>
                  <div v-else class="tiny">未绑定邮箱</div>
                </div>
              </div>

              <div class="u-actions">
                <button
                  v-if="u.status !== 'active'"
                  class="btn sm" @click="patchUser(u, { status: 'active' }, `已启用「${u.name}」`)"
                ><AppIcon name="check" :size="14" /> {{ u.status === 'pending' ? '通过并启用' : '启用' }}</button>

                <button
                  v-else class="btn sm"
                  :disabled="isSelf(u) || lastAdmin(u)"
                  :title="isSelf(u) ? '不能禁用自己的账号' : lastAdmin(u) ? '至少要保留一个可用的管理员' : ''"
                  @click="patchUser(u, { status: 'disabled' }, `已禁用「${u.name}」`)"
                ><AppIcon name="lock" :size="14" /> 禁用</button>

                <button
                  class="btn sm"
                  :disabled="lastAdmin(u)"
                  :title="lastAdmin(u) ? '至少要保留一个可用的管理员' : ''"
                  @click="patchUser(u, { role: u.role === 'admin' ? 'user' : 'admin' }, u.role === 'admin' ? `「${u.name}」已降为普通用户` : `「${u.name}」已成为管理员`)"
                ><AppIcon :name="u.role === 'admin' ? 'user' : 'settings'" :size="14" /> {{ u.role === 'admin' ? '降为用户' : '设为管理员' }}</button>

                <button class="btn sm" @click="setUserEmail(u)">
                  <AppIcon name="link" :size="14" /> {{ u.email ? '改邮箱' : '设邮箱' }}
                </button>
                <button
                  v-if="u.email && !u.email_verified" class="btn sm"
                  @click="verifyUserEmail(u, true)"
                ><AppIcon name="check" :size="14" /> 标记已验证</button>
                <button
                  v-else-if="u.email" class="btn sm ghost" title="邮箱又变得可疑时用"
                  @click="verifyUserEmail(u, false)"
                ><AppIcon name="alert" :size="14" /> 取消验证</button>

                <button class="btn sm" @click="pwTarget = u; pwValue = ''"><AppIcon name="lock" :size="14" /> 重置密码</button>
                <button class="btn sm" @click="revokeSessions(u)"><AppIcon name="power" :size="14" /> 踢下线</button>

                <button
                  class="btn sm danger" :disabled="isSelf(u) || lastAdmin(u)"
                  :title="isSelf(u) ? '不能删除当前登录的账号' : lastAdmin(u) ? '至少要保留一个可用的管理员' : ''"
                  @click="deleteUser(u)"
                ><AppIcon name="trash" :size="14" /> 删除</button>
              </div>
            </div>
          </div>

          <EmptyState v-else icon="users" title="还没有其他用户" text="打开上面的注册开关,或直接添加一个账号。" />
        </section>

        <!-- ---------- mail & captcha ---------- -->
        <section v-show="tab === 'mail'" class="stack">
          <div class="card">
            <div class="card-title"><AppIcon name="link" :size="16" /> 邮件发送(SMTP)</div>
            <div class="tiny">
              注册验证、找回密码都靠它发信。填好后用下面的「测试发送」验证一次,不收信就没必要开邮箱验证。
            </div>

            <div class="preset-row">
              <span class="tiny">快速填充:</span>
              <button v-for="p in presets" :key="p.id" class="chip sm" @click="applyPreset(p)">{{ p.label }}</button>
            </div>

            <div class="form-grid mt-4">
              <div class="field">
                <label>SMTP 服务器</label>
                <input v-model="cfg.smtp.host" placeholder="smtp.qq.com" />
              </div>
              <div class="field">
                <label>端口</label>
                <input v-model.number="cfg.smtp.port" type="number" min="1" max="65535" placeholder="465" />
              </div>
              <div class="field">
                <label>用户名</label>
                <input v-model="cfg.smtp.username" placeholder="you@qq.com" />
                <span class="hint">一般是完整邮箱地址。</span>
              </div>
              <div class="field">
                <label>密码 / 授权码</label>
                <input v-model="cfg.smtp.password" type="password" placeholder="不是登录密码,是邮箱设置里生成的授权码" />
                <span class="hint">QQ / 163 / Gmail 都要用「授权码」或「应用专用密码」,不是网页登录密码。</span>
              </div>
              <div class="field">
                <label>发件人地址</label>
                <input v-model="cfg.smtp.from" placeholder="留空则用上面的用户名" />
              </div>
              <div class="field">
                <label>发件人显示名</label>
                <input v-model="cfg.smtp.from_name" placeholder="DoriyaMedia" />
              </div>
            </div>

            <div class="field">
              <label>加密方式</label>
              <div class="type-cards">
                <button
                  v-for="e in encryptions" :key="e.id" class="type-card"
                  :class="{ on: cfg.smtp.encryption === e.id }" @click="cfg.smtp.encryption = e.id"
                >
                  <AppIcon :name="e.id === 'none' ? 'lockOpen' : 'lock'" :size="18" />
                  <span class="tc-label">{{ e.label }}</span>
                  <span class="tiny">{{ e.hint }}</span>
                </button>
              </div>
            </div>

            <div class="form-grid">
              <div class="field">
                <label>发送走代理(可选)</label>
                <input v-model="cfg.smtp.proxy" placeholder="socks5://127.0.0.1:7890" />
                <span class="hint">留空直连。Gmail 的 SMTP 在国内需要代理才能连上。</span>
              </div>
              <div class="field">
                <label>站点地址(邮件里的链接用)</label>
                <input v-model="cfg.users.base_url" placeholder="https://media.example.com" />
                <span class="hint">留空则按访问者的地址自动拼。反代/域名访问时建议填,否则邮件里的链接可能指向内网地址。</span>
              </div>
            </div>

            <div class="field wide">
              <label class="check-line">
                <input type="checkbox" v-model="cfg.smtp.skip_verify" />
                跳过 TLS 证书校验(仅用于自签证书的本地中继)
              </label>
            </div>

            <div class="btn-row">
              <input v-model="mailTestTo" class="input test-to" placeholder="收件邮箱(留空发给自己)" />
              <button class="btn" :disabled="mailTesting || !mailReady" @click="testMail">
                <AppIcon name="wifi" :size="15" /> {{ mailTesting ? '发送中…' : '测试发送' }}
              </button>
            </div>
            <Transition name="fade">
              <div v-if="mailResult" class="flash mt-4" :class="mailResult.ok ? 'ok' : 'err'">
                <AppIcon :name="mailResult.ok ? 'check' : 'alert'" :size="16" />
                <span>{{ mailResult.ok ? mailResult.message : mailResult.error }}</span>
              </div>
            </Transition>

            <div class="save-bar flat">
              <span class="tiny">测试发送会先保存当前填写的内容</span>
              <button class="btn primary" @click="saveConfig"><AppIcon name="check" :size="15" /> 保存配置</button>
            </div>
          </div>

          <!-- Cloudflare Turnstile -->
          <div class="card">
            <div class="card-head">
              <div>
                <div class="card-title"><AppIcon name="lock" :size="16" /> Cloudflare 人机验证(Turnstile)</div>
                <div class="tiny">在登录页与注册页显示验证组件,挡住脚本猜密码和批量注册。免费,去 Cloudflare 后台建一个 Turnstile 站点即可拿到两把钥匙。</div>
              </div>
              <label class="switch" :class="{ on: cfg.turnstile.enabled }" :aria-checked="!!cfg.turnstile.enabled" role="switch">
                <input type="checkbox" v-model="cfg.turnstile.enabled" class="sr-only" />
              </label>
            </div>

            <div class="form-grid mt-4">
              <div class="field">
                <label>Site Key(公开,给浏览器用)</label>
                <input v-model="cfg.turnstile.site_key" placeholder="0x4AAAAAAA..." />
              </div>
              <div class="field">
                <label>Secret Key(私密,服务端校验用)</label>
                <input v-model="cfg.turnstile.secret_key" type="password" placeholder="0x4AAAAAAA..." />
              </div>
            </div>

            <div class="field">
              <label>服务端校验走代理(可选)</label>
              <input v-model="cfg.turnstile.proxy" placeholder="socks5://127.0.0.1:7890" />
              <span class="hint">
                这只影响服务端向 Cloudflare 发起的校验请求;访客浏览器仍要能访问
                <code>challenges.cloudflare.com</code> 才能显示出验证框 —— 国内网络下这是最大的一道坎。
              </span>
            </div>

            <div v-if="cfg.turnstile.enabled && !(cfg.turnstile.site_key && cfg.turnstile.secret_key)" class="flash warn">
              <AppIcon name="alert" :size="16" />
              <span>两把钥匙都填齐才会真正生效,现在这样等于没开。</span>
            </div>
            <div v-if="cfg.turnstile.enabled && cfg.turnstile.site_key && cfg.turnstile.secret_key" class="flash warn">
              <AppIcon name="alert" :size="16" />
              <span>
                开启后,如果服务器连不上 Cloudflare,<b>所有人都无法登录</b>(包括你)。
                真遇上这情况:把 config.json 里的 <code>turnstile.enabled</code> 改成 false 再重启服务。
              </span>
            </div>

            <div class="save-bar flat">
              <span class="tiny">开启后立即生效,登录页会多出验证框</span>
              <button class="btn primary" @click="saveConfig"><AppIcon name="check" :size="15" /> 保存配置</button>
            </div>
          </div>
        </section>

        <!-- ---------- system ---------- -->
        <section v-show="tab === 'site'" class="stack">
          <div class="card">
            <div class="card-title"><AppIcon name="wand" :size="16" /> 用编辑工具栏改站点外观</div>
            <p class="tiny" style="line-height: 1.7; margin: 0">
              打开工具栏后,页面右下角会出现一条浮动工具栏(<b>文案 / 布局 / 页脚</b>)。
              标题、段落、按钮文字都可以直接在页面上点着改,右边调布局,
              改动<b>立刻</b>能在页面上看到;<b>发布</b>之后所有访客才会看到新版本。
              快捷键:Ctrl/Cmd + S 发布,Ctrl/Cmd + Z 撤销。
            </p>
            <div class="row-flex wrap" style="margin-top: 14px">
              <button class="btn" @click="openEditor('/login', 'copy')">
                <AppIcon name="type" :size="15" /> 改登录页文案
              </button>
              <button class="btn" @click="openEditor('/login', 'layout')">
                <AppIcon name="layout" :size="15" /> 改登录页布局
              </button>
              <button class="btn" @click="openEditor(null, 'footer')">
                <AppIcon name="list" :size="15" /> 改页脚
              </button>
            </div>
            <div class="save-bar flat">
              <span class="tiny">
                <template v-if="dirty"><b style="color: var(--brand)">有未发布的修改</b> · </template>
                {{ publishedLabel() }}
              </span>
              <button class="btn primary" @click="openEditor(null, panelTab || 'copy')">
                <AppIcon name="wand" :size="15" /> 在此页打开工具栏
              </button>
            </div>
          </div>

          <div class="card">
            <div class="card-title"><AppIcon name="eye" :size="16" /> 当前文案</div>
            <div class="form-grid">
              <div class="field">
                <label>站点名</label>
                <input :value="site.doc.site.title" readonly />
              </div>
              <div class="field">
                <label>登录页大标题</label>
                <input :value="site.doc.login.pitch.title" readonly />
              </div>
              <div class="field">
                <label>登录页小标签</label>
                <input :value="site.doc.login.pitch.eyebrow" readonly />
              </div>
              <div class="field">
                <label>页脚状态</label>
                <input :value="site.doc.footer.status" readonly />
              </div>
            </div>
            <p class="tiny" style="margin: 0">
              这些内容随时可以在工具栏里改,这里只是让你一眼看到当前用的是哪一版。
            </p>
          </div>
        </section>

        <section v-show="tab === 'system'" class="stack">
          <div v-if="sys" class="tiles">
            <div class="tile">
              <span class="t-icon"><AppIcon name="server" :size="17" /></span>
              <div><div class="t-k">版本</div><div class="t-v">{{ sys.version }}</div></div>
            </div>
            <div class="tile">
              <span class="t-icon" :class="sys.ffmpeg_ok ? 'ok' : 'err'"><AppIcon name="video" :size="17" /></span>
              <div><div class="t-k">ffmpeg</div><div class="t-v">{{ sys.ffmpeg_ok ? '可用' : '未找到' }}</div></div>
            </div>
            <div class="tile">
              <span class="t-icon" :class="sys.ffprobe_ok ? 'ok' : 'err'"><AppIcon name="scan" :size="17" /></span>
              <div><div class="t-k">ffprobe</div><div class="t-v">{{ sys.ffprobe_ok ? '可用' : '未找到' }}</div></div>
            </div>
            <div class="tile">
              <span class="t-icon" :class="sys.tmdb_ok ? 'ok' : ''"><AppIcon name="sparkles" :size="17" /></span>
              <div><div class="t-k">TMDB</div><div class="t-v">{{ sys.tmdb_ok ? '已配置' : '未配置' }}</div></div>
            </div>
            <div class="tile wide">
              <span class="t-icon"><AppIcon name="hdd" :size="17" /></span>
              <div><div class="t-k">数据目录</div><div class="t-v truncate">{{ sys.data_dir }}</div></div>
            </div>
          </div>

          <div class="card">
            <div class="card-title"><AppIcon name="settings" :size="16" /> 工具路径与性能</div>
            <div class="form-grid">
              <div class="field">
                <label>ffmpeg</label>
                <input v-model="cfg.ffmpeg" />
                <span class="hint">转封装、字幕烧录与字幕转换都依赖它。找不到时填绝对路径。</span>
              </div>
              <div class="field"><label>ffprobe</label><input v-model="cfg.ffprobe" /></div>
              <div class="field">
                <label>扫描并发数</label>
                <input v-model="cfg.scan_concurrency" type="number" min="1" max="16" />
                <span class="hint">网盘源建议 2–4,本地磁盘可以更高。</span>
              </div>
              <div class="field wide">
                <label>扫描排除(全局默认)</label>
                <ExcludeKeywords v-model="cfg.exclude_keywords" />
                <span class="hint">
                  命中的文件不会被导入媒体库——不当剧集、不当电影、不出卡片,也不会被刮削;文件浏览里仍然能播。
                  下面的媒体库可以「跟随全局」或单独设一份。
                </span>
              </div>
            </div>
            <div class="save-bar flat">
              <span class="tiny">保存后立即生效</span>
              <button class="btn primary" @click="saveConfig"><AppIcon name="check" :size="15" /> 保存配置</button>
            </div>
          </div>
        </section>
      </div>
    </div>

    <!-- 重置密码：独立弹窗,避免在用户卡片里塞一个容易误触的输入框 -->
    <Transition name="modal">
      <div v-if="pwTarget" class="modal-mask" @click.self="pwTarget = null">
        <div class="modal" role="dialog" aria-modal="true" aria-label="重置密码">
          <div class="modal-head">
            <div>
              <div class="card-title"><AppIcon name="lock" :size="16" /> 重置「{{ pwTarget.name }}」的密码</div>
              <div class="tiny">保存后该账号在其他设备上的登录会全部失效。</div>
            </div>
          </div>
          <form @submit.prevent="savePassword">
            <div class="field">
              <label for="user-pw">新密码</label>
              <input id="user-pw" v-model="pwValue" type="text" :placeholder="`至少 ${cfg.users.min_password || 4} 位`" autofocus />
              <span class="hint">明文显示,方便你转告对方;对方登录后可在「账号设置」里自行修改。</span>
            </div>
            <div class="btn-row">
              <button type="button" class="btn" @click="pwTarget = null">取消</button>
              <button class="btn primary" :disabled="!pwValue"><AppIcon name="check" :size="15" /> 保存新密码</button>
            </div>
          </form>
        </div>
      </div>
    </Transition>
  </div>

  <div v-else class="page">
    <div class="set-skeleton">
      <div class="sk" style="height:52px;border-radius:var(--r-m)" />
      <div class="sk mt-5" style="height:260px;border-radius:var(--r-l)" />
    </div>
  </div>

  <!-- 扫码登录: the fix for a cookie that a browser rotation killed. A scanned
       session belongs to this server and carries a refresh_token, so it does not
       die the next time somebody opens bilibili in a browser. -->
  <Transition name="fade">
    <div v-if="qr" class="qr-backdrop" @click.self="closeQR">
      <div class="qr-dialog card">
        <div class="card-head">
          <div>
            <div class="card-title"><AppIcon name="qr" :size="16" /> 扫码登录 B 站</div>
            <div class="tiny">{{ qr.source?.name }} · 用 B 站手机客户端扫描下面的二维码</div>
          </div>
          <button class="btn sm" @click="closeQR"><AppIcon name="close" :size="14" /></button>
        </div>

        <div class="qr-body">
          <div class="qr-frame" :class="{ dimmed: qr.state === 'expired' || qr.state === 'error' }">
            <img v-if="qr.png" :src="qr.png" alt="B 站登录二维码" width="240" height="240" />
            <div v-else class="sk" style="width:240px;height:240px;border-radius:var(--r-m)" />
          </div>
          <div class="qr-side">
            <div class="qr-state" :class="qr.state === 'confirmed' ? 'ok' : qr.state === 'expired' || qr.state === 'error' ? 'err' : 'dim'">
              <AppIcon :name="qr.state === 'confirmed' ? 'check' : qr.state === 'expired' || qr.state === 'error' ? 'alert' : 'qr'" :size="16" />
              <span>{{ qr.message }}</span>
            </div>
            <div v-if="qr.account && qr.account.logged_in" class="tiny" style="margin-top:8px">
              账号:{{ qr.account.uname }}
              <template v-if="qr.account.vip_status === 1"> · 大会员</template>
            </div>
            <ol class="qr-steps tiny">
              <li>手机打开 B 站 →「我的」→ 右上角扫一扫</li>
              <li>扫描左侧二维码</li>
              <li>在手机上点「确认登录」</li>
            </ol>
            <div class="tiny dim">
              扫码得到的会话属于这台服务器,带 refresh_token,可以自动续期 ——
              不像从浏览器复制的 Cookie 那样,浏览器一刷新登录态它就失效了。
            </div>
            <div class="btn-row" style="margin-top:10px">
              <button class="btn sm" :disabled="qrStarting" @click="startQRLogin(qr.source)">
                <AppIcon name="refresh" :size="14" /> 换一个二维码
              </button>
              <button v-if="qr.state === 'confirmed'" class="btn sm primary" @click="closeQR">完成</button>
            </div>
          </div>
        </div>
      </div>
    </div>
  </Transition>
</template>

<style scoped>
.settings-page { padding-top: var(--sp-6); }
.set-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: var(--sp-4);
  flex-wrap: wrap;
  margin-bottom: var(--sp-5);
}
.status-chips { display: flex; gap: 7px; flex-wrap: wrap; }
.set-layout { display: grid; grid-template-columns: 262px minmax(0, 1fr); gap: var(--sp-6); align-items: start; }

.set-nav { position: sticky; top: calc(var(--header-h) + 20px); display: flex; flex-direction: column; gap: 4px; }
.nav-item {
  display: flex; align-items: center; gap: 12px;
  padding: 11px 13px;
  border-radius: var(--r-m);
  text-align: left;
  color: var(--text-3);
  transition: all var(--t-fast);
  border: 1px solid transparent;
}
.nav-item:hover { background: var(--bg-1); color: var(--text-1); }
.nav-item.on { background: var(--bg-2); border-color: var(--line-2); color: var(--text-1); }
.nav-item.on .ni-icon { color: var(--brand); background: var(--brand-tint); border-color: rgba(240, 169, 46, 0.24); }
.ni-icon {
  display: grid; place-items: center;
  width: 34px; height: 34px; flex: none;
  border-radius: var(--r-s);
  background: rgba(255, 255, 255, 0.05);
  border: 1px solid var(--line-1);
  color: var(--text-3);
}
.ni-text { display: flex; flex-direction: column; min-width: 0; }
.ni-label { font-size: var(--fs-14); font-weight: 600; }
.ni-desc { font-size: var(--fs-11); color: var(--text-4); }

.set-body { min-width: 0; }
.card-head { display: flex; align-items: flex-start; justify-content: space-between; gap: var(--sp-4); }
.add-panel { margin-top: var(--sp-4); padding-top: var(--sp-4); border-top: 1px solid var(--line-1); }

.type-cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: var(--sp-3); margin-bottom: var(--sp-4); }
.type-card {
  display: flex; flex-direction: column; gap: 4px; align-items: flex-start;
  padding: 14px;
  border-radius: var(--r-m);
  border: 1px solid var(--line-1);
  background: var(--bg-1);
  color: var(--text-3);
  transition: all var(--t-fast);
  text-align: left;
}
.type-card:hover { border-color: var(--line-3); color: var(--text-1); }
.type-card.on { border-color: var(--brand); background: var(--brand-tint); color: var(--text-1); }
.type-card.on svg { color: var(--brand); }
.tc-label { font-size: var(--fs-13); font-weight: 650; }

.btn-row { display: flex; gap: 10px; flex-wrap: wrap; }
.list { display: flex; flex-direction: column; gap: var(--sp-3); }
.row-card {
  display: flex; align-items: center; gap: 14px;
  padding: 14px 16px;
  border-radius: var(--r-m);
  border: 1px solid var(--line-1);
  background: var(--bg-1);
  transition: border-color var(--t-fast);
}
.row-card:hover { border-color: var(--line-2); }
.r-icon {
  display: grid; place-items: center;
  width: 40px; height: 40px; flex: none;
  border-radius: var(--r-m);
  background: rgba(255, 255, 255, 0.05);
  border: 1px solid var(--line-1);
  color: var(--brand);
}
.r-main { flex: 1; min-width: 0; }
.r-title { display: flex; align-items: center; gap: 8px; font-size: var(--fs-15); font-weight: 650; margin-bottom: 2px; }

.lib-card { padding: var(--sp-4) var(--sp-5); }
.lib-line { display: flex; align-items: center; gap: 14px; }

/* One row per storage source on the library form: pick a source, name a path
   inside it, and add as many rows as the library spans. */
.src-rows { display: flex; flex-direction: column; gap: 8px; align-items: stretch; }
.src-row { display: flex; gap: 8px; align-items: center; }
.src-row select { flex: 0 0 38%; min-width: 0; }
.src-row input { flex: 1 1 auto; min-width: 0; }
.src-row .btn { flex: 0 0 auto; }
.lib-actions { display: flex; gap: 8px; flex: none; flex-wrap: wrap; }
/* 封面:预览缩略图 + 选取图片 / 自动获取 / 恢复动态 三个按钮 */
.lib-poster { display: flex; gap: 12px; align-items: flex-start; margin-top: 10px; }
.lib-poster-thumb {
  flex: 0 0 auto; width: 76px; height: 108px; border-radius: var(--r-m);
  object-fit: cover; background: var(--bg-2); border: 1px solid var(--line-1);
}
.lib-poster-empty { display: flex; align-items: center; justify-content: center; text-align: center; padding: 6px; }
.lib-poster-actions { display: flex; flex-direction: column; gap: 8px; min-width: 0; }
.lib-poster-actions .btn-row { flex-wrap: wrap; }
/* 扫描/体检的进度显示由 LiveProgress 组件自己带样式 */
.scan-done { margin-top: 10px; display: flex; align-items: center; gap: 6px; color: var(--text-3); }
.scan-done svg { color: var(--ok); }
.scan-warn { color: var(--warn); }
.scan-err { margin-top: 10px; display: flex; align-items: center; gap: 7px; color: var(--danger); font-weight: 600; font-size: var(--fs-13); }

/* 字幕体检:摘要一行 + 按剧集聚合的缺字幕名单 */
.audit-sum { margin-top: 14px; border-top: 1px solid var(--line-1); padding-top: 12px; }
.audit-sum-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.audit-tiles { display: flex; gap: 16px; flex-wrap: wrap; margin-top: 9px; color: var(--text-3); }
.audit-tiles .num { color: var(--text-1); font-variant-numeric: tabular-nums; }
.audit-tiles .num.warn { color: var(--warn); }
.audit-tiles .dim, .audit-note { color: var(--text-4); }
.audit-body { margin-top: 14px; }
.audit-groups { display: flex; flex-direction: column; gap: 14px; }
.audit-group {
  border: 1px solid var(--line-1); border-radius: var(--r-m);
  padding: 11px 13px; background: var(--bg-2);
}
.audit-group-head { display: flex; align-items: center; gap: 9px; flex-wrap: wrap; }
.audit-show { font-weight: 650; font-size: var(--fs-14); color: var(--text-1); }
a.audit-show:hover { color: var(--brand); }
.badge.warn { color: var(--warn); border-color: rgba(251, 191, 36, 0.4); }
.audit-season { margin-top: 9px; }
.audit-season-head { color: var(--text-4); margin-bottom: 5px; }
.audit-season-head a:hover { color: var(--brand); }
.audit-eps { display: flex; gap: 6px; flex-wrap: wrap; }
.audit-ep {
  display: inline-flex; align-items: center; gap: 5px;
  padding: 3px 9px; border-radius: var(--r-pill);
  border: 1px solid var(--line-1); background: var(--bg-3);
  font-size: var(--fs-12); color: var(--text-2); white-space: nowrap;
}
.audit-ep:hover { border-color: var(--brand); color: var(--text-1); }
/* 还没确认完的(正在探测内嵌字幕轨)—— 可能随时从名单里消失 */
.audit-ep.pending { border-style: dashed; color: var(--text-4); }
.audit-ep.failed { border-color: rgba(255, 107, 107, 0.45); color: var(--danger); }
.audit-ep .dot-pending, .audit-ep .dot-failed { font-weight: 700; }
.audit-note { margin-top: 13px; line-height: 1.7; }
.audit-note b { color: var(--text-3); }

.type-picker { display: flex; gap: 8px; flex-wrap: wrap; }
.test-line { display: flex; align-items: center; gap: 10px; margin-top: 10px; flex-wrap: wrap; }
.inline-input { max-width: 220px; min-height: 36px; height: 36px; }
.ok-text { display: inline-flex; align-items: center; gap: 6px; color: #86efac; }
.err-text { display: inline-flex; align-items: center; gap: 6px; color: #ffb3b3; }

.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: var(--sp-3); }
.tile {
  display: flex; align-items: center; gap: 13px;
  padding: 15px 16px;
  border-radius: var(--r-m);
  border: 1px solid var(--line-1);
  background: linear-gradient(180deg, rgba(255, 255, 255, 0.035), rgba(255, 255, 255, 0)), var(--bg-1);
}
.tile.wide { grid-column: 1 / -1; }
.t-icon {
  display: grid; place-items: center;
  width: 38px; height: 38px; flex: none;
  border-radius: var(--r-m);
  background: rgba(255, 255, 255, 0.05);
  border: 1px solid var(--line-1);
  color: var(--text-3);
}
.t-icon.ok { color: var(--ok); background: var(--ok-soft); border-color: rgba(74, 222, 128, 0.28); }
.t-icon.err { color: var(--danger); background: var(--danger-soft); border-color: rgba(255, 107, 107, 0.3); }
.t-k { font-size: var(--fs-11); letter-spacing: 0.08em; text-transform: uppercase; color: var(--text-4); font-weight: 700; }
.t-v { font-size: var(--fs-14); font-weight: 600; }

.save-bar {
  position: sticky;
  bottom: calc(16px + env(safe-area-inset-bottom));
  display: flex; align-items: center; justify-content: space-between; gap: var(--sp-4);
  padding: 12px 18px;
  border-radius: var(--r-m);
  background: rgba(16, 18, 23, 0.94);
  border: 1px solid var(--line-2);
  box-shadow: var(--sh-3);
  backdrop-filter: blur(18px);
  z-index: 400;
}
.save-bar.flat { position: static; margin-top: var(--sp-4); background: var(--bg-0); box-shadow: none; }

/* ---------- users ---------- */
.reg-modes { display: grid; grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); gap: var(--sp-3); margin-top: var(--sp-4); }
.pending-card { border-color: rgba(251, 191, 36, 0.28); }

.user-card { display: flex; flex-direction: column; gap: var(--sp-3); }
/* A blocked or not-yet-approved account reads as inactive at a glance. */
.user-card.off { opacity: 0.72; }
.u-head { display: flex; align-items: center; gap: 12px; }
.av {
  display: grid; place-items: center;
  width: 38px; height: 38px; flex: none;
  border-radius: 50%;
  background: linear-gradient(150deg, #3d4450, #22262e);
  border: 1px solid var(--line-2);
  font-size: var(--fs-15);
  font-weight: 700;
  color: var(--text-1);
}
.u-main { flex: 1; min-width: 0; }
.u-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.email-line { display: flex; align-items: center; gap: 6px; margin-top: 3px; }

/* ---------- mail & captcha ---------- */
.preset-row { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; margin-top: var(--sp-4); }
.test-to { flex: 1; min-width: 200px; }
.warn-text { color: #fcd34d; }
.btn-row .input { min-height: 38px; }

.set-skeleton { padding-top: calc(var(--header-h) + 40px); }

/* ---------- B 站账号状态 ---------- */

/* The verdict line on a source card. Tone carries the meaning: a dead cookie is
   the one thing on this page that silently ruins playback, so it is drawn like a
   warning rather than as another line of grey text. */
.bili-health {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-top: 5px;
  font-size: 12.5px;
  line-height: 1.45;
}
.bili-health.ok { color: var(--ok); }
.bili-health.err { color: var(--danger); }
.bili-health.dim { color: var(--text-3); }

.qr-backdrop {
  position: fixed;
  inset: 0;
  z-index: 200;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: var(--sp-4);
  background: rgba(0, 0, 0, 0.62);
  backdrop-filter: blur(3px);
}
.qr-dialog {
  width: min(680px, 100%);
  max-height: calc(100vh - 2 * var(--sp-4));
  overflow-y: auto;
}
.qr-body {
  display: flex;
  gap: var(--sp-5);
  align-items: flex-start;
  padding: var(--sp-4) 0 0;
}
.qr-frame {
  flex: none;
  padding: 12px;
  border-radius: var(--r-m);
  /* White behind the code whatever the theme: a dark-on-dark QR does not scan. */
  background: #fff;
  transition: opacity 0.2s;
}
.qr-frame.dimmed { opacity: 0.35; }
.qr-frame img { display: block; border-radius: 4px; }
.qr-side { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 8px; }
.qr-state {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 13px;
  font-weight: 500;
}
.qr-state.ok { color: var(--ok); }
.qr-state.err { color: var(--danger); }
.qr-state.dim { color: var(--text-3); }
.qr-steps { margin: 4px 0 0; padding-left: 18px; line-height: 1.8; }

@media (max-width: 720px) {
  .qr-body { flex-direction: column; align-items: center; }
  .qr-side { width: 100%; }
}

@media (max-width: 1080px) {
  .set-layout { grid-template-columns: 1fr; gap: var(--sp-5); }
  .set-nav {
    position: static;
    flex-direction: row;
    overflow-x: auto;
    padding-bottom: 4px;
    gap: 6px;
    scrollbar-width: none;
  }
  .set-nav::-webkit-scrollbar { display: none; }
  .nav-item { flex: none; padding: 9px 13px; }
  .ni-desc { display: none; }
}
@media (max-width: 720px) {
  .lib-line { flex-wrap: wrap; }
  .lib-actions { width: 100%; }
  .settings-page { padding-bottom: calc(96px + env(safe-area-inset-bottom)); }
}
</style>
