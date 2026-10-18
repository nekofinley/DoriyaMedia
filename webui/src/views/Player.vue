<script setup>
import { ref, onMounted, onBeforeUnmount, computed, nextTick, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, detectClient, fmtTime, fmtSize, imgUrl, parseGenres } from '../api.js'
import AppIcon from '../components/AppIcon.vue'
import SmartImage from '../components/SmartImage.vue'
import ToastHost from '../components/ToastHost.vue'
import { toast } from '../toast.js'
import { createDanmaku } from '../danmaku.js'

const route = useRoute()
const router = useRouter()

const play = ref(null)
const error = ref('')
const fatal = ref('')
const videoEl = ref(null)
const stageEl = ref(null)
const playerEl = ref(null)
let cueObs = null
let dmObs = null
const watchEl = ref(null)
const hls = ref(null)
const currentAudio = ref(0)
const activeSub = ref(null)
// streamStart: where the current stream begins in file time. Non-zero when a
// packaging session started at the viewer's saved position, because players
// present that stream as starting at zero.
const streamStart = ref(0)
// nativeSubs: the stream carries its subtitles as HLS renditions, so the player
// manages them (its own menu and its own default) and the page must not attach
// <track> elements as well, or both would render at once.
const nativeSubs = computed(() => !!play.value?.native_subtitles)

/* --------------------------------------------------------------- 弹幕
 *
 * A source that publishes bullet comments (bilibili) hands the player a URL in
 * the play payload. They are drawn on a canvas over the picture, in file time —
 * the same clock the subtitles and the progress bar use — so the layer keeps
 * working when a stream starts part-way into the file.
 */
const dmCanvas = ref(null)
const danmakuOn = ref(localStorage.getItem('dm:dmOn') !== '0')
const danmakuOpacity = ref(Number(localStorage.getItem('dm:dmOpacity') ?? 0.9))
const danmakuSpeed = ref(Number(localStorage.getItem('dm:dmSpeed') ?? 8))
const danmakuArea = ref(Number(localStorage.getItem('dm:dmArea') ?? 0.6))
const danmakuCount = ref(0)
const danmakuFailed = ref(false)
const danmakuUrl = computed(() => play.value?.danmaku_url || '')
/** Ready means there is something to draw; the control hides itself otherwise. */
const danmakuReady = computed(() => !!danmakuUrl.value && danmakuCount.value > 0 && !danmakuFailed.value)
let danmaku = null // the renderer
let dmRaf = 0
let dmLoadedFor = '' // the URL the current set belongs to
// Text tracks whose formatting WebVTT can only approximate (ASS typesetting:
// signs placed to the pixel, karaoke, animation) can be burned into the picture
// instead. On iOS that is the only way to see the original layout at all, since
// the native player renders the approximation and nothing better.
const burnables = computed(() => (play.value?.subtitles || []).filter((s) => s.burn_available))
const duration = ref(0)
const position = ref(0)
const buffered = ref(0)
const isLoading = ref(true)
const isPlaying = ref(false)
const muted = ref(localStorage.getItem('dm:muted') === '1')
const volume = ref(Number(localStorage.getItem('dm:volume') ?? 1))
const rate = ref(Number(localStorage.getItem('dm:rate') ?? 1))
const controlsVisible = ref(true)
const panelOpen = ref(false)
/** Which face the drawer shows: the subtitles, or everything else. */
const panelMode = ref('settings')
const menuOpen = ref(false)
// locked: in fullscreen, every control but the lock button stops answering taps,
// so a hand moving across the screen cannot pause, seek or leave by accident.
const locked = ref(false)
const fsActive = ref(false)
const pipActive = ref(false)
const scrubbing = ref(false)
const scrubTime = ref(0)
const hoverTime = ref(null)
const hoverX = ref(0)
const forceMode = ref('')      // current override, reapplied when re-fetching
// burnSub: the subtitle id being overlaid onto the video. Burning re-encodes, so
// it is opt-in, and it changes the stream identity — hence a reload, not a toggle.
const burnSub = ref(0)
const probeElapsed = ref(0)    // seconds the background probe has been running
const stallSeconds = ref(0)    // seconds spent waiting on data mid-playback
const streamStatus = ref(null) // segment-generation progress from the server

const isIOS = computed(() => {
  const ua = navigator.userAgent
  return /iPhone|iPad|iPod/.test(ua) || (/Macintosh/.test(ua) && 'ontouchend' in document)
})
/** iOS gets the native control surface; everything else gets the custom chrome. */
const nativeControls = computed(() => isIOS.value)

/* ----------------------------------------------------- which player is this?
 *
 * Three layouts, picked from what the device *is* rather than from how wide the
 * window happens to be:
 *   theater — the immersive full-screen player: desktop, and a phone held
 *             sideways, where a stacked page would only waste the picture;
 *   phone   — the YouTube/Bilibili portrait page: picture on top, its buttons
 *             and the episode grid underneath;
 *   tablet  — picture top-left, its buttons below it, episodes in a right
 *             sidebar.
 * A touch-only device (no hover, coarse pointer) is what tells a tablet apart
 * from a desktop window of the same width.
 */
const vp = ref({ w: window.innerWidth, h: window.innerHeight })
function onViewport() {
  vp.value = { w: window.innerWidth, h: window.innerHeight }
  // The picture's box moved with the window, and so did the place the cues belong.
  fitCuesToPicture()
}

const touchOnly = (() => {
  try { return window.matchMedia('(hover: none) and (pointer: coarse)').matches } catch { return false }
})()

/** ?view=phone|tablet|theater pins a layout — for testing, and stubborn devices. */
const forcedView = computed(() => {
  const v = String(route.query.view || '')
  return ['phone', 'tablet', 'theater'].includes(v) ? v : ''
})

const layout = computed(() => {
  if (forcedView.value) return forcedView.value
  if (!touchOnly) return 'theater'
  const short = Math.min(vp.value.w, vp.value.h)
  if (short >= 600) return 'tablet'
  return vp.value.w > vp.value.h ? 'theater' : 'phone'
})
/** Stacked: the picture is one block in a page, with everything else under it. */
const isStacked = computed(() => layout.value !== 'theater')
const layoutClass = computed(() => `lay-${layout.value}`)
/** A touch screen gets double-tap-to-pause; a mouse keeps click-to-pause. */
const touchPlayer = computed(() => touchOnly && !nativeControls.value)

/* The immersive chrome fades out while playing; a watch page keeps its buttons,
 * because a back button that vanishes on a page is just a lost back button.
 * A fullscreen stage counts as a theater even when the page behind it is not. */
const theaterChrome = computed(() => !isStacked.value || fsActive.value)
/* Who gets a bar over the picture. The tablet watch page does not: iPadOS puts
 * its own fullscreen control in the picture's top-left corner, and a back button
 * on top of it is a mis-tap waiting to happen — that page keeps its 返回 in the
 * text below instead. */
const stageBar = computed(() => theaterChrome.value || layout.value === 'phone')
const chromeHidden = computed(() => theaterChrome.value && !controlsVisible.value &&
  isPlaying.value && !menuOpen.value && !panelOpen.value)

const modeClass = computed(() => `mode-${play.value?.mode || 'direct'}`)
const MODE_LABELS = { direct: '302 直连', proxy: '代理播放', remux: 'HLS 转封装', unsupported: '无法播放' }
/** The server refused the file itself: there is nothing to attach, only a reason. */
const unplayableFile = computed(() => play.value?.mode === 'unsupported')
function candName(c) { return MODE_LABELS[c?.mode] || c?.mode || '' }
const modeLabel = computed(() => candName(play.value))

const currentSize = computed(() => {
  const v = (play.value?.versions || []).find((x) => x.current)
  return v?.size ? fmtSize(v.size) : ''
})
const shownTime = computed(() => (scrubbing.value ? scrubTime.value : position.value))
const progressPct = computed(() => (duration.value > 0 ? Math.min(100, (shownTime.value / duration.value) * 100) : 0))
const bufferedPct = computed(() => (duration.value > 0 ? Math.min(100, (buffered.value / duration.value) * 100) : 0))
const volumePct = computed(() => (muted.value ? 0 : volume.value * 100))

/** Seconds of timeline the player needs buffered before it will start. */
const streamNeeded = 12

const streamPct = computed(() => {
  const s = streamStatus.value
  if (!s || !s.generated_seconds) return 0
  return Math.min(100, (s.generated_seconds / streamNeeded) * 100)
})

/** Timeline seconds per wall second: below 1 the server cannot keep up. */
const streamRate = computed(() => {
  const r = streamStatus.value?.realtime_factor
  return r && isFinite(r) ? r : null
})

const encoderTooSlow = computed(() => {
  const r = streamRate.value
  return r !== null && r < 0.9 && stallSeconds.value >= 20
})

/** What the spinner should say — never leave the user guessing. */
const loadingText = computed(() => {
  if (play.value?.state === 'probing') {
    return `正在探测媒体信息(容器 / 编码 / 音轨)… ${probeElapsed.value.toFixed(0)}s`
  }
  const s = streamStatus.value
  if (s && s.generated_seconds > 0) {
    const rate = streamRate.value
    const speed = rate !== null ? ` · 编码速度 ${rate.toFixed(2)}× 实时` : ''
    return `正在生成分片 · 已就绪 ${s.generated_seconds.toFixed(0)}s / ${streamNeeded}s${speed}`
  }
  if (stallSeconds.value >= 3) return `正在缓冲… 已等待 ${stallSeconds.value.toFixed(0)}s`
  return modeLabel.value ? `正在准备播放 · ${modeLabel.value}` : '正在准备播放…'
})
/** Long enough that the source is clearly the problem, not a slow start. */
const slowHint = computed(() => play.value?.state !== 'probing' && stallSeconds.value >= 25)
const rates = [0.5, 0.75, 1, 1.25, 1.5, 2]
const forceModes = [
  { value: '', label: '自动决策', hint: '按客户端能力自动选择' },
  { value: 'direct', label: '强制 302 直连', hint: '不经过服务器,最省带宽' },
  { value: 'proxy', label: '强制代理', hint: '服务器中转,支持拖动' },
  { value: 'remux', label: '强制 HLS 转封装', hint: '视频直拷,只转不兼容音轨' },
  // There is no "强制转码": re-encoding video is not offered at all, so a mode
  // that would start one has no place in the list.
]

/* --------------------------------------------------------- episodes (选集) */

// The play decision answers "what do I play", not "what else is on here", so the
// siblings are fetched separately: episode → its season → that season's episodes.
// Only the watch-page layouts ever ask, so a desktop plays exactly as before.
const queue = ref(null)   // { season, show, episodes }
const meta = ref(null)    // the item behind the current file (overview, air date…)
let queueFor = 0          // item id the queue was loaded for
let queueBusy = false
let queueAgain = false

async function loadQueue() {
  const itemId = play.value?.item_id
  if (!isStacked.value || !itemId || queueFor === itemId) return
  if (queueBusy) { queueAgain = true; return }
  queueBusy = true
  try {
    const detail = await api.get(`/api/items/${itemId}`)
    const it = detail?.item
    if (!it || disposed) return
    meta.value = it
    queueFor = itemId
    if (it.type === 'episode' && it.parent_id) {
      const season = await api.get(`/api/items/${it.parent_id}`)
      if (disposed || play.value?.item_id !== itemId) return
      queue.value = (season?.episodes || []).length
        ? { season: season.item, show: season.show || null, episodes: season.episodes }
        : null
    } else if (it.type === 'season' && (detail.episodes || []).length) {
      queue.value = { season: it, show: detail.show || null, episodes: detail.episodes }
    } else {
      queue.value = null
    }
  } catch {
    // A file played straight out of the browser has no show behind it.
    queue.value = null
  } finally {
    queueBusy = false
    if (queueAgain) { queueAgain = false; loadQueue() }
  }
}

const episodes = computed(() => queue.value?.episodes || [])
const epIndex = computed(() => episodes.value.findIndex((e) => e.id === play.value?.item_id))
const prevEp = computed(() => (epIndex.value > 0 ? episodes.value[epIndex.value - 1] : null))
const nextEp = computed(() => {
  const i = epIndex.value
  return i >= 0 && i + 1 < episodes.value.length ? episodes.value[i + 1] : null
})

function epFile(ep) { return ep?.file_id || (ep?.versions || [])[0]?.id || 0 }
function epNo(ep, i) { return ep?.episode || i + 1 }
function epName(ep, i) { return ep?.name || `第 ${epNo(ep, i)} 集` }
function epSub(ep) {
  const parts = []
  if (ep?.air_date) parts.push(ep.air_date)
  const n = (ep?.versions || []).length
  if (n > 1) parts.push(`${n} 个版本`)
  return parts.join(' · ')
}

/** Switch episode. Replaces the history entry, so back still leaves the player. */
function playEpisode(ep) {
  const id = epFile(ep)
  if (!id || id === Number(route.params.fileId)) return
  saveProgress()
  router.replace(`/play/${id}`)
  // Picking an episode from the grid would otherwise leave the viewer looking at
  // the list, with the picture they just chose scrolled off the top.
  watchEl.value?.scrollTo({ top: 0, behavior: 'smooth' })
}

/* The block under the picture is tabbed: the episode grid (phone only — a tablet
 * keeps them in the sidebar), a synopsis, and the track/mode settings that the
 * theater keeps in its drawer. */
const tab = ref('')
const tabs = computed(() => {
  const out = []
  if (layout.value === 'phone' && episodes.value.length) out.push('eps')
  out.push('about', 'settings')
  return out
})
const activeTab = computed(() => (tabs.value.includes(tab.value) ? tab.value : tabs.value[0]))
/** The panel as a block *in* the page — the 设置 tab — rather than a drawer over
 * the picture. Only a watch page has somewhere to put it: fullscreen turns any
 * layout back into a theater, where the panel is the right-hand drawer again, and
 * there it needs the drawer's own manners (a close button, a slide in). */
const panelInline = computed(() => isStacked.value && !fsActive.value)
const panelShown = computed(() => (panelInline.value ? activeTab.value === 'settings' : panelOpen.value))

const infoShow = computed(() => queue.value?.show || null)
const infoGenres = computed(() => parseGenres(infoShow.value || meta.value))
const overview = computed(() => {
  const it = meta.value
  const sh = infoShow.value
  return (it?.type === 'episode' ? it.overview : '') || sh?.overview || it?.overview || ''
})

/** The drawer has two faces and one body, the subtitle button opening the tracks
 * and the gear opening the rest. Both used to open the same list, so asking for
 * the subtitles in fullscreen produced the whole settings sheet — which is the
 * thing this separates. On a watch page the two are still one tab, in order. */
function openPanel(mode) {
  if (panelInline.value) { tab.value = 'settings'; menuOpen.value = false; return }
  if (panelOpen.value && panelMode.value === mode) { panelOpen.value = false; return }
  panelMode.value = mode
  menuOpen.value = false
  panelOpen.value = true
}
/** 'all' where the panel is a watch-page tab, otherwise the face that was asked for. */
const drawerMode = computed(() => (panelInline.value ? 'all' : panelMode.value))
/** The gear: the settings face. */
function togglePanel() { openPanel('settings') }
/** Which surface the control bar's gear opens. A watch page has no room for a
 * drawer over the picture, so it jumps to the 设置 tab; a touch screen gets the
 * same right-hand drawer the subtitle button opens, because the small popup was
 * only ever a shorter copy of that drawer — and on a phone it is the bad copy:
 * no hover to read the mode hints with, a quarter of the screen to scroll, and a
 * tap target competing with the picture behind it. A mouse keeps the popup, where
 * it is the quick way to the speed and mode. */
function toggleMenu() {
  if (panelInline.value) { tab.value = 'settings'; return }
  if (touchOnly) { openPanel('settings'); return }
  menuOpen.value = !menuOpen.value
}
function setMode(m) {
  menuOpen.value = false
  loadDecision(m.value || undefined)
}

// Rotating a phone into portrait (or back) swaps layouts without a reload, so the
// watch page has to pick up its episode list the moment it appears.
watch(isStacked, (stacked) => {
  if (stacked) { panelOpen.value = false; loadQueue() }
})

let lastSave = 0
let hideTimer = null
let probeTimer = null
let streamTimer = null
let stallTimer = null
let stallSince = 0
// Where the playhead was when we last looked, and when it last advanced: these
// are what decide whether the picture is moving (see markPlaying).
let tickAt = 0
let tickMovedAt = 0
let disposed = false

/* ------------------------------------------------------------------ playback */

async function loadDecision(force) {
  error.value = ''
  fatal.value = ''
  isLoading.value = true
  forceMode.value = force || ''
  stopProbePoll()
  try {
    play.value = await fetchPlay()
  } catch (e) {
    fatal.value = e.message
    isLoading.value = false
    return
  }
  // Where the stream begins in file time (0 unless a session packages from a
  // resume position). Everything the UI and the API show is file time, so this is
  // the offset between the element's clock and the file's.
  streamStart.value = Number(play.value?.stream_start) || 0
  // The episode list does not depend on the decision, so ask for it in parallel
  // with the (slow) probe rather than after it.
  loadQueue()
  if (unplayable()) return
  if (play.value?.state === 'probing') {
    startProbePoll()
    return
  }
  await nextTick()
  attachSource()
}

/**
 * The server decided nothing here can play this file (its video codec has no
 * decoding path on this client, and re-encoding is disabled). There is no source
 * to attach, so the reason becomes the message and the player stops there.
 */
function unplayable() {
  if (!unplayableFile.value) return false
  fatal.value = play.value?.reason || '这个文件在当前设备上无法播放'
  error.value = ''
  isLoading.value = false
  return true
}

// Ask the browser what it can actually decode, rather than assuming. A machine
// with hardware HEVC support answers yes, and the server then copies the video
// instead of refusing the file; a machine that answers no is never handed HEVC it
// would only fail to decode. The report is a result either way, because
// "no answer" means "unknown" and must keep the old behaviour.
//
// Audio is asked the same way, for the same reason: AC-3, E-AC-3 and DTS are
// decoded by the operating system rather than the browser, so the server's codec
// tables can only guess — Windows and Android usually have those decoders, Linux
// usually does not, and the two look identical from the server. A codec reported
// here may play direct; one that is not mentioned keeps the table's verdict
// (which is usually "remux it so there is sound").
const AUDIO_CAPS = [
  ['ac3', ['ac-3']],
  ['eac3', ['ec-3']],
  ['dts', ['dtsc', 'dtsh', 'dtsl', 'dtse', 'dtsx']],
  ['truehd', ['mlpa']],
  ['flac', ['flac']],
  ['opus', ['opus']],
  ['vorbis', ['vorbis']],
]

const clientCaps = (() => {
  try {
    if (typeof MediaSource === 'undefined' || !MediaSource.isTypeSupported) return ''
    const caps = []
    const hevc = [
      'video/mp4; codecs="hvc1.1.6.L123.B0"', // Main
      'video/mp4; codecs="hvc1.2.4.L153.B0"', // Main 10
    ].some((t) => MediaSource.isTypeSupported(t))
    caps.push(hevc ? 'hevc' : 'nohevc')
    for (const [name, codecs] of AUDIO_CAPS) {
      if (codecs.some((c) => MediaSource.isTypeSupported(`audio/mp4; codecs="${c}"`))) caps.push(name)
    }
    return caps.join(',')
  } catch {
    return ''
  }
})()

function fetchPlay() {
  const params = new URLSearchParams({ client: detectClient() })
  if (forceMode.value) params.set('force', forceMode.value)
  if (burnSub.value) params.set('burn', String(burnSub.value))
  if (currentAudio.value) params.set('a', String(currentAudio.value))
  if (clientCaps) params.set('caps', clientCaps)
  return api.get(`/api/play/${route.params.fileId}?${params}`)
}

/*
 * Probing is the slowest step of starting playback and happens in the
 * background on the server, so poll its state instead of hanging on one
 * request. This is what turns an opaque spinner into "探测媒体信息… 12s".
 */
function startProbePoll() {
  stopProbePoll()
  probeElapsed.value = 0
  const started = Date.now()
  let idleRetries = 0

  const tick = async () => {
    if (disposed) return
    probeElapsed.value = (Date.now() - started) / 1000
    let st = null
    try { st = await api.get(`/api/play/${route.params.fileId}/status`) } catch { /* transient */ }
    if (disposed) return

    if (st && st.state === 'failed') {
      fatal.value = `媒体探测失败:${st.error || '未知原因'}`
      isLoading.value = false
      return
    }
    if (st && st.state === 'ready') {
      try { play.value = await fetchPlay() } catch (e) { fatal.value = e.message; isLoading.value = false; return }
      if (disposed) return
      loadQueue()
      if (unplayable()) return
      if (play.value?.state === 'probing') { probeTimer = setTimeout(tick, 800); return }
      await nextTick()
      attachSource()
      return
    }
    // idle: no job registered anymore (server restart, or a failed probe inside
    // its cooldown). Ask /api/play again to (re)start it, but never loop.
    if (st && st.state === 'idle') {
      if (++idleRetries > 2) {
        fatal.value = '媒体探测未能启动,请重试'
        isLoading.value = false
        return
      }
      try { play.value = await fetchPlay() } catch { /* handled below */ }
      if (disposed) return
      if (play.value?.url) { await nextTick(); attachSource(); return }
      if (play.value?.state !== 'probing') {
        fatal.value = '媒体探测未能启动,请重试'
        isLoading.value = false
        return
      }
    }
    probeTimer = setTimeout(tick, probeElapsed.value < 5 ? 700 : 1500)
  }
  probeTimer = setTimeout(tick, 500)
}

function stopProbePoll() {
  if (probeTimer) { clearTimeout(probeTimer); probeTimer = null }
}

/*
 * Segment generation progress. Preparing HLS can take a minute (the server must
 * reach the source, seek, decode and re-encode every 4s of video), so poll the
 * status endpoint and show how much timeline is ready plus the encoder's
 * realtime factor — which is what tells the user whether it will ever catch up.
 */
function startStreamPoll() {
  stopStreamPoll()
  const tick = async () => {
    if (disposed) return
    // The probe poller already owns the endpoint while a probe is running.
    if (play.value?.state !== 'probing') {
      let st = null
      try { st = await api.get(`/api/play/${route.params.fileId}/status`) } catch { /* transient */ }
      if (disposed) return
      if (st && st.stream) streamStatus.value = st.stream
      else if (st && !st.stream && !isLoading.value) streamStatus.value = null
    }
    streamTimer = setTimeout(tick, isLoading.value ? 1200 : 5000)
  }
  streamTimer = setTimeout(tick, 800)
}

function stopStreamPoll() {
  if (streamTimer) { clearTimeout(streamTimer); streamTimer = null }
}

/* Stall clock: while the player waits on data, show how long we have waited —
   that is the difference between "still loading" and "dead".

   The playhead has the last word on whether the player is loading, because no
   event is evidence about the picture. `waiting` says data ran short, not that
   the picture stopped; and on a slow connection readyState sits at
   HAVE_CURRENT_DATA (2) for a whole film while frames keep arriving one at a
   time, so a `readyState >= 3` test never passes there. That is how a spinner
   ends up turning over a picture that is plainly playing, and how the stall clock
   reaches the "switch mode?" hint with nothing actually wrong. So the state is
   claimed only once the playhead has really stopped, and any advance clears it
   (markPlaying). */
function onWaiting() {
  if (stallSince) return
  stallSince = Date.now()
  stallSeconds.value = 0
  clearInterval(stallTimer)
  stallTimer = setInterval(() => {
    stallSeconds.value = (Date.now() - stallSince) / 1000
    const v = videoEl.value
    // Paused and short of data is genuinely preparing; playing is only preparing
    // once the playhead has stopped moving.
    if (v && (v.paused || !playheadMoving())) isLoading.value = true
  }, 500)
}

function onPlaying() {
  isLoading.value = false
  clearStall()
}

// Enough data has arrived to keep playing on its own. This settles the loading
// state whatever the earlier events said, and it is the one signal that also
// works while the element is paused (a viewer who pauses mid-stall should not be
// left looking at "正在缓冲" after the data has turned up).
function onCanPlay() {
  const v = videoEl.value
  if (!v || v.readyState < 3) return
  isLoading.value = false
  clearStall()
}

/** Has the playhead advanced in the last moment? The events lie; the clock does not. */
function playheadMoving() {
  return tickMovedAt > 0 && Date.now() - tickMovedAt < 1000
}

/** Frames are moving: nothing is loading, whatever the element's events said. */
function markPlaying() {
  const v = videoEl.value
  if (!v) return
  const t = v.currentTime
  const advanced = Math.abs(t - tickAt) > 0.002
  tickAt = t
  if (v.paused || !advanced || !(t > 0)) return
  tickMovedAt = Date.now()
  isLoading.value = false
  clearStall()
}

function clearStall() {
  stallSince = 0
  stallSeconds.value = 0
  if (stallTimer) { clearInterval(stallTimer); stallTimer = null }
}

const cands = ref([])
const candIdx = ref(-1)
let HlsLib = null
let mediaRecovered = false
let netRestarted = false

/* One candidate is not abandoned lightly.
 *
 * An error means "this strategy is wrong" only sometimes. From inside the page a
 * slow start and a broken mode look the same: the first segments of a cloud file
 * take tens of seconds to produce, and a player gives up on the manifest long
 * before the server gives up packaging it. Falling down the chain there is
 * exactly how an iOS remux used to turn into a 302 of a Matroska file — the one
 * thing that device can never open. So a failure has to earn the switch: a decode
 * error (the bytes arrived and were refused) switches at once, everything else
 * gets another attempt at the same mode for as long as the server is visibly
 * working on it.
 */
const RETRY_DELAYS = [2000, 5000, 10000]
const HLS_DWELL_MS = 15000     // an HLS attempt is not judged before this
const CAND_PATIENCE_MS = 60000 // ...nor retried forever
let candTries = 0      // retries spent on the current candidate
let candFor = -1       // the candidate they belong to (-1: none yet)
let candSince = 0      // when it was attached
let candMeta = false   // the element reported metadata for it
let switchTimer = null // pending retry, so two error events cannot race
let errHandledAt = 0   // when a failure was last acted on (one failure, two events)

// 候选链:主决策 + 后端下发的备用方案(直连/代理/转封装)。
// 后端只会下发这个客户端真的能用的方案,并已按"便宜→贵"排好序。
function buildCandidates() {
  const p = play.value
  const list = []
  const push = (mode, url, kind) => {
    if (!url) return   // an unplayable decision carries no stream and no chain
    list.push({ mode, url, kind: kind || (url.includes('/api/hls/') ? 'hls' : 'mp4') })
  }
  push(p.mode, p.url, p.kind)
  for (const a of (p.alternates || [])) push(a.mode, a.url)
  return list
}

/** A retry must not be answered out of the browser's cache of the failure. */
function bust(url, n) {
  if (!n || !url) return url
  return url + (url.includes('?') ? '&' : '?') + 'r=' + n
}

function destroyHls() {
  if (hls.value) { hls.value.destroy(); hls.value = null }
}

async function attachSource() {
  cands.value = buildCandidates()
  candFor = -1
  candTries = 0
  candMeta = false
  await startCand(0)
}

async function startCand(i) {
  clearTimeout(switchTimer)
  switchTimer = null
  if (i >= cands.value.length) {
    isLoading.value = false
    const tried = cands.value.map(candName).join(' → ')
    fatal.value = `所有播放方式都失败了(已尝试:${tried})。可以换一条音轨,或手动指定一种方式重试。`
    return
  }
  if (candFor !== i) { candFor = i; candTries = 0; candMeta = false }
  candSince = Date.now()
  candIdx.value = i
  const c = cands.value[i]
  const src = bust(c.url, candTries)
  if (play.value) play.value = { ...play.value, mode: c.mode, url: src, kind: c.kind }
  const v = videoEl.value
  if (!v) return
  destroyHls()
  mediaRecovered = false
  netRestarted = false
  startStreamPoll()
  v.removeAttribute('src')
  activeSub.value = null

  if (c.kind === 'hls') {
    const canNative = v.canPlayType('application/vnd.apple.mpegurl')
    if (canNative) {
      v.src = src
    } else {
      if (!HlsLib) HlsLib = (await import('hls.js')).default
      const Hls = HlsLib
      if (Hls.isSupported()) {
        hls.value = new Hls({
          maxBufferLength: 30,
          manifestLoadingTimeOut: 30000,
          manifestLoadingMaxRetry: 4,
          levelLoadingTimeOut: 30000,
          fragLoadingTimeOut: 120000,
          fragLoadingMaxRetry: 8,
          fragLoadingRetryDelay: 1000,
          nudgeMaxRetry: 10,
        })
        hls.value.loadSource(src)
        hls.value.attachMedia(v)
        hls.value.on(Hls.Events.ERROR, onHlsError)
      } else {
        await startCand(i + 1)
        return
      }
    }
  } else {
    v.src = src
  }

  applyVolume()
  v.playbackRate = rate.value
  v.load()
  v.play().catch(() => { /* autoplay may be blocked — user presses play */ })
  showControls()
}

/** The server says it is still producing this stream. */
function serverPreparing() {
  const s = streamStatus.value
  if (!s) return false
  if (Number(s.segments_ready) > 0) return false   // there is something to play
  // No segment out yet and the packager was asked for one recently: it is working.
  return !(Number(s.idle_seconds) > 10)
}

/** Is this failure "not ready yet" rather than "the wrong mode"? */
function keepWaiting(code) {
  const c = cands.value[candIdx.value]
  if (!c || candTries >= RETRY_DELAYS.length) return false
  const waited = Date.now() - candSince
  // The patience window is about *starting*: a stream that never produced a
  // picture and has been retried for a minute is not going to. One that was
  // already playing is a different case — a hiccup mid-playback is worth another
  // attempt however long it has been running, since a mode switch reloads the
  // whole stream (and jumps back to the last saved position).
  if (!candMeta && waited > CAND_PATIENCE_MS) return false
  // A fresh attempt swallowing the previous one's abort is not a failure.
  if (code === 1 && waited < 1500) return false
  // Decode error: the data arrived and the decoder refused it — believe it.
  if (code === 3) return false
  // Network-flavoured errors are worth another attempt whatever the mode.
  if (code === 1 || code === 2) return true
  // SRC_NOT_SUPPORTED is ambiguous: a native-HLS player (iOS) reports it for a
  // manifest whose first segments are still being produced, and that is a verdict
  // on nothing. While the server is packaging, wait; and an HLS attempt gets a
  // minimum dwell either way, because its first segment is the slowest thing here.
  if (c.kind !== 'hls' || candMeta) return false
  return serverPreparing() || waited < HLS_DWELL_MS
}

function retryCand(code) {
  candTries++
  const c = cands.value[candIdx.value]
  const why = (code === 1 || code === 2) ? '网络中断' : '服务器还在生成分片'
  toast.info(`${why},继续用「${candName(c)}」重试(${candTries}/${RETRY_DELAYS.length})…`, 2400)
  isLoading.value = true
  clearStall()
  const i = candIdx.value
  clearTimeout(switchTimer)
  switchTimer = setTimeout(() => {
    switchTimer = null
    if (!disposed && candIdx.value === i) startCand(i)
  }, RETRY_DELAYS[candTries - 1] || 10000)
}

function onHlsError(_, data) {
  if (!data.fatal) return
  if (data.type === HlsLib.ErrorTypes.MEDIA_ERROR && !mediaRecovered) {
    mediaRecovered = true
    hls.value?.recoverMediaError()
    return
  }
  if (data.type === HlsLib.ErrorTypes.NETWORK_ERROR && !netRestarted) {
    netRestarted = true
    hls.value?.startLoad()
    return
  }
  // hls.js has spent its own retries on this; the question is still whether the
  // mode is wrong or the server is merely slow.
  const code = Number(videoEl.value?.error?.code) || 0
  if (keepWaiting(code)) { errHandledAt = Date.now(); retryCand(code); return }
  toast.info(`${modeLabel.value}失败,切换下一种播放方式…`)
  errHandledAt = Date.now()
  startCand(candIdx.value + 1)
}

function onLoadedMetadata() {
  // Metadata is not "ready": a stream still waiting for its first segments keeps
  // its loading state (and its bar) until there is enough to play, or until
  // frames actually move.
  if (videoEl.value && videoEl.value.readyState >= 3) isLoading.value = false
  candMeta = true
  const v = videoEl.value
  // Everything here is reported in *file* time. A packaged session begins where
  // the viewer's saved position was, and players present that stream as starting
  // at zero (hls.js re-aligns it), so the element's own clock is offset from the
  // file by streamStart.
  const elDuration = Number(v.duration) || 0
  // The payload's duration is already the whole file's, so only the element's own
  // (remaining) duration gets the offset added.
  duration.value = elDuration > 0 ? elDuration + streamStart.value : (play.value?.duration || 0)
  position.value = (Number(v.currentTime) || 0) + streamStart.value
  // Subtitles come on by themselves; the panel is there to change or turn them off.
  autoSelectSub()
  api.get(`/api/progress?file_id=${route.params.fileId}`).then((p) => {
    const inStream = p ? p.position - streamStart.value : 0
    if (p && p.position > 30 && duration.value > 0 && p.position < duration.value * 0.95) {
      v.currentTime = Math.max(0, inStream)
      position.value = p.position
      toast.info(`已从 ${fmtTime(p.position)} 继续播放`)
    }
  }).catch(() => {})
}

function onErr() {
  isLoading.value = false
  if (!play.value) return
  // One failure can arrive twice: hls.js reports it, and the element fires error
  // for the same underlying cause. Acting twice would skip a candidate.
  const at = Date.now()
  if (at - errHandledAt < 500) return
  errHandledAt = at
  // Not every error is a verdict on the mode: a slow first segment looks exactly
  // like a broken one from here, and switching modes on it is how a working
  // strategy gets replaced by one this client can never play.
  const code = Number(videoEl.value?.error?.code) || 0
  if (keepWaiting(code)) { retryCand(code); return }
  if (candIdx.value < cands.value.length - 1) {
    toast.info('该方式播放失败,自动切换…')
    startCand(candIdx.value + 1)
    return
  }
  const tried = cands.value.map(candName).join(' → ')
  error.value = `播放失败(解码或网络错误)。已尝试:${tried}`
}

function onTimeUpdate() {
  const v = videoEl.value
  if (!v) return
  // Every tick is proof the picture is moving — the moment it starts, the
  // loading state goes away and stays away until it really stalls.
  markPlaying()
  // The element's clock is relative to the stream; everything the UI and the API
  // deal in is file time, so the session offset is added on the way out.
  if (!scrubbing.value) position.value = v.currentTime + streamStart.value
  if (v.buffered.length) buffered.value = v.buffered.end(v.buffered.length - 1) + streamStart.value
  // A paused picture draws no frames of its own, so a seek while paused has to
  // be painted from here.
  if (v.paused) paintDanmaku()
  const now = Date.now()
  if (now - lastSave > 10000) { lastSave = now; saveProgress() }
}

// ---------- 弹幕 ----------

/** The layer draws in file time, like every other clock the page shows. */
function danmakuTime() {
  const v = videoEl.value
  return v ? v.currentTime + streamStart.value : 0
}

function ensureDanmaku() {
  if (danmaku || !dmCanvas.value) return
  danmaku = createDanmaku(dmCanvas.value)
  danmaku.setOptions({
    opacity: danmakuOpacity.value, speed: danmakuSpeed.value, area: danmakuArea.value,
  })
}

function applyDanmakuOptions() {
  danmaku?.setOptions({
    opacity: danmakuOpacity.value, speed: danmakuSpeed.value, area: danmakuArea.value, force: true,
  })
  paintDanmaku()
}

/** One frame, for the moments when nothing is moving (a seek, a pause). */
function paintDanmaku() {
  if (!danmaku || !danmakuOn.value || !danmakuReady.value) return
  danmaku.draw(danmakuTime())
}

function dmFrame() {
  dmRaf = 0
  const v = videoEl.value
  if (!v || !danmaku || !danmakuOn.value || !danmakuReady.value) return
  danmaku.draw(danmakuTime())
  // A paused picture needs no frames; the next play or seek paints again.
  if (!v.paused && !v.ended) dmRaf = requestAnimationFrame(dmFrame)
}

function kickDanmaku() {
  if (dmRaf || !danmakuOn.value || !danmakuReady.value) return
  dmRaf = requestAnimationFrame(dmFrame)
}

function stopDanmaku() {
  if (dmRaf) cancelAnimationFrame(dmRaf)
  dmRaf = 0
}

function toggleDanmaku() {
  danmakuOn.value = !danmakuOn.value
  localStorage.setItem('dm:dmOn', danmakuOn.value ? '1' : '0')
  if (danmakuOn.value) kickDanmaku()
  else {
    stopDanmaku()
    danmaku?.clear()
  }
}

async function loadDanmaku(url) {
  if (!url || url === dmLoadedFor) return
  dmLoadedFor = url
  danmakuFailed.value = false
  danmakuCount.value = 0
  stopDanmaku()
  danmaku?.clear()
  try {
    const res = await fetch(url, { credentials: 'same-origin' })
    if (!res.ok) throw new Error(`HTTP ${res.status}`)
    const data = await res.json()
    const list = Array.isArray(data?.comments) ? data.comments : []
    if (disposed || dmLoadedFor !== url) return
    danmakuCount.value = list.length
    await nextTick()
    ensureDanmaku()
    danmaku?.setComments(list)
    applyDanmakuOptions()
    kickDanmaku()
  } catch {
    // A source that cannot hand over its comments must not break playback: the
    // control simply reports that they are missing.
    danmakuFailed.value = true
  }
}

watch(danmakuUrl, (url) => {
  if (url) loadDanmaku(url)
  else {
    dmLoadedFor = ''
    danmakuCount.value = 0
    danmakuFailed.value = false
    stopDanmaku()
    danmaku?.clear()
  }
})

watch([danmakuOpacity, danmakuSpeed, danmakuArea], () => {
  localStorage.setItem('dm:dmOpacity', String(danmakuOpacity.value))
  localStorage.setItem('dm:dmSpeed', String(danmakuSpeed.value))
  localStorage.setItem('dm:dmArea', String(danmakuArea.value))
})

function saveProgress() {  const v = videoEl.value
  if (!v || !v.duration || !isFinite(v.duration)) return
  api.post('/api/progress', {
    file_id: Number(route.params.fileId),
    // File time, not stream time: otherwise resuming a session-packaged playback
    // would save a position measured from where the session happened to begin.
    position: v.currentTime + streamStart.value,
    duration: v.duration + streamStart.value,
  }).catch(() => {})
}

function selectVersion(v) {
  if (v.current) return
  saveProgress()
  toast.info(`切换到 ${v.label || '另一版本'}`)
  router.replace(`/play/${v.id}`)
}

watch(() => route.params.fileId, (nv, ov) => {
  if (nv && ov && nv !== ov) loadDecision()
})

function selectAudio(i) {
  currentAudio.value = i
  panelOpen.value = false
  loadDecision()
}

// Subtitle preference: 'auto' (follow the server's ranking), 'off', or a
// language code the viewer picked. Kept across files so a viewing habit sticks.
const SUB_PREF_KEY = 'dm:subPref'

function findSubTrack(sub) {
  const v = videoEl.value
  if (!v) return null
  for (const el of v.querySelectorAll('track')) {
    if (el.src.includes(`/${sub.id}.vtt`)) return el
  }
  return null
}

// Subtitle files carry times in *file* time, while the element's clock is
// relative to a packaged session (see streamStart). Move the cues onto the
// element's clock, once per track, whenever they arrive.
function shiftCues(el) {
  if (!el || el.dataset.cuesShifted === '1' || !streamStart.value) return
  const t = el.track
  if (!t || !t.cues || !t.cues.length) return
  const off = streamStart.value
  for (const c of t.cues) {
    try {
      c.startTime = Math.max(0, c.startTime - off)
      c.endTime = Math.max(0, c.endTime - off)
    } catch { /* frozen cue: leave it alone */ }
  }
  el.dataset.cuesShifted = '1'
}

// Cue placement is a percentage of the *element*, but the picture inside it may be
// letterboxed — a portrait window showing a 16:9 film is a band across the middle,
// with black above and below it — and a cue near the bottom of the element then
// lands in the black, nowhere near the picture. That is what "the subtitles are
// stuck to the very bottom" was on a phone. ASS coordinates are in video space,
// which is the picture, so the placement is remapped into the picture's own box.
// It runs again whenever that box changes (rotation, fullscreen, a window resize);
// the original value stays on the cue so repeated runs cannot compound.
function fitCuesToPicture() {
  const v = videoEl.value
  if (!v || !v.videoWidth || !v.videoHeight) return
  const box = v.getBoundingClientRect()
  if (!box.height || !box.width) return
  const scale = Math.min(box.width / v.videoWidth, box.height / v.videoHeight)
  const picH = v.videoHeight * scale
  const top = (box.height - picH) / 2
  for (const t of v.textTracks) {
    if (!t.cues) continue
    for (const c of t.cues) {
      if (typeof c.line !== 'number') continue
      if (c.dmLine === undefined) c.dmLine = c.line
      try {
        c.line = ((top + (c.dmLine / 100) * picH) / box.height) * 100
      } catch { /* frozen cue: leave it alone */ }
    }
  }
}

// showSub turns one subtitle track on and the rest off. It goes through the
// element's own .track rather than indexing v.textTracks, because hls.js
// registers text tracks of its own and the indices then stop lining up.
function showSub(sub) {
  const v = videoEl.value
  if (!v) return false
  for (const t of v.textTracks) t.mode = 'disabled'
  if (!sub) {
    activeSub.value = null
    return true
  }
  const el = findSubTrack(sub)
  if (!el || !el.track) return false
  el.track.mode = 'showing'
  // The cues may already be loaded, or may arrive with the track's load event.
  shiftCues(el)
  fitCuesToPicture()
  el.addEventListener('load', () => { shiftCues(el); fitCuesToPicture() }, { once: true })
  activeSub.value = sub
  return true
}

// Burning a text subtitle overlays it on the picture, which is the faithful
// rendering for typesetting — and what an iOS player can actually show. It
// re-encodes, so it is a deliberate choice; pressing it again turns it off.
function burnSubtitle(sub) {
  if (!sub) return
  if (burnSub.value === sub.id) {
    burnSub.value = 0
    loadDecision(forceMode.value || undefined)
    return
  }
  burnSub.value = sub.id
  activeSub.value = null
  panelOpen.value = false
  toast.info(`烧录字幕:${sub.label || '字幕'}（需要重新编码,稍等）`)
  loadDecision(forceMode.value || undefined)
}

function selectSub(sub) {
  // A track the server marked as burn-only is images (PGS/VobSub) or was chosen
  // for faithful typesetting: it can only be shown by re-encoding the video with
  // it overlaid, so switching to it reloads the stream.
  if (sub && sub.burn) {
    burnSub.value = sub.id
    activeSub.value = null
    panelOpen.value = false
    toast.info(`烧录字幕:${sub.label || '图形字幕'}（需要重新编码,稍等）`)
    loadDecision(forceMode.value || undefined)
    return
  }
  // Leaving a burned stream behind also needs a reload: those pixels are part of
  // the picture now, so the track list cannot simply be switched.
  const wasBurning = burnSub.value !== 0
  burnSub.value = 0
  const isActive = sub && activeSub.value && activeSub.value.id === sub.id
  if (!sub || isActive) {
    showSub(null)
    localStorage.setItem(SUB_PREF_KEY, 'off')
    if (wasBurning) loadDecision(forceMode.value || undefined)
    return
  }
  if (!showSub(sub)) return
  localStorage.setItem(SUB_PREF_KEY, sub.lang || 'auto')
  toast.info(`字幕:${sub.label || sub.lang || '已开启'}`)
  if (wasBurning) loadDecision(forceMode.value || undefined)
}

// autoSelectSub turns subtitles on without being asked: the track the server
// ranked best, unless the viewer already turned them off or picked a language.
function autoSelectSub() {
  if (nativeSubs.value) {
    // The player owns the subtitles (HLS renditions): its own menu applies the
    // server's DEFAULT, and attaching tracks here would double them up.
    activeSub.value = null
    return
  }
  const subs = play.value?.subtitles || []
  if (!subs.length) {
    activeSub.value = null
    return
  }
  const pref = localStorage.getItem(SUB_PREF_KEY) || 'auto'
  if (pref === 'off') {
    // The server marks a track as default, which lets the browser turn it on by
    // itself; make sure "off" still wins.
    showSub(null)
    return
  }
  const wanted = pref !== 'auto' ? subs.find((s) => s.lang === pref) : null
  const pick = wanted || subs.find((s) => s.default) || subs[0]
  nextTick(() => {
    if (!showSub(pick)) activeSub.value = null
  })
}

/* ------------------------------------------------------------------ controls */

/*
 * Tapping the picture. On a touch screen a single tap is far too easy to make by
 * accident — a hand shifting while watching pauses the video — so a tap only
 * brings the controls back and play/pause takes the deliberate double tap. The
 * count is our own rather than the browser's dblclick, whose window is wide
 * enough to catch two unrelated taps. A mouse has no such problem and keeps
 * click-to-pause; iOS plays through its own controls and is left alone.
 */
let tapTimer = null
let taps = 0

function onTap() {
  if (locked.value || nativeControls.value) return
  if (!touchPlayer.value) { togglePlay(); return }
  taps++
  if (taps > 1) {
    clearTimeout(tapTimer)
    tapTimer = null
    taps = 0
    togglePlay()
    return
  }
  // Not a pause yet: reveal the controls, and wait long enough to be told the tap
  // was the first half of a double tap.
  tapTimer = setTimeout(() => { tapTimer = null; taps = 0; showControls() }, 240)
}

function onDoubleTap() {
  // The mouse path only: a touch screen handled its double tap in onTap already.
  if (locked.value || touchPlayer.value || nativeControls.value) return
  toggleFullscreen()
}

/** The lock button — the one control a locked player still answers to. */
function toggleLock() {
  locked.value = !locked.value
  if (locked.value) {
    menuOpen.value = false
    panelOpen.value = false
    toast.info('已锁定按键,点左侧锁图标解锁', 2600)
  } else {
    showControls()
  }
}

function togglePlay() {
  const v = videoEl.value
  if (!v) return
  if (v.paused) v.play().catch(() => {}); else v.pause()
}

function seekBy(delta) {
  const v = videoEl.value
  if (!v || !isFinite(v.duration)) return
  v.currentTime = Math.max(0, Math.min(v.duration, v.currentTime + delta))
  toast.info(`${delta > 0 ? '快进' : '后退'} ${Math.abs(delta)} 秒`, 1200)
}

function pctFromEvent(e) {
  const el = e.currentTarget
  const rect = el.getBoundingClientRect()
  const x = ('clientX' in e ? e.clientX : e.touches?.[0]?.clientX ?? 0) - rect.left
  return Math.max(0, Math.min(1, x / rect.width))
}

function beginScrub(e) {
  if (!duration.value) return
  scrubbing.value = true
  scrubTime.value = pctFromEvent(e) * duration.value
  window.addEventListener('pointermove', moveScrub)
  window.addEventListener('pointerup', endScrub)
}
function moveScrub(e) {
  if (!scrubbing.value) return
  const bar = document.querySelector('.seek')
  if (!bar) return
  const rect = bar.getBoundingClientRect()
  const x = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width))
  scrubTime.value = x * duration.value
}
function endScrub() {
  const v = videoEl.value
  if (scrubbing.value && v && isFinite(scrubTime.value)) {
    // scrubTime is file time; the element wants stream time.
    v.currentTime = Math.max(0, scrubTime.value - streamStart.value)
    position.value = scrubTime.value
  }
  scrubbing.value = false
  window.removeEventListener('pointermove', moveScrub)
  window.removeEventListener('pointerup', endScrub)
}

function onSeekHover(e) {
  if (!duration.value) return
  const rect = e.currentTarget.getBoundingClientRect()
  hoverX.value = e.clientX - rect.left
  hoverTime.value = pctFromEvent(e) * duration.value
}
function clearHover() { hoverTime.value = null }

function applyVolume() {
  const v = videoEl.value
  if (!v) return
  v.volume = volume.value
  v.muted = muted.value
}
function setVolume(val) {
  volume.value = Math.max(0, Math.min(1, val))
  muted.value = volume.value === 0
  localStorage.setItem('dm:volume', String(volume.value))
  localStorage.setItem('dm:muted', muted.value ? '1' : '0')
  applyVolume()
}
function toggleMute() {
  muted.value = !muted.value
  localStorage.setItem('dm:muted', muted.value ? '1' : '0')
  applyVolume()
}
function setRate(r) {
  rate.value = r
  localStorage.setItem('dm:rate', String(r))
  if (videoEl.value) videoEl.value.playbackRate = r
  menuOpen.value = false
}

async function toggleFullscreen() {
  // The whole player goes fullscreen, not just the picture. Fullscreen is a top
  // layer that only renders the element's own subtree, so anything outside it —
  // the settings drawer — is not on screen and cannot be touched: a drag on it
  // landed on the controls underneath instead, which is how a swipe down the
  // settings list ended up on the volume slider.
  const el = playerEl.value ?? stageEl.value
  const v = videoEl.value
  if (!document.fullscreenElement) {
    if (el?.requestFullscreen) await el.requestFullscreen().catch(() => {})
    else if (v?.webkitEnterFullscreen) v.webkitEnterFullscreen()
  } else {
    document.exitFullscreen?.()
  }
}
async function togglePip() {
  const v = videoEl.value
  if (!v) return
  try {
    if (document.pictureInPictureElement) await document.exitPictureInPicture()
    else await v.requestPictureInPicture()
  } catch { toast.err('当前浏览器不支持画中画') }
}
function onFsChange() {
  fsActive.value = !!document.fullscreenElement
  fitCuesToPicture()
  // Fullscreen is the lock's only home: the watch page has no lock button, so a
  // lock that outlived fullscreen would be a player with no controls at all.
  if (!fsActive.value) locked.value = false
}
function onPipChange() { pipActive.value = !!document.pictureInPictureElement }

function showControls() {
  if (locked.value) return   // a locked player shows nothing but the lock button
  controlsVisible.value = true
  clearTimeout(hideTimer)
  if (!theaterChrome.value) return   // a page keeps its buttons; only the theater fades
  if (isPlaying.value && !menuOpen.value && !panelOpen.value) {
    hideTimer = setTimeout(() => { controlsVisible.value = false }, 3200)
  }
}
function onStageMove() { if (!isIOS.value) showControls() }

function onKey(e) {
  if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return
  const k = e.key.toLowerCase()
  if (k === ' ' || k === 'k') { e.preventDefault(); togglePlay(); showControls() }
  else if (k === 'arrowright') { e.preventDefault(); seekBy(10) }
  else if (k === 'arrowleft') { e.preventDefault(); seekBy(-10) }
  else if (k === 'l') seekBy(30)
  else if (k === 'j') seekBy(-30)
  else if (k === 'arrowup') { e.preventDefault(); setVolume(volume.value + 0.05) }
  else if (k === 'arrowdown') { e.preventDefault(); setVolume(volume.value - 0.05) }
  else if (k === 'm') toggleMute()
  else if (k === 'f') toggleFullscreen()
  else if (k === 'p') togglePip()
  else if (k === 'escape') { if (panelOpen.value) panelOpen.value = false; else if (menuOpen.value) menuOpen.value = false }
  else if (/^[0-9]$/.test(k) && videoEl.value?.duration) {
    videoEl.value.currentTime = (Number(k) / 10) * videoEl.value.duration
  }
  showControls()
}

/* A drag over the picture in fullscreen is either ours or the browser's.
   Ours is tap/double-tap and the seek bar's own pointer handling; the browser's is
   its video-gesture layer, which some Android browsers run over fullscreen video:
   a horizontal drag seeks, a vertical one on the right-hand side changes the
   volume, and on the left it changes the *brightness* — which is the giveaway,
   since no page can touch screen brightness. Consuming the drag keeps that layer
   from acting on a locked player, and keeps its zones from claiming touches that
   belong to a popup. A popup is left alone so it can still scroll itself. */
function consumeFullscreenDrag(e) {
  if (!fsActive.value) return
  const t = e.target
  if (t instanceof Element && t.closest('.panel, .ctl-menu')) return
  if (e.cancelable) e.preventDefault()
}

function goBack() {
  saveProgress()
  router.back()
}

onMounted(() => {
  loadDecision()
  applyVolume()
  window.addEventListener('beforeunload', saveProgress)
  window.addEventListener('keydown', onKey)
  window.addEventListener('resize', onViewport)
  // Fullscreenchange fires before the element has its new box, and a rotation does
  // not always produce a resize the player sees in time, so the placement follows
  // the element's own box rather than an event that may be early.
  if (typeof ResizeObserver !== 'undefined' && videoEl.value) {
    cueObs = new ResizeObserver(() => fitCuesToPicture())
    cueObs.observe(videoEl.value)
  }
  // The 弹幕 canvas is sized from its own box, which moves with fullscreen, a
  // rotation and a layout change alike.
  if (typeof ResizeObserver !== 'undefined' && dmCanvas.value) {
    dmObs = new ResizeObserver(() => {
      danmaku?.resize()
      paintDanmaku()
    })
    dmObs.observe(dmCanvas.value)
  }
  window.addEventListener('orientationchange', onViewport)
  document.addEventListener('fullscreenchange', onFsChange)
  document.addEventListener('pointermove', showControls)
  // Not passive: the whole point is being able to cancel the gesture.
  document.addEventListener('touchmove', consumeFullscreenDrag, { passive: false })
  videoEl.value?.addEventListener('enterpictureinpicture', onPipChange)
  videoEl.value?.addEventListener('leavepictureinpicture', onPipChange)
})
onBeforeUnmount(() => {
  disposed = true
  saveProgress()
  destroyHls()
  stopDanmaku()
  clearTimeout(switchTimer)
  stopProbePoll()
  stopStreamPoll()
  clearStall()
  clearTimeout(hideTimer)
  clearTimeout(tapTimer)
  window.removeEventListener('beforeunload', saveProgress)
  window.removeEventListener('keydown', onKey)
  window.removeEventListener('resize', onViewport)
  window.removeEventListener('orientationchange', onViewport)
  document.removeEventListener('fullscreenchange', onFsChange)
  document.removeEventListener('pointermove', showControls)
  document.removeEventListener('touchmove', consumeFullscreenDrag)
  cueObs?.disconnect()
  cueObs = null
  dmObs?.disconnect()
  dmObs = null
  window.removeEventListener('pointermove', moveScrub)
  window.removeEventListener('pointerup', endScrub)
})
</script>

<template>
  <div ref="playerEl" class="player" :class="[layoutClass, { fs: fsActive, locked, 'hide-ui': chromeHidden }]">
    <div ref="watchEl" class="watch" :class="{ 'no-side': layout === 'tablet' && !episodes.length }">
      <div class="wmain">
        <!-- ================= the picture ================= -->
        <div ref="stageEl" class="stage" @pointermove="onStageMove">
          <div class="frame">
            <video
              ref="videoEl"
              :controls="nativeControls"
              playsinline
              webkit-playsinline
              preload="auto"
              @loadedmetadata="onLoadedMetadata"
              @error="onErr"
              @timeupdate="onTimeUpdate"
              @progress="onTimeUpdate"
              @pause="saveProgress(); isPlaying = false; showControls(); stopDanmaku()"
              @play="isPlaying = true; showControls(); kickDanmaku()"
              @seeking="stopDanmaku()"
              @seeked="paintDanmaku()"
              @waiting="onWaiting"
              @playing="onPlaying"
              @canplay="onCanPlay"
              @click="onTap"
              @dblclick="onDoubleTap"
              @ratechange="rate = videoEl?.playbackRate || 1"
            >
              <template v-if="!nativeSubs">
                <track
                  v-for="sub in (play?.subtitles || []).filter(s => s.url)" :key="sub.id" kind="subtitles"
                  :src="sub.url" :label="sub.label || sub.lang || '字幕'"
                  :default="!!sub.default"
                />
              </template>
            </video>

            <!-- 弹幕层. Under the controls (z-index 3 against their 4 and up) and
                 transparent to input, so every gesture still reaches the picture. -->
            <canvas v-show="danmakuReady && danmakuOn" ref="dmCanvas" class="dm-layer" aria-hidden="true"></canvas>

            <!-- On the immersive player the top bar carries the title and the
                 settings; on a watch page both are in the page below, so only
                 the way back stays on the picture. -->
            <div v-if="stageBar" class="top-bar">
              <button class="icon-btn solid round" aria-label="返回" @click="goBack">
                <AppIcon name="arrowLeft" :size="19" />
              </button>
              <template v-if="theaterChrome">
                <div class="tb-title">
                  <div class="tb-name truncate">{{ play?.title || '播放中' }}</div>
                  <div class="tb-sub">
                    <span v-if="play" class="badge" :class="modeClass">{{ modeLabel }}</span>
                    <span v-if="play?.resolution" class="tiny">{{ play.resolution.replace('x', '×') }}</span>
                    <span v-if="play?.video_codec" class="tiny">
                      {{ play.video_codec.toUpperCase() }}<template v-if="play?.audio_codec"> / {{ play.audio_codec.toUpperCase() }}</template>
                    </span>
                    <span v-if="currentSize" class="tiny">{{ currentSize }}</span>
                  </div>
                </div>
                <button
                  class="icon-btn solid round" :class="{ on: panelOpen }"
                  aria-label="播放设置" @click="togglePanel"
                >
                  <AppIcon name="list" :size="19" />
                </button>
              </template>
            </div>

            <!-- Lock: fullscreen only. While locked it stays on screen — it is the
                 one control a locked player still answers to. -->
            <Transition name="fade">
              <button
                v-if="fsActive && (locked || controlsVisible)"
                class="icon-btn solid round lock-btn" :class="{ on: locked }"
                :aria-pressed="locked" :aria-label="locked ? '已锁定,点击解锁' : '锁定按键'"
                :title="locked ? '已锁定 · 点击解锁' : '锁定按键'" @click="toggleLock"
              >
                <AppIcon :name="locked ? 'lock' : 'lockOpen'" :size="18" />
              </button>
            </Transition>

            <!-- Center states. The theater has nowhere else to put them; a watch
                 page shows its progress under the picture instead (see .wi-loading
                 below), where it cannot cover the picture or the play button. -->
            <Transition name="fade">
              <div v-if="theaterChrome && isLoading && !error && !fatal" class="center-state">
                <span class="ring" />
                <div class="cs-text">{{ loadingText }}</div>
                <div v-if="streamPct > 0" class="cs-bar" role="progressbar" :aria-valuenow="Math.round(streamPct)" aria-valuemin="0" aria-valuemax="100">
                  <i :style="{ width: streamPct + '%' }"></i>
                </div>
                <div v-if="encoderTooSlow" class="cs-hint">
                  <div class="cs-hint-t">
                    服务器生成分片的速度只有实时速度的 {{ streamRate.toFixed(2) }} 倍,追不上播放。
                    通常是片源读取慢;换个方式或稍等再试。
                  </div>
                  <div class="cs-actions">
                    <button class="btn sm" @click="loadDecision('remux')">强制转封装</button>
                    <button class="btn sm" @click="loadDecision()">重试自动</button>
                  </div>
                </div>
                <div v-else-if="slowHint" class="cs-hint">
                  <div class="cs-hint-t">这个片源读取得慢,当前方式追不上播放速度。</div>
                  <div class="cs-actions">
                    <button class="btn sm" @click="loadDecision('remux')">强制转封装</button>
                    <button class="btn sm" @click="loadDecision()">重试自动</button>
                  </div>
                </div>
              </div>
            </Transition>

            <Transition name="fade">
              <div v-if="error || fatal" class="error-panel">
                <AppIcon name="alert" :size="26" />
                <div class="ep-title">{{ unplayableFile ? '无法播放' : '播放遇到问题' }}</div>
                <div class="ep-text">{{ error || fatal }}</div>
                <div class="ep-actions">
                  <!-- An unplayable file has nothing to retry: the server made a
                       decision about the file, not about a failed attempt. -->
                  <template v-if="unplayableFile">
                    <button class="btn primary" @click="goBack"><AppIcon name="arrowLeft" :size="15" /> 返回</button>
                  </template>
                  <template v-else>
                    <button class="btn primary" @click="loadDecision()"><AppIcon name="refresh" :size="15" /> 重试自动</button>
                    <button class="btn" @click="loadDecision('remux')">强制转封装</button>
                  </template>
                </div>
              </div>
            </Transition>

            <!-- big play affordance when paused (custom chrome only) -->
            <Transition name="fade">
              <button
                v-if="!nativeControls && !isPlaying && !isLoading && !error && !fatal"
                class="big-play" aria-label="播放" @click="togglePlay"
              >
                <AppIcon name="play" :size="30" filled />
              </button>
            </Transition>
          </div>

          <!-- ================= bottom control bar =================
               Immersive: laid over the foot of the picture. Watch page: the same
               bar, sitting under the picture — a 16:9 box is no place for a
               control strip. Same markup, two positions. -->
          <div v-if="!nativeControls" class="controls" @pointermove="showControls">
            <div
              class="seek" @pointerdown="beginScrub" @pointermove="onSeekHover"
              @pointerleave="clearHover"
            >
              <div class="seek-track">
                <div class="seek-buffer" :style="{ width: bufferedPct + '%' }"></div>
                <div class="seek-fill" :style="{ width: progressPct + '%' }"></div>
                <div class="seek-knob" :style="{ left: progressPct + '%' }"></div>
              </div>
              <div v-if="hoverTime !== null" class="seek-tip" :style="{ left: hoverX + 'px' }">{{ fmtTime(hoverTime) }}</div>
            </div>

            <div class="ctl-row">
              <button class="icon-btn" :aria-label="isPlaying ? '暂停' : '播放'" @click="togglePlay">
                <AppIcon :name="isPlaying ? 'pause' : 'play'" :size="20" :filled="true" />
              </button>
              <button class="icon-btn" aria-label="后退 10 秒" @click="seekBy(-10)">
                <AppIcon name="back10" :size="19" />
              </button>
              <button class="icon-btn" aria-label="前进 10 秒" @click="seekBy(10)">
                <AppIcon name="fwd10" :size="19" />
              </button>

              <div class="vol">
                <button class="icon-btn" :aria-label="muted ? '取消静音' : '静音'" @click="toggleMute">
                  <AppIcon :name="muted || volume === 0 ? 'volumeMute' : 'volume'" :size="19" />
                </button>
                <div class="vol-slider">
                  <div class="vol-fill" :style="{ width: volumePct + '%' }"></div>
                  <input
                    type="range" min="0" max="1" step="0.02" :value="muted ? 0 : volume"
                    aria-label="音量" @input="setVolume(Number($event.target.value))"
                  />
                </div>
              </div>

              <div class="time num">
                <span>{{ fmtTime(shownTime) }}</span>
                <span class="dim">/</span>
                <span class="dim">{{ fmtTime(duration) }}</span>
              </div>

              <span class="spacer"></span>

              <button
                v-if="!nativeSubs && (play?.subtitles || []).length" class="icon-btn"
                :class="{ on: activeSub }" aria-label="字幕" title="字幕"
                @click="openPanel('subs')"
              >
                <AppIcon name="subtitles" :size="19" />
              </button>

              <div class="menu-wrap">
                <button
                  class="icon-btn" :class="{ on: menuOpen || (panelOpen && panelMode === 'settings') }"
                  :aria-label="touchOnly ? '设置' : '播放速度与模式'" title="设置" @click="toggleMenu"
                >
                  <AppIcon name="settings" :size="19" />
                </button>
                <Transition name="fade">
                  <div v-if="menuOpen && (!isStacked || fsActive)" class="ctl-menu">
                    <div class="cm-title">播放速度</div>
                    <div class="cm-rates">
                      <button
                        v-for="r in rates" :key="r" class="chip sm" :class="{ on: rate === r }"
                        @click="setRate(r)"
                      >{{ r }}×</button>
                    </div>
                    <div class="cm-title mt-4">播放模式</div>
                    <button
                      v-for="m in forceModes" :key="m.value" class="cm-item"
                      :class="{ on: (play?.mode === m.value) || (!m.value && !forceModes.some(x => x.value === play?.mode)) }"
                      @click="setMode(m)"
                    >
                      <span class="cm-label">{{ m.label }}</span>
                      <span class="tiny">{{ m.hint }}</span>
                    </button>
                    <div class="cm-title mt-4">画面</div>
                    <button class="cm-item" @click="togglePip(); menuOpen = false">
                      <span class="cm-label"><AppIcon name="pip" :size="15" /> 画中画</span>
                      <span class="tiny">{{ pipActive ? '已开启' : '小窗播放' }}</span>
                    </button>
                  </div>
                </Transition>
              </div>

              <button class="icon-btn" aria-label="全屏" @click="toggleFullscreen">
                <AppIcon :name="fsActive ? 'fullscreenExit' : 'fullscreen'" :size="19" />
              </button>
            </div>
          </div>
        </div>

        <!-- ============ watch page: everything under the picture ============
             In the theater this wrapper is display:contents, so the settings
             drawer inside it is the same right-hand drawer it always was. -->
        <div class="winfo">
          <template v-if="isStacked">
            <!-- Preparing, as a long thin bar under the picture: centred on a 16:9
                 box it would sit exactly where the play button goes. -->
            <div v-if="!theaterChrome && isLoading && !error && !fatal" class="wi-loading">
              <div
                class="wl-bar" :class="{ live: streamPct > 0 }" role="progressbar"
                :aria-valuenow="Math.round(streamPct)" aria-valuemin="0" aria-valuemax="100"
              >
                <i :style="streamPct > 0 ? { width: streamPct + '%' } : null"></i>
              </div>
              <div class="wl-text">{{ loadingText }}</div>
              <div v-if="encoderTooSlow" class="cs-hint">
                <div class="cs-hint-t">
                  服务器生成分片的速度只有实时速度的 {{ streamRate.toFixed(2) }} 倍,追不上播放。
                  通常是片源读取慢;换个方式或稍等再试。
                </div>
                <div class="cs-actions">
                  <button class="btn sm" @click="loadDecision('remux')">强制转封装</button>
                  <button class="btn sm" @click="loadDecision()">重试自动</button>
                </div>
              </div>
              <div v-else-if="slowHint" class="cs-hint">
                <div class="cs-hint-t">这个片源读取得慢,当前方式追不上播放速度。</div>
                <div class="cs-actions">
                  <button class="btn sm" @click="loadDecision('remux')">强制转封装</button>
                  <button class="btn sm" @click="loadDecision()">重试自动</button>
                </div>
              </div>
            </div>

            <!-- The tablet's own way back, in the page rather than on the picture. -->
            <div v-if="layout === 'tablet'" class="wi-topline">
              <button class="btn sm" @click="goBack">
                <AppIcon name="arrowLeft" :size="15" /> 返回
              </button>
              <span v-if="infoShow?.name" class="tiny truncate">{{ infoShow.name }}</span>
            </div>

            <h1 class="wi-title">{{ play?.title || '播放中' }}</h1>
            <div class="wi-meta">
              <span v-if="play" class="badge" :class="modeClass">{{ modeLabel }}</span>
              <span v-if="infoShow?.year" class="badge plain">{{ infoShow.year }}</span>
              <span v-if="infoShow?.rating" class="badge rating">
                <AppIcon name="star" :size="11" filled />{{ Number(infoShow.rating).toFixed(1) }}
              </span>
              <span v-if="play?.resolution" class="tiny">{{ play.resolution.replace('x', '×') }}</span>
              <span v-if="play?.video_codec" class="tiny">
                {{ play.video_codec.toUpperCase() }}<template v-if="play?.audio_codec"> / {{ play.audio_codec.toUpperCase() }}</template>
              </span>
              <span v-if="currentSize" class="tiny">{{ currentSize }}</span>
              <span v-if="meta?.air_date" class="tiny">{{ meta.air_date }}</span>
            </div>

            <div v-if="episodes.length" class="wi-actions">
              <button class="btn sm" :disabled="!prevEp" @click="playEpisode(prevEp)">
                <AppIcon name="chevronLeft" :size="14" /> 上一集
              </button>
              <button class="btn sm" :disabled="!nextEp" @click="playEpisode(nextEp)">
                下一集 <AppIcon name="chevronRight" :size="14" />
              </button>
              <span class="spacer"></span>
              <span v-if="epIndex >= 0" class="tiny num">第 {{ epIndex + 1 }} / {{ episodes.length }} 集</span>
            </div>

            <div class="wi-tabs" role="tablist">
              <button
                v-if="tabs.includes('eps')" role="tab" :aria-selected="activeTab === 'eps'"
                :class="{ on: activeTab === 'eps' }" @click="tab = 'eps'"
              >
                <AppIcon name="grid" :size="14" /> 选集<span class="tiny num">{{ episodes.length }}</span>
              </button>
              <button
                role="tab" :aria-selected="activeTab === 'about'"
                :class="{ on: activeTab === 'about' }" @click="tab = 'about'"
              >
                <AppIcon name="info" :size="14" /> 简介
              </button>
              <button
                role="tab" :aria-selected="activeTab === 'settings'"
                :class="{ on: activeTab === 'settings' }" @click="tab = 'settings'"
              >
                <AppIcon name="settings" :size="14" /> 设置
              </button>
            </div>

            <!-- phone: the episode grid, Bilibili-style -->
            <div v-if="activeTab === 'eps' && episodes.length" class="wi-eps">
              <button
                v-for="(ep, i) in episodes" :key="ep.id" class="wi-ep num"
                :class="{ on: ep.id === play?.item_id }"
                :aria-label="epName(ep, i)" :title="epName(ep, i)"
                @click="playEpisode(ep)"
              >{{ epNo(ep, i) }}</button>
            </div>

            <div v-else-if="activeTab === 'about'" class="wi-about">
              <p v-if="overview" class="wi-ov">{{ overview }}</p>
              <p v-else class="tiny">这个条目还没有简介。</p>
              <div v-if="infoGenres.length" class="wi-facts">
                <span v-for="g in infoGenres" :key="g" class="chip sm">{{ g }}</span>
              </div>
            </div>
          </template>

          <!-- ============ side panel: tracks, mode, picture ============
               A right-hand drawer over the picture in the theater, the 设置 tab
               of the page in a watch page. -->
          <Transition :name="panelInline ? 'none' : 'slide'">
            <aside v-if="panelShown" class="panel glass">
              <div class="pn-head">
                <div class="card-title mb-0">
                  <AppIcon :name="drawerMode === 'subs' ? 'subtitles' : 'layers'" :size="16" />
                  {{ drawerMode === 'subs' ? '字幕' : '播放设置' }}
                </div>
                <button v-if="!panelInline" class="icon-btn" aria-label="关闭" @click="panelOpen = false">
                  <AppIcon name="close" :size="17" />
                </button>
              </div>

              <div v-if="drawerMode !== 'subs' && (play?.versions || []).length" class="pn-group">
                <div class="label">版本</div>
                <div class="pn-chips">
                  <button
                    v-for="v in play.versions" :key="v.id"
                    class="chip" :class="{ on: v.current }" @click="selectVersion(v)"
                  >{{ v.label || '默认版本' }}</button>
                </div>
              </div>

              <div v-if="drawerMode !== 'subs' && (play?.audio_tracks || []).length > 1" class="pn-group">
                <div class="label">音轨</div>
                <div class="pn-chips">
                  <button
                    v-for="a in play.audio_tracks" :key="a.ordinal"
                    class="chip" :class="{ on: a.ordinal === currentAudio }"
                    @click="selectAudio(a.ordinal)"
                  >
                    {{ a.codec.toUpperCase() }}{{ a.lang ? ' · ' + a.lang : '' }}{{ a.title ? ' · ' + a.title : '' }}
                  </button>
                </div>
              </div>

              <div v-if="drawerMode !== 'settings' && !nativeSubs && (play?.subtitles || []).length" class="pn-group">
                <div class="label">字幕</div>
                <div class="pn-chips">
                  <button class="chip" :class="{ on: activeSub === null }" @click="selectSub(null)">关闭</button>
                  <button
                    v-for="sub in play.subtitles" :key="sub.id"
                    class="chip" :class="{ on: activeSub && activeSub.id === sub.id || (sub.burn && burnSub === sub.id) }"
                    @click="selectSub(sub)"
                  >{{ sub.label || sub.lang || '字幕' }}<span v-if="sub.burn" class="tiny"> · 烧录</span></button>
                </div>
              </div>

              <!-- Typesetting (ASS) tracks: WebVTT can only approximate where a
                   sign sits and what it looks like, so the original is offered as
                   a burn — the faithful rendering, and the only one iOS shows.
                   This group appears on iOS too, where the text tracks themselves
                   are left to the native player. -->
              <div v-if="drawerMode !== 'settings' && burnables.length" class="pn-group">
                <div class="label">忠实排版(烧录)</div>
                <div class="pn-chips">
                  <button
                    v-for="sub in burnables" :key="'burn' + sub.id"
                    class="chip" :class="{ on: burnSub === sub.id }"
                    @click="burnSubtitle(sub)"
                  >{{ sub.label || sub.lang || '字幕' }}<span class="tiny"> · 重新编码</span></button>
                </div>
              </div>

              <div v-if="drawerMode !== 'subs'" class="pn-group">
                <div class="label">播放速度</div>
                <div class="pn-rates">
                  <button v-for="r in rates" :key="r" class="chip sm" :class="{ on: rate === r }" @click="setRate(r)">{{ r }}×</button>
                </div>
              </div>

              <div v-if="drawerMode !== 'subs'" class="pn-group">
                <div class="label">播放模式</div>
                <div class="pn-chips">
                  <button
                    v-for="m in forceModes" :key="m.value" class="chip"
                    :class="{ on: (play?.mode === m.value) || (!m.value && !forceModes.some(x => x.value === play?.mode)) }"
                    @click="setMode(m)"
                  >{{ m.label }}</button>
                </div>
              </div>

              <div v-if="drawerMode !== 'subs'" class="pn-group">
                <div class="label">画面</div>
                <div class="pn-chips">
                  <button class="chip" :class="{ on: pipActive }" @click="togglePip()">
                    <AppIcon name="pip" :size="14" /> 画中画{{ pipActive ? ' · 已开启' : '' }}
                  </button>
                </div>
              </div>

              <div v-if="drawerMode !== 'subs' && danmakuUrl" class="pn-group">
                <div class="label">
                  弹幕
                  <span class="tiny">{{ danmakuFailed ? ' · 读取失败' : danmakuCount ? ` · ${danmakuCount} 条` : '' }}</span>
                </div>
                <div class="pn-chips">
                  <button class="chip" :class="{ on: danmakuOn }" @click="toggleDanmaku">
                    <AppIcon name="tv" :size="14" /> {{ danmakuOn ? '已开启' : '已关闭' }}
                  </button>
                </div>
                <template v-if="danmakuOn && danmakuReady">
                  <label class="pn-range">
                    <span>不透明度</span>
                    <input v-model.number="danmakuOpacity" type="range" min="0.2" max="1" step="0.1" @input="applyDanmakuOptions" />
                  </label>
                  <label class="pn-range">
                    <span>滚动速度</span>
                    <input v-model.number="danmakuSpeed" type="range" min="4" max="14" step="1" @input="applyDanmakuOptions" />
                  </label>
                  <label class="pn-range">
                    <span>显示区域</span>
                    <input v-model.number="danmakuArea" type="range" min="0.25" max="1" step="0.05" @input="applyDanmakuOptions" />
                  </label>
                </template>
              </div>

              <div v-if="play?.reason" class="pn-reason tiny">
                <AppIcon name="sparkles" :size="13" />
                <span>{{ play.reason }}</span>
              </div>
            </aside>
          </Transition>
        </div>
      </div>

      <!-- tablet: the episodes down the right-hand side -->
      <aside v-if="layout === 'tablet' && episodes.length" class="wside">
        <div class="ws-head">
          <div class="ws-title"><AppIcon name="list" :size="15" /> 选集</div>
          <span class="tiny">{{ episodes.length }} 集</span>
        </div>
        <div class="ws-list">
          <button
            v-for="(ep, i) in episodes" :key="ep.id" class="ws-ep"
            :class="{ on: ep.id === play?.item_id }"
            :aria-label="epName(ep, i)" @click="playEpisode(ep)"
          >
            <span class="ws-thumb">
              <SmartImage v-if="ep.still_path" :src="imgUrl(ep.id, 'still')" :alt="epName(ep, i)" :seed="epName(ep, i)" />
              <span v-else class="ws-ph num">{{ epNo(ep, i) }}</span>
            </span>
            <span class="ws-copy">
              <span class="ws-name truncate">{{ epName(ep, i) }}</span>
              <span class="tiny truncate">{{ epSub(ep) }}</span>
            </span>
            <AppIcon v-if="ep.id === play?.item_id" name="play" :size="14" filled />
          </button>
        </div>
      </aside>
    </div>

    <!-- ================= shortcuts hint (desktop theater only) ================= -->
    <div v-if="!nativeControls && layout === 'theater'" class="hints">
      <span><kbd>空格</kbd> 播放/暂停</span>
      <span><kbd>←</kbd><kbd>→</kbd> 快退/快进 10s</span>
      <span><kbd>F</kbd> 全屏</span>
      <span><kbd>M</kbd> 静音</span>
      <span><kbd>Esc</kbd> 返回</span>
    </div>

    <ToastHost />
  </div>
</template>

<style scoped>
.player {
  position: fixed;
  inset: 0;
  background: #000;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
/* toasts must clear the control bar, not sit inside it */
.player :deep(.toast-host) { bottom: calc(118px + env(safe-area-inset-bottom)); }

/* ============================ the picture ================================= */
/* Theater: the stage *is* the screen and the frame fills it. The watch-page
   layouts below turn both into a normal 16:9 block in a page. */
.watch { flex: 1; min-height: 0; display: flex; flex-direction: column; }
.wmain { position: relative; flex: 1; min-height: 0; display: flex; flex-direction: column; }
.stage { position: relative; flex: 1; min-height: 0; background: #000; }
.frame { position: absolute; inset: 0; }
/* touch-action: the player has a double-tap gesture of its own, so the browser's
   double-tap zoom must not race it. */
video { width: 100%; height: 100%; object-fit: contain; background: #000; display: block; touch-action: manipulation; }

.top-bar {
  position: absolute;
  inset: 0 0 auto 0;
  display: flex;
  align-items: center;
  gap: 14px;
  padding: max(14px, env(safe-area-inset-top)) clamp(14px, 3vw, 34px) 34px;
  background: linear-gradient(to bottom, rgba(3, 4, 6, 0.86), rgba(3, 4, 6, 0));
  transition: opacity var(--t-mid), transform var(--t-mid);
  z-index: 5;
}
.tb-title { min-width: 0; }
.tb-name { font-size: var(--fs-15); font-weight: 650; text-shadow: 0 2px 12px rgba(0, 0, 0, 0.6); }
.tb-sub { display: flex; align-items: center; gap: 9px; margin-top: 4px; }

/* Overlays are centred on the picture rather than laid out beside it: as flex
   items they would compete with the video for room and be pushed off-screen. */
.center-state {
  position: absolute;
  top: 50%;
  left: 50%;
  transform: translate(-50%, -50%);
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 14px;
  z-index: 4;
  pointer-events: none;
}
.ring {
  width: 44px; height: 44px; border-radius: 50%;
  border: 2.5px solid rgba(255, 255, 255, 0.16);
  border-top-color: var(--brand);
  animation: spin 0.9s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }
.cs-text { font-size: var(--fs-13); color: var(--text-2); background: rgba(0, 0, 0, 0.5); padding: 6px 14px; border-radius: var(--r-pill); }
.cs-bar {
  width: min(260px, 62vw);
  height: 6px;
  border-radius: var(--r-pill);
  background: rgba(255, 255, 255, 0.18);
  overflow: hidden;
}
.cs-bar i { display: block; height: 100%; background: var(--brand); border-radius: inherit; transition: width 0.4s ease; }
.cs-hint {
  pointer-events: auto;
  max-width: min(420px, 86vw);
  padding: 14px 16px;
  border-radius: var(--r-m);
  background: rgba(14, 16, 21, 0.94);
  border: 1px solid var(--line-2);
  box-shadow: var(--sh-4);
  text-align: center;
}
.cs-hint-t { font-size: var(--fs-12); color: var(--text-2); line-height: 1.6; }
.cs-actions { display: flex; gap: 8px; justify-content: center; flex-wrap: wrap; margin-top: 10px; }

.error-panel {
  position: absolute;
  top: 50%;
  left: 50%;
  transform: translate(-50%, -50%);
  z-index: 6;
  max-width: min(520px, 88vw);
  text-align: center;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 28px;
  border-radius: var(--r-l);
  background: rgba(14, 16, 21, 0.94);
  border: 1px solid rgba(255, 107, 107, 0.34);
  box-shadow: var(--sh-4);
  color: var(--text-2);
}
.error-panel svg { color: var(--danger); }
.ep-title { font-size: var(--fs-16); font-weight: 700; color: var(--text-1); }
.ep-text { font-size: var(--fs-13); line-height: 1.6; }
.ep-actions { display: flex; gap: 8px; flex-wrap: wrap; justify-content: center; margin-top: 6px; }

.big-play {
  position: absolute;
  top: 50%;
  left: 50%;
  transform: translate(-50%, -50%);
  z-index: 4;
  width: 84px; height: 84px;
  display: grid; place-items: center;
  border-radius: 50%;
  background: rgba(255, 255, 255, 0.94);
  color: #0a0b0f;
  padding-left: 5px;
  box-shadow: 0 20px 60px rgba(0, 0, 0, 0.55);
  transition: transform var(--t-fast), background var(--t-fast);
}
.big-play:hover { transform: translate(-50%, -50%) scale(1.06); background: #fff; }

/* Theater position: laid over the foot of the picture. */
.controls {
  position: absolute;
  inset: auto 0 0 0;
  padding: 60px clamp(14px, 3vw, 34px) max(20px, env(safe-area-inset-bottom));
  background: linear-gradient(to top, rgba(3, 4, 6, 0.94) 10%, rgba(3, 4, 6, 0.6) 55%, rgba(3, 4, 6, 0));
  transition: opacity var(--t-mid), transform var(--t-mid);
  z-index: 5;
}
.hide-ui .controls { opacity: 0; transform: translateY(12px); pointer-events: none; }
.hide-ui .top-bar { opacity: 0; transform: translateY(-12px); pointer-events: none; }
.hide-ui { cursor: none; }

/* Locked: the picture plays on, the chrome is gone, and only the lock button
   still takes a tap. The hidden chrome is not hit-testable, and neither the
   picture nor the page it sits in answers a pan or a zoom — a lock that leaves
   the browser's own gesture machinery running is not a lock. */
.locked .controls { opacity: 0; transform: translateY(12px); pointer-events: none; }
.locked .top-bar { opacity: 0; transform: translateY(-12px); pointer-events: none; }
.locked .panel, .locked .ctl-menu { display: none; }
.locked { touch-action: none; }
.locked video { touch-action: none; }
.locked .hints { opacity: 0; }
.locked { cursor: none; }

.lock-btn {
  position: absolute;
  left: max(10px, env(safe-area-inset-left));
  top: 50%;
  z-index: 9;
  /* Spelled out, and more specific than .icon-btn:active, which would otherwise
     swap the centring transform for a scale while the button is pressed. */
  transform: translateY(-50%);
}
.player .lock-btn:active { transform: translateY(-50%) scale(0.94); }
.locked .lock-btn { opacity: 0.55; }
.locked .lock-btn:hover, .locked .lock-btn:active { opacity: 1; }

/* Everything here is the same whatever the layout: the bar simply moves. */
.seek { position: relative; padding: 10px 0; cursor: pointer; touch-action: none; }
.seek-track {
  position: relative;
  height: 5px;
  border-radius: var(--r-pill);
  background: rgba(255, 255, 255, 0.22);
  transition: height var(--t-fast);
}
.seek:hover .seek-track { height: 8px; }
.seek-buffer { position: absolute; inset: 0 auto 0 0; background: rgba(255, 255, 255, 0.28); border-radius: inherit; }
.seek-fill { position: absolute; inset: 0 auto 0 0; background: var(--brand); border-radius: inherit; }
.seek-knob {
  position: absolute;
  top: 50%;
  width: 14px; height: 14px;
  margin-left: -7px;
  border-radius: 50%;
  background: #fff;
  transform: translateY(-50%) scale(0);
  transition: transform var(--t-fast);
  box-shadow: 0 2px 8px rgba(0, 0, 0, 0.5);
}
.seek:hover .seek-knob { transform: translateY(-50%) scale(1); }
.seek-tip {
  position: absolute;
  bottom: 26px;
  transform: translateX(-50%);
  padding: 4px 9px;
  border-radius: var(--r-xs);
  background: rgba(12, 14, 18, 0.95);
  border: 1px solid var(--line-2);
  font-size: var(--fs-12);
  font-variant-numeric: tabular-nums;
  pointer-events: none;
}

.ctl-row { display: flex; align-items: center; gap: 4px; margin-top: 4px; }
.ctl-row .icon-btn { width: 42px; height: 42px; color: #fff; }
.ctl-row .icon-btn:hover { background: rgba(255, 255, 255, 0.14); }
.ctl-row .icon-btn.on { color: var(--brand); }
.time { display: flex; gap: 6px; font-size: var(--fs-13); margin-left: 8px; color: #fff; }
.time .dim { color: rgba(255, 255, 255, 0.55); }

.vol { display: flex; align-items: center; }
.vol-slider { position: relative; width: 0; overflow: hidden; transition: width var(--t-mid), margin var(--t-mid); display: flex; align-items: center; }
.vol:hover .vol-slider { width: 92px; margin-right: 6px; }
.vol-fill {
  position: absolute; left: 0; height: 4px; border-radius: var(--r-pill);
  background: var(--brand); pointer-events: none; max-width: 100%;
}
.vol-slider input[type="range"] {
  appearance: none;
  width: 92px; height: 4px; border-radius: var(--r-pill);
  background: rgba(255, 255, 255, 0.28);
  outline: none;
}
.vol-slider input[type="range"]::-webkit-slider-thumb {
  appearance: none;
  width: 12px; height: 12px; border-radius: 50%;
  background: #fff; border: none;
  box-shadow: 0 1px 6px rgba(0, 0, 0, 0.5);
}
.vol-slider input[type="range"]::-moz-range-thumb { width: 12px; height: 12px; border: none; border-radius: 50%; background: #fff; }

.menu-wrap { position: relative; }
.ctl-menu {
  position: absolute;
  bottom: calc(100% + 12px);
  right: 0;
  /* Never taller or wider than the screen it is opened on: in phone fullscreen
     the whole menu used to run off the top. */
  width: min(268px, calc(100vw - 24px));
  max-height: calc(100dvh - 132px);
  overflow-y: auto;
  overscroll-behavior: contain;
  padding: 14px;
  border-radius: var(--r-m);
  background: rgba(14, 16, 21, 0.96);
  border: 1px solid var(--line-2);
  box-shadow: var(--sh-4);
  backdrop-filter: blur(20px);
  z-index: 8;
}
.cm-title { font-size: var(--fs-11); font-weight: 700; letter-spacing: 0.1em; text-transform: uppercase; color: var(--text-4); margin-bottom: 8px; }
.cm-rates { display: flex; flex-wrap: wrap; gap: 6px; }
.cm-item {
  display: flex;
  flex-direction: column;
  gap: 1px;
  width: 100%;
  padding: 9px 11px;
  border-radius: var(--r-s);
  text-align: left;
  transition: background var(--t-fast);
}
.cm-item:hover { background: rgba(255, 255, 255, 0.07); }
.cm-item.on { background: var(--brand-tint); }
.cm-label { font-size: var(--fs-13); font-weight: 600; color: var(--text-1); display: inline-flex; align-items: center; gap: 7px; }

/* ============ settings: drawer in the theater, inline in a page =========== */
/* In the theater this wrapper is display:contents, so the panel inside it is
   positioned against .wmain — the same right-hand drawer it always was. */
.winfo { display: contents; }
.panel {
  position: absolute;
  inset: 0 0 0 auto;
  width: min(370px, 90vw);
  padding: max(18px, env(safe-area-inset-top)) 20px 24px;
  border-radius: 0;
  border-left: 1px solid var(--line-2);
  display: flex;
  flex-direction: column;
  gap: 18px;
  overflow-y: auto;
  /* A drag over the panel scrolls the panel and nothing else: no scroll chaining
     onto the page behind it, and no gesture machinery engaging on the picture. */
  overscroll-behavior: contain;
  touch-action: pan-y;
  z-index: 7;
}
.pn-head { display: flex; align-items: center; justify-content: space-between; }
.pn-group { display: flex; flex-direction: column; gap: 8px; align-items: flex-start; }
.pn-group .label { font-size: var(--fs-11); font-weight: 700; letter-spacing: 0.1em; text-transform: uppercase; color: var(--text-4); }
.pn-group .chip { max-width: 100%; height: auto; min-height: 34px; padding: 7px 14px; white-space: normal; text-align: left; }
.pn-rates { display: flex; flex-wrap: wrap; gap: 6px; }
/* A group's options: stacked in the drawer, a wrapping row on a watch page — the
   same shape as the speed chips, so every group reads the same way. */
.pn-chips { display: flex; flex-direction: column; align-items: flex-start; gap: 8px; }
.lay-phone .panel .pn-chips, .lay-tablet .panel .pn-chips {
  flex-direction: row;
  flex-wrap: wrap;
  gap: 6px;
}
.lay-phone .panel .pn-group .chip, .lay-tablet .panel .pn-group .chip { white-space: nowrap; }
.pn-reason { display: flex; gap: 8px; line-height: 1.55; margin-top: auto; padding-top: 16px; border-top: 1px solid var(--line-1); }

/* 弹幕 sliders: label and value share a line, the track takes the rest. */
.pn-range { display: flex; align-items: center; gap: 10px; width: 100%; max-width: 320px; }
.pn-range > span { flex: 0 0 62px; font-size: var(--fs-12); color: var(--text-3); }
.pn-range > input[type='range'] { flex: 1; min-width: 0; accent-color: var(--accent, #ff5c8a); }


/* 弹幕层: above the picture, below every control (which start at 4). */
.dm-layer {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  z-index: 3;
  pointer-events: none;
}

/* In fullscreen the settings are a bar down the side, not a full-width sheet: the
   picture stays behind it and it reads as a panel. On a phone the medium query
   above asks for 100% — a watch page has nothing to keep in view, but a fullscreen
   player has. */
.player.fs .panel { width: min(340px, 86vw); }

/* And its text scales with the window, so a short one — a phone on its side is
   390px tall — still shows every group without scrolling. Scrolling a settings list
   is also how a drag ends up on the volume slider on the way past, so "it all
   fits" is a gesture fix as much as a layout one. `overflow-y: auto` stays as the
   fallback for a file with more tracks than any window has room for. */
.player.fs .panel {
  gap: clamp(7px, 1.9vh, 18px);
  padding: clamp(10px, 2.4vh, 20px) clamp(12px, 3vw, 20px) clamp(12px, 2.6vh, 24px);
  font-size: clamp(11px, 2.05vh, 13.5px);
}
.player.fs .panel .pn-group { gap: clamp(3px, 1vh, 8px); }
.player.fs .panel .pn-group .label { font-size: clamp(8.5px, 1.7vh, 11px); }
.player.fs .panel .pn-group .chip {
  min-height: 0;
  padding: clamp(3px, 1.05vh, 7px) clamp(8px, 1.8vw, 14px);
  font-size: inherit;
}
.player.fs .panel .pn-chips {
  flex-direction: row;
  flex-wrap: wrap;
  gap: clamp(4px, 1.1vh, 8px);
}
.player.fs .panel .pn-group .chip { white-space: nowrap; }
.player.fs .panel .pn-reason { padding-top: clamp(6px, 1.6vh, 16px); line-height: 1.4; }

.slide-enter-active, .slide-leave-active { transition: transform var(--t-mid), opacity var(--t-mid); }
.slide-enter-from, .slide-leave-to { transform: translateX(24px); opacity: 0; }

/* ============================ phone: the watch page ======================= */
/* Picture on top, its controls directly under it, then the title, the episode
   grid and the settings — the whole thing scrolls, YouTube/Bilibili style. */
.lay-phone .watch { display: block; overflow-y: auto; overscroll-behavior: contain; }
.lay-phone .wmain { display: block; }
.lay-phone .stage { display: block; flex: none; }
.lay-phone .frame { position: relative; inset: auto; width: 100%; aspect-ratio: 16 / 9; }
.lay-phone .top-bar { padding: max(10px, env(safe-area-inset-top)) 10px 10px; background: none; }
.lay-phone .controls {
  position: static;
  inset: auto;
  padding: 2px 8px 6px;
  background: var(--bg-1);
  border-bottom: 1px solid var(--line-1);
}
.lay-phone .big-play { width: 62px; height: 62px; }
.lay-phone .error-panel, .lay-tablet .error-panel { padding: 16px; gap: 7px; }
.lay-phone .error-panel svg, .lay-tablet .error-panel svg { display: none; }
.lay-phone .cs-hint { padding: 10px 12px; }
.lay-phone .winfo { display: block; padding: 14px 16px calc(30px + env(safe-area-inset-bottom)); }
.lay-phone :deep(.toast-host) { bottom: calc(18px + env(safe-area-inset-bottom)); }

/* ============================ tablet: sidebar on the right ================ */
/* Picture top-left, its controls below it, episodes down the right-hand side. */
.lay-tablet .watch { display: grid; grid-template-columns: minmax(0, 1fr) min(340px, 34vw); overflow: hidden; }
.lay-tablet .watch.no-side { grid-template-columns: minmax(0, 1fr); }
.lay-tablet .wmain { display: block; overflow-y: auto; }
.lay-tablet .stage { display: block; flex: none; }
.lay-tablet .frame { position: relative; inset: auto; width: 100%; aspect-ratio: 16 / 9; }
.lay-tablet .watch.no-side .stage { max-width: 1040px; margin: 0 auto; }
.lay-tablet .top-bar { padding: max(14px, env(safe-area-inset-top)) 16px 20px; background: none; }
.lay-tablet .controls {
  position: static;
  inset: auto;
  padding: 4px 12px 10px;
  background: var(--bg-1);
  border-bottom: 1px solid var(--line-1);
}
.lay-tablet .winfo { display: block; padding: 18px 20px calc(34px + env(safe-area-inset-bottom)); }
.lay-tablet :deep(.toast-host) { bottom: calc(22px + env(safe-area-inset-bottom)); }

/* The page's 设置 tab: the drawer's groups, flowed into the page instead.
   Not in fullscreen, where there is no page to flow them into: there the panel is
   the right-hand drawer again. That is not only cosmetic — as a static block its
   rect sat underneath the absolutely positioned control bar, so the picture's own
   controls won the hit test where they overlapped: a drag down the settings list
   landed on the volume slider, and a drag across it on the seek bar. */
.lay-phone:not(.fs) .panel, .lay-tablet:not(.fs) .panel {
  position: static;
  inset: auto;
  width: auto;
  padding: 0;
  border: 0;
  border-radius: 0;
  background: none;
  box-shadow: none;
  backdrop-filter: none;
  -webkit-backdrop-filter: none;
  overflow: visible;
  gap: 16px;
}

/* Loading, under the picture: the bar carries a real width once segments are
   being generated, and sweeps while there is nothing to measure yet. */
.wi-loading { display: flex; flex-direction: column; gap: 10px; padding: 14px 0 4px; }
.wl-bar {
  position: relative;
  height: 4px;
  border-radius: var(--r-pill);
  background: rgba(255, 255, 255, 0.12);
  overflow: hidden;
}
.wl-bar i {
  position: absolute;
  top: 0; bottom: 0; left: 0;
  border-radius: inherit;
  background: var(--brand);
  transition: width 0.4s ease;
}
.wl-bar:not(.live) i { width: 32%; animation: wl-sweep 1.2s ease-in-out infinite; }
@keyframes wl-sweep { 0% { left: -32%; } 100% { left: 100%; } }
.wl-text { font-size: var(--fs-12); color: var(--text-3); }

.wi-topline { display: flex; align-items: center; gap: 10px; margin-bottom: 12px; }
.wi-topline .tiny { min-width: 0; }

.wi-title { font-size: var(--fs-20); font-weight: 700; letter-spacing: -0.02em; line-height: 1.4; margin: 0; }
.wi-meta { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-top: 8px; }
.wi-actions { display: flex; align-items: center; gap: 8px; margin-top: 14px; }
.wi-tabs { display: flex; align-items: center; gap: 2px; margin: 16px 0 14px; border-bottom: 1px solid var(--line-1); }
.wi-tabs button {
  position: relative;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 9px 12px;
  font-size: var(--fs-13);
  font-weight: 600;
  color: var(--text-3);
  transition: color var(--t-fast);
}
.wi-tabs button:hover { color: var(--text-1); }
.wi-tabs button.on { color: var(--text-1); }
.wi-tabs button.on::after {
  content: '';
  position: absolute;
  left: 10px; right: 10px; bottom: -1px;
  height: 2px;
  border-radius: 2px;
  background: var(--brand);
}
.wi-eps { display: grid; grid-template-columns: repeat(auto-fill, minmax(50px, 1fr)); gap: 8px; }
.wi-ep {
  height: 44px;
  border-radius: var(--r-s);
  border: 1px solid var(--line-1);
  background: var(--bg-2);
  color: var(--text-2);
  font-size: var(--fs-14);
  font-weight: 650;
  transition: background var(--t-fast), color var(--t-fast), border-color var(--t-fast);
}
.wi-ep:hover { background: var(--bg-3); color: var(--text-1); }
.wi-ep.on { background: var(--brand); border-color: transparent; color: var(--brand-ink); }
.wi-about { display: flex; flex-direction: column; gap: 14px; }
.wi-ov { margin: 0; font-size: var(--fs-14); line-height: 1.72; color: var(--text-2); max-width: 78ch; }
.wi-facts { display: flex; flex-wrap: wrap; gap: 8px; }

.wside {
  min-height: 0;
  overflow-y: auto;
  background: var(--bg-1);
  border-left: 1px solid var(--line-1);
  padding: 12px 10px calc(24px + env(safe-area-inset-bottom));
}
.ws-head { display: flex; align-items: center; justify-content: space-between; gap: 10px; padding: 4px 6px 10px; }
.ws-title { display: flex; align-items: center; gap: 8px; font-size: var(--fs-13); font-weight: 700; }
.ws-list { display: flex; flex-direction: column; gap: 4px; }
.ws-ep {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 7px;
  border-radius: var(--r-s);
  border: 1px solid transparent;
  text-align: left;
  color: var(--text-3);
  transition: background var(--t-fast), border-color var(--t-fast);
}
.ws-ep:hover { background: var(--bg-2); }
.ws-ep.on { background: var(--brand-tint); border-color: rgba(240, 169, 46, 0.28); }
.ws-ep.on svg { color: var(--brand); }
.ws-thumb {
  position: relative;
  flex: none;
  display: grid;
  place-items: center;
  width: 92px;
  aspect-ratio: 16 / 9;
  border-radius: var(--r-xs);
  overflow: hidden;
  background: var(--bg-3);
}
.ws-thumb :deep(img) { width: 100%; height: 100%; object-fit: cover; }
.ws-ph { font-size: var(--fs-16); font-weight: 700; color: var(--text-4); }
.ws-copy { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 3px; }
.ws-name { font-size: var(--fs-13); font-weight: 600; color: var(--text-2); }
.ws-ep.on .ws-name { color: var(--text-1); }

/* ============================ fullscreen ================================= */
/* The stage is the screen again, whatever page layout it came from. */
/* The stage fills the screen in fullscreen, whatever page layout it came from: the
   chain becomes a column that fills and stops scrolling, and the page's own
   furniture steps aside. All of this used to happen by itself, because the stage was
   the fullscreen element and everything else was outside the top layer — which is
   also exactly why the settings drawer could not be touched there. */
.player.fs .watch, .player.fs .wmain {
  display: flex;
  flex-direction: column;
  height: 100%;
  overflow: hidden;
}
.player.fs .stage { display: block; flex: 1; min-height: 0; max-width: none; margin: 0; overflow: hidden; }
/* The watch page's info block is a real box with padding on a small screen, and
   hidden children still leave its padding behind — 44px of it, which is exactly
   what stopped the stage from filling the screen. Contents means the settings
   drawer inside it keeps working while the block itself takes no room. */
.player.fs .winfo { display: contents; padding: 0; }
.player.fs .wi-topline, .player.fs .wi-title, .player.fs .wi-meta,
.player.fs .wi-actions, .player.fs .wi-tabs, .player.fs .wi-eps,
.player.fs .wi-about, .player.fs .wi-facts, .player.fs .wside { display: none; }
.player.fs .frame { position: absolute; inset: 0; width: auto; aspect-ratio: auto; }
.player.fs .controls {
  position: absolute;
  inset: auto 0 0 0;
  padding: 60px clamp(14px, 3vw, 34px) max(20px, env(safe-area-inset-bottom));
  background: linear-gradient(to top, rgba(3, 4, 6, 0.94) 10%, rgba(3, 4, 6, 0.6) 55%, rgba(3, 4, 6, 0));
  border-bottom: 0;
}
.player.fs .top-bar {
  padding: max(14px, env(safe-area-inset-top)) clamp(14px, 3vw, 34px) 34px;
  background: linear-gradient(to bottom, rgba(3, 4, 6, 0.86), rgba(3, 4, 6, 0));
}

/* ============================ misc ======================================= */
.hints {
  position: absolute;
  bottom: calc(104px + env(safe-area-inset-bottom));
  right: clamp(14px, 3vw, 34px);
  display: flex;
  gap: 14px;
  font-size: var(--fs-11);
  color: rgba(255, 255, 255, 0.42);
  transition: opacity var(--t-mid);
  z-index: 3;
  pointer-events: none;
}
.hide-ui .hints { opacity: 0; }
kbd {
  display: inline-grid; place-items: center;
  min-width: 18px; height: 18px;
  padding: 0 4px; margin-right: 4px;
  border-radius: 4px;
  border: 1px solid var(--line-2);
  background: rgba(255, 255, 255, 0.06);
  font-family: var(--font-mono);
  font-size: 10px;
  color: var(--text-3);
}

@media (max-width: 860px) {
  .hints { display: none; }
  .vol-slider { display: none; }
  .panel { width: 100%; border-left: none; }
}
@media (max-width: 620px) {
  .ctl-row .icon-btn { width: 38px; height: 38px; }
  .time { font-size: var(--fs-12); margin-left: 4px; }
  .big-play { width: 66px; height: 66px; }
}
</style>
