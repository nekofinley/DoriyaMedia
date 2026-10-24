<script setup>
import { ref, onMounted, onBeforeUnmount, computed, nextTick, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, detectClient, fmtTime, fmtSize, imgUrl, parseGenres } from '../api.js'
import AppIcon from '../components/AppIcon.vue'
import BiliBadge from '../components/BiliBadge.vue'
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
let subSeekGeneration = 0
let subSeekRaf = 0
let subSeekTimer = 0
let subSeekHidden = null // { track, el, subId } while one native track is repainting
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
/* Bilibili's own 弹幕设置, in its order: the areas its panel names, the five
 * font sizes, and the three types of comment — plus one thing Bilibili does not
 * offer, turning off the black outline. The stored numbers are what the renderer
 * takes — a fraction of the canvas, a multiplier, seconds to cross. */
const DM_AREAS = [
  { value: 0.25, label: '1/4 屏' },
  { value: 0.5, label: '半屏' },
  { value: 0.75, label: '3/4 屏' },
  { value: 1, label: '全屏' },
]
const DM_FONTS = [
  { value: 0.7, label: '极小' },
  { value: 0.85, label: '小' },
  { value: 1, label: '中' },
  { value: 1.2, label: '大' },
  { value: 1.45, label: '极大' },
]
const DM_TYPES = [
  { key: 'scroll', label: '滚动' },
  { key: 'top', label: '顶部' },
  { key: 'bottom', label: '底部' },
]
const DM_SPEED_MIN = 4
const DM_SPEED_MAX = 14
const DM_DEFAULTS = { opacity: 0.9, speed: 8, area: 0.5, font: 1 }

/** A stored number, or the default when nothing valid was ever written. */
function dmStored(key, fallback) {
  const raw = localStorage.getItem(key)
  const n = raw === null ? NaN : Number(raw)
  return Number.isFinite(n) ? n : fallback
}

/** An older build saved the area as a free slider position, which no chip could
 *  show as selected; snap it to the nearest choice the panel offers. */
function dmNearest(value, options) {
  let best = options[0].value
  let gap = Infinity
  for (const o of options) {
    const d = Math.abs(o.value - value)
    if (d < gap) { best = o.value; gap = d }
  }
  return best
}

/** One object so a toggle is one assignment; a type missing from storage (or an
 *  unreadable value) is on, which is the state the panel starts in. */
function dmStoredTypes() {
  const on = { scroll: true, top: true, bottom: true }
  try {
    const raw = JSON.parse(localStorage.getItem('dm:dmTypes') || 'null')
    if (raw && typeof raw === 'object') {
      for (const t of DM_TYPES) if (raw[t.key] === false) on[t.key] = false
    }
  } catch { /* a value an older build wrote is simply ignored */ }
  return on
}

const danmakuOn = ref(localStorage.getItem('dm:dmOn') !== '0')
const danmakuOpacity = ref(Math.min(1, Math.max(0, dmStored('dm:dmOpacity', DM_DEFAULTS.opacity))))
const danmakuSpeed = ref(Math.min(DM_SPEED_MAX, Math.max(DM_SPEED_MIN, dmStored('dm:dmSpeed', DM_DEFAULTS.speed))))
const danmakuArea = ref(dmNearest(dmStored('dm:dmArea', DM_DEFAULTS.area), DM_AREAS))
const danmakuFont = ref(dmNearest(dmStored('dm:dmFont', DM_DEFAULTS.font), DM_FONTS))
const danmakuTypes = ref(dmStoredTypes())
/** The black outline that keeps light text readable over a bright picture. On by
 *  default; off is for viewers who want plain type. It is also the part that
 *  would otherwise reach above the first lane, so the renderer gives every lane
 *  a small top margin as well (see TOP_PAD_RATIO in danmaku.js). */
const danmakuStroke = ref(localStorage.getItem('dm:dmStroke') !== '0')
const dmOpacityPct = computed(() => Math.round(danmakuOpacity.value * 100))
/** The slider reads 慢 → 快, so its value is the crossing time reversed: the
 *  right-hand end is the renderer's smallest number of seconds. */
const dmSpeedSlider = computed(() => DM_SPEED_MIN + DM_SPEED_MAX - danmakuSpeed.value)
const danmakuCount = ref(0)
const danmakuFailed = ref(false)
const danmakuUrl = computed(() => play.value?.danmaku_url || '')
/** Ready means there is something to draw; the control hides itself otherwise. */
const danmakuReady = computed(() => !!danmakuUrl.value && danmakuCount.value > 0 && !danmakuFailed.value)
/** Whether this file has 弹幕 at all: the two bar buttons answer only that. A
 *  source without them (every non-bilibili file) gets no button rather than a
 *  switch that can never do anything. */
const dmControl = computed(() => !!danmakuUrl.value && !danmakuFailed.value)
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
/** The 弹幕 popup, opened by its own button in the control bar. It is a separate
 *  surface from the settings drawer on purpose: the drawer is the picture, the
 *  tracks and the mode, and the bullet-comment options are the one group a viewer
 *  reaches for while watching — often enough to want it one tap from the picture,
 *  next to the switch that turns the layer on and off. */
const dmMenuOpen = ref(false)
/** The 选集 popup: the season's episodes, next to the subtitle button, so a viewer
 *  can jump to another episode without leaving the picture. Only the theater shows
 *  the button — a watch page already has its grid or sidebar under the picture. */
const epMenuOpen = ref(false)
const epListEl = ref(null)
// locked: in fullscreen, every control but the lock button stops answering taps,
// so a hand moving across the screen cannot pause, seek or leave by accident.
const locked = ref(false)
const fsActive = ref(false)
const pipActive = ref(false)
const scrubbing = ref(false)
const scrubTime = ref(0)
// A saved position the stream has not packaged yet, in file seconds: the playhead
// goes there once the packaging reaches it (see onLoadedMetadata).
const resumeAt = ref(0)
// Where playback last ran out of stream (see onEnded): one reconnect per position.
let endedAt = -1
const hoverTime = ref(null)
const hoverX = ref(0)
const forceMode = ref('')      // current override, reapplied when re-fetching
// burnSub: the subtitle id being overlaid onto the video. Burning re-encodes, so
// it is opt-in, and it changes the stream identity — hence a reload, not a toggle.
const burnSub = ref(0)
const probeElapsed = ref(0)    // seconds the background probe has been running
const stallSeconds = ref(0)    // seconds spent waiting on data mid-playback
const streamStatus = ref(null) // segment-generation progress from the server

/* --------------------------------------------------------------- 清晰度
 *
 * A bilibili episode carries several picture rungs. The source no longer fixes
 * one in the library settings: the player asks for the rung it wants with each
 * decision (and with every HLS request after it), and a source without a ladder
 * has nothing to choose from. The preference is kept in this browser, like the
 * volume and the subtitle language, because a viewing habit sticks.
 */
function qualityStored() {
  const n = Number(localStorage.getItem('dm:quality'))
  return Number.isFinite(n) && n > 0 ? n : 0
}
const qualityPref = ref(qualityStored())
const qMenuOpen = ref(false)
const qualityBusy = ref(false)
// switchAt: the file position a quality change or a resume must continue from.
// Both are a reload of the stream at that position: the server starts a session
// on the keyframe at or before it (its picture and sound are read from separate
// inputs, so the break has to be a keyframe), reports where that is as
// stream_start, and the player states the distance as `startPosition`. switchAt
// stays set until maybeResume has confirmed the playhead really is there.
const switchAt = ref(0)
const switchLabel = ref('')
// True when switchAt is a saved position being restored (not a quality change).
// The two share the seek machinery, but the wording must not call a resume
// a switch.
const switchIsResume = ref(false)
// packaged: the server will produce nothing more for this stream — its playlist
// carries the end marker. It is the only state in which waiting longer cannot
// help, so it is what the loading text reads instead of the produced timeline.
const packaged = ref(false)
// Whether playback should resume by itself once the new stream is in place. A
// viewer who switched while paused expects the new rung at the same frame, not
// a picture that starts moving on its own.
let switchAutoPlay = true
// Whether the saved position has been looked at for this playback. A quality
// switch restores the position itself, and hls.js re-attaches the media (firing
// another loadedmetadata) after a seek; fetching progress on every attach can
// apply an old position in the middle of playback — which then also becomes the
// target a switch starts from. Once per decision is enough.
let progressChecked = false
// Whether a completed session has been asked to reload its playlist. hls.js
// keeps the level details it parsed while the playlist was live; a paused player
// does not refresh them by itself, so the element never learns the VOD duration
// and a seek is clamped. startLoad() fetches the details again (now ended).
let resumeReloaded = false
// The packaging job this playback belongs to, as the decision named it. The
// status endpoint reports one job per file; while a switch is catching up the
// abandoned rung may still be alive and further ahead, so its progress must not
// answer for the new stream.
const expectedVariant = ref('')

const qualities = computed(() => play.value?.qualities || [])
const effectiveQuality = computed(() => Number(play.value?.quality) || 0)
const qualityLabel = computed(() => {
  const q = qualities.value.find((x) => x.id === effectiveQuality.value)
  return q?.short || q?.label || ''
})
const qualityFullLabel = computed(() => {
  const q = qualities.value.find((x) => x.id === effectiveQuality.value)
  return q?.label || q?.short || ''
})
/** The bar shows the rung in use, and during a switch the one being fetched. */
const qualityButtonLabel = computed(() => {
  if (switchAt.value && switchLabel.value) return switchLabel.value
  return qualityLabel.value || '清晰度'
})

watch(qualityPref, (v) => localStorage.setItem('dm:quality', String(v || 0)))

const isIOS = computed(() => {
  const ua = navigator.userAgent
  return /iPhone|iPad|iPod/.test(ua) || (/Macintosh/.test(ua) && 'ontouchend' in document)
})
/** iOS gets the native control surface; everything else gets the custom chrome. */
const nativeControls = computed(() => isIOS.value)

/**
 * Whether this browser's *own* HLS player should be used instead of hls.js.
 *
 * Safari's is better than hls.js here — it decodes the fMP4 a session writes, it
 * honours the tfdt the server shifts onto a mid-file session, and it plays
 * subtitles as renditions — and on iOS there is nothing else. Chromium also
 * accepts a playlist URL, but its player cannot seek a playlist that is still
 * growing (its `seekable` stays empty until the end marker), which would undo the
 * whole point of a session starting at the viewer's position. So the native
 * player is kept for WebKit, and MediaSource browsers get hls.js.
 */
const nativeHlsPreferred = computed(() => {
  const ua = navigator.userAgent
  if (isIOS.value) return true
  return /Safari\//.test(ua) && !/Chrome\/|Chromium\/|Edg\/|OPR\//.test(ua)
})

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
  // And so did the buttons the two popups hang off: a rotation or a resize that
  // turns the theater into a watch page (or the other way round) would otherwise
  // leave an open popup anchored to where its button used to be.
  menuOpen.value = false
  dmMenuOpen.value = false
  epMenuOpen.value = false
  qMenuOpen.value = false
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
  isPlaying.value && !menuOpen.value && !dmMenuOpen.value && !epMenuOpen.value &&
  !qMenuOpen.value && !panelOpen.value)

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

/**
 * How much of the wait is done, in seconds of timeline.
 *
 * `generated_seconds` is measured from the session's own first segment, and a
 * two-stream session starts at a keyframe at or before the viewer's position —
 * not at the file's beginning. So the amount that has to be produced is the
 * distance from the stream's start to the target, and comparing the two directly
 * (as this used to) made the bar run to 100% while the wait had barely begun.
 */
const streamGoal = computed(() => {
  const target = switchAt.value || resumeAt.value
  if (target > 0) return Math.max(1, target - streamStart.value)
  return streamNeeded
})

const streamPct = computed(() => {
  const s = streamStatus.value
  if (!s || !s.generated_seconds) return 0
  // A quality switch or a resume has a definite target — where the viewer was —
  // so the bar measures against that rather than against the few seconds a start
  // needs.
  return Math.min(100, (s.generated_seconds / streamGoal.value) * 100)
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
  if (switchAt.value > 0) {
    const got = Math.max(0, Number(streamStatus.value?.generated_seconds) || 0)
    const label = switchIsResume.value
      ? `回到 ${fmtTime(switchAt.value)}`
      : (switchLabel.value ? `切换到 ${switchLabel.value}` : '切换清晰度')
    // Both numbers are in the stream's own clock: `got` is what the session has
    // produced since its start, and the goal is the distance from that start to
    // where the viewer is going. Reporting the absolute target here (as this used
    // to) showed "0:00 / 2:00" for a stream that had already produced a minute.
    const goal = streamGoal.value
    // The wait is over once the target itself has been produced: from there on the
    // picture is the player's business — reading the playlist, fetching the
    // fragment, seeking — not the packager's. The end marker is no longer what is
    // being waited for, so "已生成 X / Y" turning into "正在加载画面" is the honest
    // transition, whether or not the episode is fully packaged yet.
    if (got >= goal || packaged.value) return `正在${label} · 正在加载画面…`
    return `正在${label} · 已生成 ${fmtTime(got)} / ${fmtTime(goal)}`
  }
  if (resumeAt.value > 0 && play.value?.stream_variant) {
    const got = Math.max(0, Number(streamStatus.value?.generated_seconds) || 0)
    if (streamStatus.value?.complete || got + streamStart.value >= resumeAt.value) {
      return `正在回到 ${fmtTime(resumeAt.value)}…`
    }
    return `正在生成到 ${fmtTime(resumeAt.value)} · 已就绪 ${fmtTime(got)}`
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
// Every layout asks now — the theater's 选集 popup needs the list just as the
// watch page's grid and sidebar do.
const queue = ref(null)   // { season, show, episodes }
const meta = ref(null)    // the item behind the current file (overview, air date…)
let queueFor = 0          // item id the queue was loaded for
let queueBusy = false
let queueAgain = false

async function loadQueue() {
  const itemId = play.value?.item_id
  if (!itemId || queueFor === itemId) return
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
  epMenuOpen.value = false
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
  if (panelInline.value) { tab.value = 'settings'; menuOpen.value = false; dmMenuOpen.value = false; epMenuOpen.value = false; qMenuOpen.value = false; return }
  if (panelOpen.value && panelMode.value === mode) { panelOpen.value = false; return }
  panelMode.value = mode
  menuOpen.value = false
  dmMenuOpen.value = false
  epMenuOpen.value = false
  qMenuOpen.value = false
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
  dmMenuOpen.value = false
  epMenuOpen.value = false
  qMenuOpen.value = false
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

async function loadDecision(force, at) {
  error.value = ''
  fatal.value = ''
  isLoading.value = true
  // A fresh decision starts a fresh stream, so the saved position is looked at
  // again — except for a quality switch, which carries the viewer's position in
  // switchAt and must not have it overwritten by the progress saved a moment ago.
  if (switchAt.value === 0) progressChecked = false
  resumeReloaded = false
  packaged.value = false
  forceMode.value = force || ''
  stopProbePoll()
  try {
    play.value = await fetchPlay(at)
  } catch (e) {
    fatal.value = e.message
    isLoading.value = false
    return
  }
  // Where the stream begins in file time (0 unless a session packages from a
  // resume position). Everything the UI and the API show is file time, so this is
  // the offset between the element's clock and the file's.
  streamStart.value = Number(play.value?.stream_start) || 0
  expectedVariant.value = String(play.value?.stream_variant || '')
  // A switch clears what the previous rung's packager had produced: until a
  // status for the new variant arrives, the wait must not be measured against
  // the abandoned job.
  if (switchAt.value > 0) streamStatus.value = null
  // A stored rung that this episode does not have is not an error: the server
  // falls back to automatic and says which rung it used. Drop the preference so
  // the menu agrees with the picture instead of marking a choice that cannot play.
  const rungs = play.value?.qualities || []
  if (qualityPref.value > 0 && rungs.length && !rungs.some((q) => q.id === qualityPref.value)) {
    qualityPref.value = 0
  }
  // The episode list does not depend on the decision, so ask for it in parallel
  // with the (slow) probe rather than after it.
  loadQueue()
  if (unplayable()) return
  if (play.value?.state === 'probing') {
    startProbePoll()
    return
  }
  // A two-stream source (bilibili) starts its session on a keyframe at or before
  // the saved position, so the packager only has to produce from there — the wait
  // is a few segments, not the whole episode (see warmSession). The resume is
  // taken on the same path as a quality switch because both need the element
  // attached *at* a position: `startPosition` is what decides the first fragment,
  // and it has to be known before hls.js reads the playlist.
  if (!switchAt.value && play.value?.stream_variant && Number(play.value?.resume_at) > 0) {
    switchAt.value = Number(play.value.resume_at)
    switchIsResume.value = true
    switchLabel.value = ''
    switchAutoPlay = true
    streamStatus.value = null
  }
  // Start the packaging with a plain playlist request and wait for it to pass the
  // viewer's position before attaching, so every fragment the element may need is
  // already listed. That is a few segments — not the whole episode, and not the
  // end marker (see warmSession).
  if (switchAt.value > 0 && play.value?.stream_variant) {
    await warmSession(play.value.url, switchAt.value)
  }
  await nextTick()
  attachSource()
}

/**
 * How far past the resume point the packaging must be before the element is
 * attached: one fragment.
 *
 * Not a safety margin — it is what makes `startPosition` land. A growing EVENT
 * playlist is only as long as what ffmpeg has written, and hls.js clamps a start
 * position to that (measured: with the target fragment not listed yet, asking to
 * start at 10s started at the playlist's end, 8.13s, and never came back). One
 * fragment past the target guarantees the fragment holding it is listed, and
 * waiting any longer is time the viewer spends looking at a spinner: on a 4K
 * episode each fragment is several seconds of wall time.
 *
 * The real fragment length is measured from the session's own progress rather
 * than assumed: ffmpeg cuts on keyframes, so a 4s setting produces 5s segments on
 * most bilibili episodes, and a hard-coded guess either waits too long or clamps.
 */
const warmAheadFragments = 1

/**
 * How long the packaging has to be ahead, in seconds, given what it has produced.
 */
function warmAhead(stream) {
  const generated = Number(stream?.generated_seconds) || 0
  const segments = Number(stream?.segments_ready) || 0
  const fragLen = segments > 0 && generated > 0 ? generated / segments : 5
  return Math.max(1, fragLen * warmAheadFragments)
}

/**
 * Start a switched stream's packaging without attaching the element, and wait
 * until it has produced the position being restored.
 *
 * Not the whole episode. A growing EVENT playlist used to look unusable — hls.js
 * pins the playhead to the live edge when no start position is given, and that was
 * taken to mean it ignores `startPosition` too. It does not: with a start position
 * stated relative to the stream's own beginning, hls.js seeks into an EVENT
 * playlist at that position exactly (measured, hls.js 1.7.3: startPosition 10 on a
 * session packaging from 20s landed at 10 and played on), and it keeps following
 * the playlist as it grows. So the wait is only until the target fragment exists —
 * plus one fragment, so that it is listed rather than clamped (see warmAhead).
 *
 * With the server starting a two-stream session on the keyframe at or before the
 * saved position (see media.Engine.PackStart), a resume twenty minutes in waits
 * for the few seconds between that keyframe and the viewer's position — not for
 * the rest of the episode.
 *
 * Progress is read for this stream only (`variant`), because the abandoned rung's
 * packager is still alive and is usually further ahead.
 *
 * Best effort: a timeout attaches anyway and lets the normal waiting logic take
 * over.
 */
async function warmSession(url, target) {
  if (!url || !target || !expectedVariant.value) return
  let started = false
  try {
    const res = await fetch(url, { credentials: 'same-origin' })
    started = res.ok
  } catch { /* the poll will report nothing; attach and let normal handling run */ }
  if (!started || disposed) return
  const query = `?variant=${encodeURIComponent(expectedVariant.value)}`
  // Packaging copies the streams rather than re-encoding them, so it runs far
  // faster than playback; how long the whole episode would take is set by how fast
  // the source delivers it. This waits for a few segments, so the deadline is only
  // a backstop against a packager that never starts.
  const deadline = Date.now() + 15 * 60 * 1000
  // Packaging starts at the session's own first segment, so progress is measured
  // from there: what has to be produced is the distance from the stream's start to
  // the position being restored, not the position itself.
  const need = Math.max(0, Number(target) - streamStart.value)
  let sawSession = false
  let restarts = 0
  while (!disposed && Date.now() < deadline) {
    let st = null
    try { st = await api.get(`/api/play/${route.params.fileId}/status${query}`) } catch { /* transient */ }
    if (disposed) return
    if (st && st.stream) {
      sawSession = true
      streamStatus.value = st.stream
      const generated = Number(st.stream.generated_seconds) || 0
      // The end marker is the one case where waiting cannot help: the packager is
      // done, and whatever it produced is all there will ever be. `packaged` is the
      // "nothing left to wait for" flag the loading text reads.
      if (st.stream.complete) {
        packaged.value = true
        return
      }
      // Enough to start at the target. See warmAhead for why exactly one fragment
      // past it — no more, which is time spent waiting, and no less, which is a
      // start position hls.js has to clamp.
      if (generated >= need + warmAhead(st.stream)) return
      // The packager stopped without finishing (a stalled source, a refused link).
      // Asking for the playlist again starts a fresh job in its place, so the wait
      // goes on from there; a bounded number of tries, then the attach takes over.
      if (!st.stream.packager_running && !st.stream.complete) {
        if (++restarts > 4) return
        try { await fetch(url, { credentials: 'same-origin' }) } catch { /* the next poll reports it */ }
      }
    } else if (sawSession) {
      // The job finished and its cache was torn down (caching disabled): there is
      // no session left to warm, so attaching will start a new one. Waiting for
      // one that will never appear only delays the picture.
      return
    }
    await new Promise((r) => setTimeout(r, 600))
  }
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

function fetchPlay(at) {
  const params = new URLSearchParams({ client: detectClient() })
  if (forceMode.value) params.set('force', forceMode.value)
  if (burnSub.value) params.set('burn', String(burnSub.value))
  if (currentAudio.value) params.set('a', String(currentAudio.value))
  if (clientCaps) params.set('caps', clientCaps)
  // The rung the viewer picked (0 = automatic, which is what leaving it out
  // means). Only a source with a ladder notices it.
  if (qualityPref.value > 0) params.set('q', String(qualityPref.value))
  // An explicit seek position, in file seconds. The server answers with the session
  // boundary it snapped to (stream_start), so the element's clock stays aligned.
  if (at > 0) params.set('at', String(Math.floor(at)))
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
      const q = expectedVariant.value ? `?variant=${encodeURIComponent(expectedVariant.value)}` : ''
      try { st = await api.get(`/api/play/${route.params.fileId}/status${q}`) } catch { /* transient */ }
      if (disposed) return
      if (st && st.stream) {
        // While a quality change is catching up the previous rung's packager is
        // still alive, and it is probably further ahead. Its progress describes
        // a stream this player is no longer on.
        const want = expectedVariant.value
        if (!want || !st.stream.variant || st.stream.variant === want) {
          streamStatus.value = st.stream
        }
      } else if (st && !st.stream && !isLoading.value) {
        streamStatus.value = null
      }
      // A quality change (or a saved position) can be waiting for the packaging
      // to reach a point. The poll is the only clock that keeps running while
      // the picture is paused for that wait, so the jump happens from here.
      maybeResume()
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
  // A saved position that arrived after the element was attached (see
  // onLoadedMetadata) parks the picture until the packaging reaches it: whatever
  // the element reports, the viewer is not watching yet. The pending v.play() from
  // startCand can fire after that pause, so the hold is enforced here too.
  if (holdForSession()) { videoEl.value?.pause(); return }
  isLoading.value = false
  clearStall()
  kickDanmaku()
}

// Enough data has arrived to keep playing on its own. This settles the loading
// state whatever the earlier events said, and it is the one signal that also
// works while the element is paused (a viewer who pauses mid-stall should not be
// left looking at "正在缓冲" after the data has turned up).
function onCanPlay() {
  const v = videoEl.value
  if (!v || v.readyState < 3) return
  if (holdForSession()) { v.pause(); return }
  isLoading.value = false
  clearStall()
}

/**
 * The picture is parked on purpose, waiting for a position the stream cannot
 * reach yet.
 *
 * A switch or resume normally attaches at the viewer's position
 * (`startPosition`), so there is nothing to hold. But hls.js clamps that
 * position when the target fragment has not reached the playlist, and a failed
 * seek can fall back to the opening. Playing that opening while `switchAt` keeps
 * the progress bar and danmaku at the target is the exact desync this guard
 * prevents: if the element is still behind the pending target, keep it paused
 * until maybeResume has moved it there.
 */
function holdForSession() {
  const v = videoEl.value
  const target = switchAt.value || resumeAt.value
  if (!v || !target || !play.value?.stream_variant) return false
  return Number(v.currentTime) + streamStart.value < target - 1
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
  if (holdForSession()) return
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
// The last thing the server said about a failure, if it said anything. A 502 from
// the packager ("packaging index.m3u8 failed: ...") explains far more than "decode
// or network error" can, and it is the difference between a bug report that can be
// acted on and one that cannot.
let lastServerReason = ''

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
    fatal.value = `所有播放方式都失败了(已尝试:${tried})。可以换一条音轨,或手动指定一种方式重试。` +
      (lastServerReason ? `\n服务端说:${lastServerReason}` : '')
    return
  }
  if (candFor !== i) { candFor = i; candTries = 0; candMeta = false; lastServerReason = ''; resumeAt.value = 0 }
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
  // Stop whatever was playing *before* the new source is attached. `load()` after
  // hls.js has already put its MediaSource on the element resets the element under
  // it: the seek hls.js makes to `startPosition` is discarded and playback starts
  // at the beginning of the session instead (measured on a mid-episode resume —
  // the element began at 0 while the playlist said fragment 2).
  v.removeAttribute('src')
  v.load()
  cancelSubtitleRestore(false)
  activeSub.value = null

  if (c.kind === 'hls') {
    // Whose HLS player to use is not only "can this element play a playlist".
    // Safari's own player is worth having — it plays the fMP4 the session writes,
    // it follows a keyframe'd stream's timestamps, and its subtitles arrive as
    // renditions (see native_subtitles) — and on iOS there is no alternative.
    //
    // A Chromium build also answers `canPlayType` with "maybe", but its native
    // player cannot seek into a playlist that is still growing: `seekable` stays
    // empty until the end marker, so a resume starts at the beginning of the
    // session instead of at the viewer's position (measured: the element played
    // from 0 for ten seconds while the playlist listed the target fragment). That
    // is precisely the wait this player exists without, so every browser with
    // MediaSource gets hls.js.
    if (nativeHlsPreferred.value && v.canPlayType('application/vnd.apple.mpegurl')) {
      v.src = src
    } else {
      if (!HlsLib) HlsLib = (await import('hls.js')).default
      const Hls = HlsLib
      if (Hls.isSupported()) {
        // A resume or a quality switch is a packaging job that starts at its own
        // stream position (a keyframe, for a two-stream source), which the server
        // reported as stream_start. The element's clock is relative to that, so the
        // position to ask for is the distance from it. `startPosition` is what
        // decides the very first fragment: calling startLoad() after MANIFEST_PARSED
        // was too late, and the opening had already been requested and appended
        // (measured: seg00000 plus the target fragment, playhead stuck at 0).
        const startPos = switchAt.value > 0 ? Math.max(0, switchAt.value - streamStart.value) : -1
        const hlsConfig = {
          maxBufferLength: 30,
          manifestLoadingTimeOut: 30000,
          manifestLoadingMaxRetry: 4,
          levelLoadingTimeOut: 30000,
          fragLoadingTimeOut: 120000,
          fragLoadingMaxRetry: 8,
          fragLoadingRetryDelay: 1000,
          nudgeMaxRetry: 10,
        }
        if (startPos > 0) hlsConfig.startPosition = startPos
        hls.value = new Hls(hlsConfig)
        hls.value.loadSource(src)
        hls.value.attachMedia(v)
        hls.value.on(Hls.Events.ERROR, onHlsError)
      } else if (v.canPlayType('application/vnd.apple.mpegurl')) {
        // No MediaSource, but the element can play a playlist: last resort.
        v.src = src
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
  // `startPosition` puts the playhead where the viewer is, so a resume plays on
  // by itself — but a rung picked while paused must stay paused on the same frame,
  // and starting the element is what would break that.
  if (switchAt.value === 0 || switchAutoPlay) {
    v.play().catch(() => { /* autoplay may be blocked — user presses play */ })
  }
  showControls()
}

/** The server says it is still producing this stream. */
function serverPreparing() {
  const s = streamStatus.value
  if (!s) return false
  if (Number(s.segments_ready) > 0) return false   // there is something to play
  // A packager that is still alive is still working. The first segments of a 4K
  // file on a cold cloud drive take far longer to produce than any fixed timeout
  // worth applying here, and giving up on them means showing a failure card for a
  // stream that was about to start — the retry the viewer then presses works only
  // because it waited longer.
  if (s.packager_running) return true
  // No segment out, nothing running and the packager was asked for one recently:
  // it is working. Once it has been silent for a while, it is not.
  return !(Number(s.idle_seconds) > 10)
}

/** Is this failure "not ready yet" rather than "the wrong mode"? */
function keepWaiting(code) {
  const c = cands.value[candIdx.value]
  if (!c) return false
  const waited = Date.now() - candSince
  // The patience window is about *starting*: a stream that never produced a
  // picture and has been retried for a minute is not going to. One that was
  // already playing is a different case — a hiccup mid-playback is worth another
  // attempt however long it has been running, since a mode switch reloads the
  // whole stream (and jumps back to the last saved position).
  if (!candMeta && waited > CAND_PATIENCE_MS) return false
  // A decode error normally means the data arrived and the decoder refused it — a
  // verdict on the codec, not on the mode. Before the first frame, though, it is
  // also what a fragment that arrived cut short looks like, so while the server is
  // still producing, give it the benefit of the doubt; once it has stopped, the
  // decoder's verdict stands.
  if (code === 3) return !candMeta && (serverPreparing() || candTries < 1)
  // Network-flavoured errors are worth another attempt whatever the mode.
  if (code === 1 || code === 2) return candTries < RETRY_DELAYS.length
  // SRC_NOT_SUPPORTED is ambiguous: a native-HLS player (iOS) reports it for a
  // manifest whose first segments are still being produced, and that is a verdict
  // on nothing. While the server is still packaging, waiting is not "retrying a
  // broken mode" — it is the only honest thing to do, and the alternative is a
  // failure card for a stream that is about to start. Past that, an HLS attempt
  // gets a minimum dwell either way, because its first segment is the slowest
  // thing here.
  if (c.kind !== 'hls' || candMeta) return false
  if (serverPreparing()) return true
  return candTries < RETRY_DELAYS.length && waited < HLS_DWELL_MS
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
  // Keep the server's own words before acting on the failure.
  const said = data.response?.text
  if (said) lastServerReason = String(said).slice(0, 200)
  if (data.type === HlsLib.ErrorTypes.MEDIA_ERROR && !mediaRecovered) {
    mediaRecovered = true
    hls.value?.recoverMediaError()
    return
  }
  if (data.type === HlsLib.ErrorTypes.NETWORK_ERROR && !netRestarted) {
    netRestarted = true
    // startLoad() without a position means "start from the earliest point" in
    // hls.js. On a failed seek that silently moves the picture and sound to the
    // opening while switchAt keeps the progress bar and danmaku at the target.
    // Restart the loader at the position the viewer is actually waiting for.
    const target = switchAt.value || resumeAt.value || position.value
    const at = target > 0 ? target - streamStart.value : (videoEl.value?.currentTime || 0)
    hls.value?.startLoad(Math.max(0, at))
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

/**
 * The file's duration, from whichever source actually knows it.
 *
 * hls.js reports `Infinity` until it has parsed the level's duration, and
 * `loadedmetadata` can fire before that. Reading it once and keeping it made every
 * later calculation meaningless: the duration read 0:00, the buffered bar stayed
 * empty, and a drag computed an infinite target that seeking then threw away — a
 * progress bar that cannot be moved. The element's duration is not final at that
 * moment either, so this also runs on durationchange; the decision's own duration
 * (the whole file's, straight from the probe) is the honest fallback.
 */
function applyDuration() {
  const v = videoEl.value
  if (!v) return
  const d = Number(v.duration)
  const known = Number(play.value?.duration) || 0
  if (isFinite(d) && d > 0) {
    // While a session is packaging, the element's duration is only what has been
    // packaged so far — it grows. Taking it as the file's length would show a
    // shrinking total (32:03 becoming 1:36) and, worse, make the player treat the
    // packaging edge as the end of the episode. The decision's duration is the
    // file's; the element's is only ever better when it is larger.
    duration.value = known > 0 ? Math.max(known, d + streamStart.value) : d + streamStart.value
  } else if (known > 0) {
    duration.value = known
  }
}

/**
 * How far into the file there is actually something to play, in file time.
 *
 * Deliberately not `seekable`: a packaged stream is served as an event playlist
 * with no end yet, so the element reports its seekable range as [0, Infinity]. That
 * number means "you may ask for this", not "this exists" — and asking is precisely
 * how a player ends up with nothing: it jumps to a position the packager has not
 * reached, waits for fragments that are minutes away, and shows no picture, no
 * progress and no duration in the meantime.
 *
 * What exists is what has been buffered, plus what the server says it has finished
 * producing (segment generation progress, which the session path reports too).
 */
function reachable() {
  let reach = streamStart.value
  const v = videoEl.value
  if (v && v.buffered.length) {
    reach = Math.max(reach, v.buffered.end(v.buffered.length - 1) + streamStart.value)
  }
  const gen = Number(streamStatus.value?.generated_seconds)
  if (isFinite(gen) && gen > 0) reach = Math.max(reach, streamStart.value + gen)
  return reach
}

/**
 * Move to a position the stream has now packaged: a saved resume, or the point
 * a 清晰度 switch was waiting for.
 *
 * This is the safety net, not the normal path anymore. A resume or a switch that
 * knows where it is going states it as `startPosition`, and hls.js seeks there by
 * itself while the playlist is still growing (see warmSession). What is left here
 * is (a) the correction when that seek did not land — hls.js clamps a start
 * position to what the playlist listed, and the element can end up short of the
 * target — and (b) positions that arrive from elsewhere, like a saved progress a
 * moment after the first attach, or the edge of the packaging (onEnded).
 *
 * The element is paused for neither: there is no longer anything to hold at zero
 * for, since the stream begins where the viewer is.
 */
function maybeResume() {
  const v = videoEl.value
  const target = switchAt.value || resumeAt.value
  if (!v || !target || !candMeta) return
  if (v.readyState < 2) return
  const switching = switchAt.value > 0
  // A target the picture has already passed is spent, not pending. This is what a
  // resume leaves behind once playback is running: the target was where the viewer
  // came back to, and the playhead is now beyond it. Seeking to it again would drag
  // the picture backwards — the "playback jumps to another scene" a viewer sees
  // after pausing and playing again. Measured: paused at 130s, played, and the
  // playhead snapped back to the 120s resume point.
  if (!switching && target <= v.currentTime + streamStart.value + 0.5) {
    resumeAt.value = 0
    return
  }
  // A target ahead of the playhead is only reachable once the fragments exist, and
  // `reachable` below is the one place that decides whether they do: it reads what
  // has been buffered plus what the server reports as produced. Waiting for the end
  // marker first (as this used to) made the jump wait for the whole episode, which
  // is the wait this player exists without — the playlist being EVENT is not a
  // reason to hold, only the target not being produced yet is.
  if (play.value?.stream_variant && target > v.currentTime + streamStart.value + 0.5) {
    // The playlist is VOD now, but the element learns the whole duration only
    // after hls.js parses it. Seeking before that clamps the target to the few
    // seconds it has buffered, and the picture starts there instead.
    const dur = Number(v.duration)
    if (!isFinite(dur) || dur + streamStart.value < target) {
      // The details hls.js holds were parsed while the playlist was live, and a
      // paused element never refreshes them. Re-attach once: the ended playlist
      // is then read from scratch as VOD (with the target as its start position),
      // which is exactly the quality-switch path.
      if (!resumeReloaded) {
        resumeReloaded = true
        if (!switching) {
          switchAt.value = target
          switchIsResume.value = true
          switchLabel.value = ''
          resumeAt.value = 0
          switchAutoPlay = true
          isLoading.value = true
          attachSource()
        }
      }
      return
    }
  }
  if (target > reachable()) return
  const wasResumeSwitch = switchIsResume.value
  const label = switchLabel.value
  switchAt.value = 0
  switchLabel.value = ''
  switchIsResume.value = false
  resumeAt.value = 0
  v.currentTime = Math.max(0, target - streamStart.value)
  position.value = target
  isLoading.value = false
  if (!switching || switchAutoPlay) v.play().catch(() => {})
  toast.info(switching && label && !wasResumeSwitch
    ? `已切换到 ${label},从 ${fmtTime(target)} 继续`
    : `已从 ${fmtTime(target)} 继续播放`)
}

function onLoadedMetadata() {
  candMeta = true
  const v = videoEl.value
  // Everything here is reported in *file* time. A packaged session begins where
  // the viewer's saved position was, and players present that stream as starting
  // at zero (hls.js re-aligns it), so the element's own clock is offset from the
  // file by streamStart.
  applyDuration()
  const switchTo = switchAt.value
  position.value = switchTo > 0 ? switchTo : (Number(v.currentTime) || 0) + streamStart.value
  // Subtitles come on by themselves; the panel is there to change or turn them off.
  autoSelectSub()

  // A quality switch or a resume was attached *at* the viewer's position: the
  // decision put it in `startPosition` (see startCand), and hls.js seeks there
  // itself as soon as the first fragment lands. So nothing is held here — the wait
  // for the packaging is over before this element exists. What is left is the
  // element's own work: parse the playlist, fetch the fragment holding the target,
  // seek. `maybeResume` is the safety net if the seek did not land (a start
  // position hls.js had to clamp to what the playlist listed, a wrong fragment);
  // it corrects the playhead once the picture is really moving, and until then
  // `switchAt` keeps the "switching" wording on the overlay.
  if (switchTo > 0) {
    resumeAt.value = switchTo
    // A rung picked while paused stays paused: hls.js has already put the playhead
    // on the right frame, and `v.play()` in startCand was skipped for this case.
    // The overlay stays until maybeResume has confirmed the position (it clears
    // both switchAt and the loading state).
    if (!switchAutoPlay) v.pause()
    return
  }

  // Metadata is not "ready": a stream still waiting for its first segments keeps
  // its loading state (and its bar) until there is enough to play, or until
  // frames actually move.
  if (v.readyState >= 3) isLoading.value = false
  // The saved position belongs to the first attach of this playback; a later
  // metadata (a re-attach after a seek, or the second level hls.js builds) must
  // not apply it again over a picture that is already playing.
  if (progressChecked) return
  progressChecked = true
  api.get(`/api/progress?file_id=${route.params.fileId}`).then((p) => {
    if (!p || !(p.position > 30) || !(p.position < duration.value * 0.95)) return
    // A quality switch has already restored the viewer to this position (it is
    // saved before the switch). Applying the same saved progress again would only
    // queue a second "continue from…" over the picture, and while the new rung is
    // still settling it could pull the playhead back.
    if (Math.abs(position.value - p.position) < 3) return
    // A position the stream has not packaged yet cannot be jumped to: the fragments
    // do not exist, so the element stalls with nothing buffered and the playhead
    // appears to wander. Wait for the packaging to reach it instead — it runs many
    // times faster than playback — and go there the moment it does. The wait is
    // paused, so the opening is not played at a viewer who is resuming.
    if (p.position <= reachable()) {
      v.currentTime = Math.max(0, p.position - streamStart.value)
      position.value = p.position
      toast.info(`已从 ${fmtTime(p.position)} 继续播放`)
    } else {
      resumeAt.value = p.position
      position.value = p.position
      // This position arrived after the element was attached, so unlike a resume
      // taken from the decision it has no `startPosition` of its own — and holding
      // the picture is the honest thing while the packaging catches up, rather than
      // playing the opening at a viewer who asked to continue. maybeResume makes the
      // jump as soon as the target is produced.
      if (play.value?.stream_variant) {
        isLoading.value = true
        v.pause()
      }
      toast.info(`正在生成到 ${fmtTime(p.position)} 再继续播放…`)
      maybeResume()
    }
  }).catch(() => {})
}

/**
 * The element reached the end of what it knows about, which is not the same as the
 * end of the episode while a session is still packaging.
 *
 * A packaged stream arrives as an event playlist; when the packager finishes, the
 * list gains its end marker and the player switches from "live" to "on demand" —
 * and it takes the length it had buffered at that instant as the stream's length.
 * Playback then ends in the middle of an episode that is sitting finished on disk.
 * The picture also gets here simply by catching up with a packager that is not far
 * enough ahead yet. Either way, re-reading the playlist brings a timeline that
 * covers everything produced since.
 */
function onEnded() {
  const v = videoEl.value
  if (!v) return
  const known = Number(play.value?.duration) || 0
  const at = v.currentTime + streamStart.value
  // A real ending, or nothing was played at all: either way this is not a packaged
  // edge. The second case matters more than it looks — an element that has not
  // started reports "ended" too, and treating that as a packaging edge makes the
  // player reconnect in a loop and never play anything.
  if (known <= 0 || at < 5 || at >= known - 5) return
  // One attempt per position: if reconnecting lands on the same edge again, that
  // is the truth about this stream, not a reason to try forever.
  if (Math.abs(at - endedAt) < 30) return
  endedAt = at
  if (reachable() > at + 5) {
    // More exists than this stream knows about: start again with the fuller
    // timeline, and beginning where it stopped. The position goes through
    // `switchAt` because that is what becomes the element's `startPosition` — a
    // bare resumeAt is a plan to seek later, and the fresh attach drops it, which
    // put the picture back at the stream's start instead of the viewer's place.
    resumeAt.value = 0
    switchAt.value = at
    switchIsResume.value = true
    switchLabel.value = ''
    switchAutoPlay = true
    toast.info('继续播放…')
    attachSource()
  } else {
    // Nothing beyond this point exists yet. Wait for the packaging to pass it:
    // hls.js keeps re-reading the playlist and picks the fragments up itself.
    resumeAt.value = at
    toast.info('正在生成后面的部分…')
  }
}

function onErr() {
  isLoading.value = false
  if (!play.value) return
  // One failure can arrive twice: hls.js reports it, and the element fires error
  // for the same underlying cause. Acting twice would skip a candidate.
  const at = Date.now()
  if (at - errHandledAt < 500) return
  const code = Number(videoEl.value?.error?.code) || 0
  // An abort moments after attaching is the previous attempt being swallowed by
  // this one — a verdict on nothing. Ignoring it is not the same as failing it:
  // spending a retry here, or worse, showing the failure card, is how a working
  // stream gets declared broken before it has had a chance to start.
  if (code === 1 && at - candSince < 1500) return
  errHandledAt = at
  // Not every error is a verdict on the mode: a slow first segment looks exactly
  // like a broken one from here, and switching modes on it is how a working
  // strategy gets replaced by one this client can never play.
  if (keepWaiting(code)) { retryCand(code); return }
  if (candIdx.value < cands.value.length - 1) {
    toast.info('该方式播放失败,自动切换…')
    startCand(candIdx.value + 1)
    return
  }
  const tried = cands.value.map(candName).join(' → ')
  error.value = `播放失败(解码或网络错误)。已尝试:${tried}` +
    (lastServerReason ? `\n服务端说:${lastServerReason}` : '')
}

function onTimeUpdate() {
  const v = videoEl.value
  if (!v) return
  // Every tick is proof the picture is moving — the moment it starts, the
  // loading state goes away and stays away until it really stalls.
  markPlaying()
  // The element's clock is relative to the stream; everything the UI and the API
  // deal in is file time, so the session offset is added on the way out. While a
  // quality switch holds the picture at 0, the bar shows the position being waited
  // for; a saved-position resume keeps playing, so its bar follows the playhead.
  if (!scrubbing.value && !switchAt.value && (!resumeAt.value || !v.paused)) {
    position.value = v.currentTime + streamStart.value
  }
  if (v.buffered.length) buffered.value = v.buffered.end(v.buffered.length - 1) + streamStart.value
  // A saved position or a quality switch the stream had not reached yet (see
  // onLoadedMetadata and maybeResume): go there as soon as it is packaged.
  if (resumeAt.value || switchAt.value) maybeResume()
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
    fontScale: danmakuFont.value, types: danmakuTypes.value, stroke: danmakuStroke.value,
  })
}

function applyDanmakuOptions() {
  danmaku?.setOptions({
    opacity: danmakuOpacity.value, speed: danmakuSpeed.value, area: danmakuArea.value,
    fontScale: danmakuFont.value, types: danmakuTypes.value, stroke: danmakuStroke.value,
  })
  paintDanmaku()
}

function setDanmakuArea(value) {
  danmakuArea.value = value
  applyDanmakuOptions()
}

function setDanmakuFont(value) {
  danmakuFont.value = value
  applyDanmakuOptions()
}

function onDanmakuOpacity(e) {
  danmakuOpacity.value = Number(e.target.value) / 100
  applyDanmakuOptions()
}

function onDanmakuSpeed(e) {
  // The track runs 慢 → 快; the renderer stores the seconds a comment takes to
  // cross, so the two run opposite each other.
  danmakuSpeed.value = DM_SPEED_MIN + DM_SPEED_MAX - Number(e.target.value)
  applyDanmakuOptions()
}

function toggleDanmakuType(key) {
  danmakuTypes.value = { ...danmakuTypes.value, [key]: !danmakuTypes.value[key] }
  applyDanmakuOptions()
}

function toggleDanmakuStroke() {
  danmakuStroke.value = !danmakuStroke.value
  applyDanmakuOptions()
}

function resetDanmakuOptions() {
  danmakuOpacity.value = DM_DEFAULTS.opacity
  danmakuSpeed.value = DM_DEFAULTS.speed
  danmakuArea.value = DM_DEFAULTS.area
  danmakuFont.value = DM_DEFAULTS.font
  danmakuTypes.value = { scroll: true, top: true, bottom: true }
  danmakuStroke.value = true
  applyDanmakuOptions()
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
  if (danmakuOn.value) {
    // The layer was display:none and has this tick's box back only after the DOM
    // update, so the geometry and the frame are taken after it. One more frame is
    // painted straight away: a paused picture runs no loop, and the layer has to be
    // on the screen the moment it is switched back on, not at the next seek.
    nextTick(() => {
      danmaku?.resize()
      paintDanmaku()
      kickDanmaku()
    })
  } else {
    // Only the loop stops: the layer itself is hidden by the template, and the
    // comments stay loaded for the next time it is switched on. Dropping them here
    // would leave a blank layer until the page reloaded, because the loader hands
    // its list over once per URL.
    stopDanmaku()
  }
  toast.info(danmakuOn.value ? '弹幕已开启' : '弹幕已关闭', 1300)
}

/** The 弹幕 button: its own popup, not the settings drawer. Opening it closes the
 *  other popup, because both hang off the same corner of the control bar. */
function toggleDmMenu() {
  dmMenuOpen.value = !dmMenuOpen.value
  if (dmMenuOpen.value) { menuOpen.value = false; epMenuOpen.value = false; qMenuOpen.value = false }
}

/** The 选集 button. The bar popups share one corner, so only one is open at a
 *  time; the list scrolls the current episode into view, since a season can be
 *  longer than the popup is tall. */
function toggleEpMenu() {
  epMenuOpen.value = !epMenuOpen.value
  if (!epMenuOpen.value) return
  menuOpen.value = false
  dmMenuOpen.value = false
  qMenuOpen.value = false
  nextTick(() => {
    epListEl.value?.querySelector('.on')?.scrollIntoView({ block: 'nearest' })
  })
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

watch([danmakuOpacity, danmakuSpeed, danmakuArea, danmakuFont, danmakuStroke], () => {
  localStorage.setItem('dm:dmOpacity', String(danmakuOpacity.value))
  localStorage.setItem('dm:dmSpeed', String(danmakuSpeed.value))
  localStorage.setItem('dm:dmArea', String(danmakuArea.value))
  localStorage.setItem('dm:dmFont', String(danmakuFont.value))
  localStorage.setItem('dm:dmStroke', danmakuStroke.value ? '1' : '0')
})

// Toggles replace the object, so a shallow watch sees every change; deep is
// harmless and keeps a future in-place edit persisted too.
watch(danmakuTypes, () => {
  localStorage.setItem('dm:dmTypes', JSON.stringify(danmakuTypes.value))
}, { deep: true })

function saveProgress() {
  const v = videoEl.value
  if (!v) return
  // While a switch or a resume holds the picture at 0 and waits for packaging,
  // the element's clock is not where the viewer is — the waiting target is.
  //
  // A pending target only counts while the picture is actually parked on it. Once
  // playback is running, the element's clock is the truth: a stale target left
  // behind by a resume would otherwise be saved as the position, and the next
  // visit would jump to it — which looks exactly like playback skipping ahead on
  // its own.
  const parked = v.paused || v.readyState < 2
  const waiting = parked ? (switchAt.value || resumeAt.value) : 0
  const pos = waiting || v.currentTime + streamStart.value
  // `v.duration` belongs to the growing EVENT playlist, not to the whole file.
  // Saving it once packaging was 105s ahead wrote 155s for a 1421s episode and
  // made later resume checks believe 95% had already been watched. The playback
  // decision's probe duration is canonical; fall back to the measured UI duration
  // only for a source whose real duration is genuinely unknown.
  const knownDur = Number(play.value?.duration) || 0
  const dur = knownDur > 0
    ? knownDur
    : Math.max(duration.value || 0, (v.duration && isFinite(v.duration)) ? v.duration + streamStart.value : 0)
  if (!dur || !isFinite(dur)) return
  api.post('/api/progress', {
    file_id: Number(route.params.fileId),
    // File time, not stream time: otherwise resuming a session-packaged playback
    // would save a position measured from where the session happened to begin.
    position: pos,
    duration: dur,
  }).catch(() => {})
}

function selectVersion(v) {
  if (v.current) return
  saveProgress()
  toast.info(`切换到 ${v.label || '另一版本'}`)
  router.replace(`/play/${v.id}`)
}

watch(() => route.params.fileId, (nv, ov) => {
  if (nv && ov && nv !== ov) {
    // A switch that was catching up belonged to the file being left.
    switchAt.value = 0
    switchLabel.value = ''
    switchIsResume.value = false
    resumeAt.value = 0
    switchAutoPlay = true
    qMenuOpen.value = false
    loadDecision()
  }
})

function selectAudio(i) {
  currentAudio.value = i
  panelOpen.value = false
  loadDecision()
}

/* --------------------------------------------------------------- 清晰度 switch
 *
 * Picking a rung is a reload of the stream, not a toggle: the source hands out
 * whole ladders and the server packages one of them. The position is the thing
 * that has to survive, so it is saved first and then restored by the waiting
 * logic (see switchAt/maybeResume). The old rung keeps playing in the background
 * until it is replaced, so a failed switch can fall back to it.
 */
async function selectQuality(opt) {
  const want = Number(opt?.id) || 0
  qMenuOpen.value = false
  if (qualityBusy.value || want === qualityPref.value) return
  qualityBusy.value = true
  // Where the viewer wants to continue: a pending switch or resume wins (the
  // picture is parked for it), otherwise the element's clock, which is the truth —
  // `position` is a rendered copy that a throttled timeupdate can leave behind.
  // A resumeAt while the picture is moving is stale and must not pull the new
  // stream backwards.
  const vNow = videoEl.value
  const at = switchAt.value ||
    (vNow && vNow.paused ? resumeAt.value : 0) ||
    (vNow ? vNow.currentTime + streamStart.value : position.value)
  // The new stream carries its own position (switchAt). The progress saved a
  // moment ago is the same point, and looking it up again would only compete with
  // the switch.
  progressChecked = true
  const prev = qualityPref.value
  const canStart = !!play.value?.can_start_at
  const wasPlaying = !!vNow && !vNow.paused && !vNow.ended
  switchAutoPlay = wasPlaying
  switchIsResume.value = false
  qualityPref.value = want
  // The old stream is still attached while the new decision is being fetched, and
  // its buffer would satisfy a resume check: disarm it, or the poll could "restore"
  // the position on the stream that is about to be replaced (and the real switch
  // would then have no target left).
  candMeta = false
  // The old rung keeps its picture and sound until the new one is ready, but the
  // overlay is about to cover it — stop it now so the wait is not scored over a
  // programme that is still playing.
  videoEl.value?.pause()
  // A resumeAt while the picture was moving is stale: saving it would store the
  // wrong position for this switch (and for a reload after it).
  if (wasPlaying) resumeAt.value = 0
  saveProgress()
  switchAt.value = at > 1 ? at : 0
  switchLabel.value = want ? (opt?.short || opt?.label || '') : '自动'
  resumeAt.value = switchAt.value
  // The old stream's progress must not be mistaken for the new one's: a bilibili
  // episode starts packaging from 0, and the previous rung may already be
  // minutes ahead.
  streamStatus.value = null
  panelOpen.value = false
  menuOpen.value = false
  dmMenuOpen.value = false
  epMenuOpen.value = false
  toast.info(want
    ? `正在切换到 ${opt.label || opt.short || '所选清晰度'}…`
    : '正在切换到自动清晰度…', 2000)
  try {
    await loadDecision(forceMode.value, canStart ? at : 0)
  } finally {
    qualityBusy.value = false
    if (fatal.value) {
      // The new rung could not be started. Put the preference back so the menu
      // does not claim a quality that is not on screen, and let the old stream's
      // own bookkeeping (resume checks) run again.
      qualityPref.value = prev
      switchAt.value = 0
      switchLabel.value = ''
      resumeAt.value = 0
      progressChecked = false
      candMeta = true
    }
  }
}

/** The 清晰度 popup: its own button, like 弹幕 and 选集, because changing rung
 *  mid-episode is a thing a viewer does while watching. */
function toggleQMenu() {
  qMenuOpen.value = !qMenuOpen.value
  if (!qMenuOpen.value) return
  menuOpen.value = false
  dmMenuOpen.value = false
  epMenuOpen.value = false
  panelOpen.value = false
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
// relative to a packaged session (see streamStart). Ask the subtitle endpoint
// to move the cues before the browser parses them. Mutating parsed cues while
// the track is already showing leaves Chromium's cue renderer in an unreliable
// state, which showed up as subtitles stuck on an old line after a cached seek.
function subTrackSrc(sub) {
  if (!sub?.url) return ''
  if (!streamStart.value) return sub.url
  const url = new URL(sub.url, window.location.origin)
  url.searchParams.set('start', String(streamStart.value))
  return `${url.pathname}${url.search}${url.hash}`
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
        const line = ((top + (c.dmLine / 100) * picH) / box.height) * 100
        if (Math.abs(c.line - line) > 0.001) c.line = line
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
  if (!sub) {
    cancelSubtitleRestore(false)
    for (const t of v.textTracks) t.mode = 'disabled'
    activeSub.value = null
    return true
  }
  const el = findSubTrack(sub)
  if (!el || !el.track) return false
  cancelSubtitleRestore(false)
  for (const t of v.textTracks) t.mode = 'disabled'
  // The cues may already be loaded, or may arrive with the track's load event.
  fitCuesToPicture()
  el.addEventListener('load', fitCuesToPicture, { once: true })
  el.track.mode = 'showing'
  activeSub.value = sub
  return true
}

// A repaint is intentionally short, but it must never leave captions off. A
// second seek can cancel the pending frames before its own `seeked` arrives, so
// restore the old track first and let that seek perform a fresh repaint.
function cancelSubtitleRestore(restoreCurrent) {
  subSeekGeneration++
  if (subSeekRaf) cancelAnimationFrame(subSeekRaf)
  if (subSeekTimer) clearTimeout(subSeekTimer)
  subSeekRaf = 0
  subSeekTimer = 0
  const hidden = subSeekHidden
  subSeekHidden = null
  if (!restoreCurrent || !hidden) return
  if (activeSub.value?.id !== hidden.subId || !hidden.el.isConnected ||
    hidden.el.track !== hidden.track || hidden.track.mode !== 'hidden') return
  fitCuesToPicture()
  hidden.track.mode = 'showing'
}

// Chromium can keep the last native cue painted after a seek even though the
// track's activeCues have already changed. Briefly taking the track out of
// `showing` after `seeked`, then putting it back two frames later, rebuilds
// that renderer state without touching cue timings or formatting.
function onSubtitleSeeking() {
  stopDanmaku()
  cancelSubtitleRestore(true)
}

function onSubtitleSeeked() {
  paintDanmaku()
  kickDanmaku()
  const sub = activeSub.value
  const v = videoEl.value
  const el = sub && !nativeSubs.value ? findSubTrack(sub) : null
  const track = el?.track
  if (!track || v?.seeking) return
  if (track.mode === 'showing') {
    track.mode = 'hidden'
    subSeekHidden = { track, el, subId: sub.id }
  } else if (subSeekHidden?.track !== track || subSeekHidden?.el !== el ||
    subSeekHidden?.subId !== sub.id) {
    return
  }
  const generation = ++subSeekGeneration
  const show = () => {
    if (disposed || generation !== subSeekGeneration || activeSub.value?.id !== sub.id ||
      !el.isConnected || el.track !== track || track.mode !== 'hidden') return
    // A late seek event can leave `seeking` true briefly. Retry instead of
    // abandoning the track in `hidden` when no later seeked event follows.
    if (videoEl.value?.seeking) {
      subSeekTimer = setTimeout(show, 100)
      return
    }
    if (subSeekTimer) clearTimeout(subSeekTimer)
    if (subSeekRaf) cancelAnimationFrame(subSeekRaf)
    subSeekTimer = 0
    subSeekRaf = 0
    fitCuesToPicture()
    track.mode = 'showing'
    subSeekHidden = null
  }
  // rAF keeps the repaint frame-aligned, while the timer is the fallback for a
  // backgrounded tab where animation frames can be suspended indefinitely.
  subSeekTimer = setTimeout(show, 150)
  subSeekRaf = requestAnimationFrame(() => {
    subSeekRaf = requestAnimationFrame(() => {
      subSeekRaf = 0
      show()
    })
  })
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
    dmMenuOpen.value = false
    epMenuOpen.value = false
    qMenuOpen.value = false
    panelOpen.value = false
    toast.info('已锁定按键,点左侧锁图标解锁', 2600)
  } else {
    showControls()
  }
}

function togglePlay() {
  const v = videoEl.value
  if (!v) return
  // A switch in progress is only a reason to refuse while there is nothing to
  // play: the element is attached *after* the packaging reached the viewer's
  // position, so once a picture exists a tap means what it says. Without this the
  // first second of a resumed episode answered "please wait" to a pause.
  if (switchAt.value && v.readyState < 2) {
    toast.info('正在切换清晰度,请稍等…', 1600)
    return
  }
  if (v.paused) v.play().catch(() => {}); else v.pause()
}

function seekBy(delta) {
  if (switchAt.value) {
    toast.info('正在切换清晰度,请稍等…', 1600)
    return
  }
  const v = videoEl.value
  if (!v || !isFinite(v.duration)) return
  // The element's own clock is stream time; the jump is meant in file time.
  const target = v.currentTime + streamStart.value + delta
  // Jumping past what has been packaged would only stall on fragments that do not
  // exist yet. The server can start a stream at any position (can_start_at), and
  // the same reload that a drag past the window uses lands there directly.
  if (delta > 0 && target > reachable() && play.value?.can_start_at && target < duration.value - 1) {
    resumeAt.value = 0
    seekTo(target)
    return
  }
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
  if (switchAt.value) {
    // There is no stream at the target yet, and a drag would fight the wait.
    toast.info('正在切换清晰度,请稍等…', 1600)
    return
  }
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
  // A drag overrides a pending resume: the viewer has said where they want to be.
  resumeAt.value = 0
  if (scrubbing.value && v && isFinite(scrubTime.value)) {
    // scrubTime is file time; the element wants stream time.
    let target = scrubTime.value
    // How far this stream reaches: what its session has packaged. With on-demand
    // packaging that is a window that grows behind the playhead, not the whole
    // episode — and dragging past it is the one thing a player cannot do, because
    // those fragments do not exist yet.
    const reach = reachable()
    const beyond = target > reach - 0.5
    // The stream does not contain the file's opening when its session began
    // mid-file (a resume), so a drag before the session's own start is as
    // unreachable as one past the packaging edge. Same remedy: ask the server for
    // a stream that begins there.
    const before = target < streamStart.value - 0.5
    if ((beyond || before) && play.value?.can_start_at && target > 0) {
      // The server can begin a stream there, so ask for one: the same thing it does
      // for "continue watching", with the position stated instead of remembered.
      seekTo(target)
    } else {
      // Where the playhead can actually go: the farthest fragment that exists.
      let applied = target
      if (beyond) {
        if (reach > streamStart.value) {
          applied = reach - 0.5
          toast.info(`这一段还在生成中,先跳到 ${fmtTime(applied)}`)
        } else {
          // Nothing is packaged yet, so there is nowhere to go. Say so and leave
          // the clock where it is — showing the requested position while the video
          // plays somewhere else is worse than not seeking at all.
          applied = null
          toast.info('这一段还在生成中,稍等一下')
        }
      }
      if (applied !== null) {
        v.currentTime = Math.max(0, applied - streamStart.value)
        position.value = applied
      }
    }
  }
  scrubbing.value = false
  window.removeEventListener('pointermove', moveScrub)
  window.removeEventListener('pointerup', endScrub)
}

/**
 * Move playback to a position this stream has not packaged.
 *
 * A player cannot seek into fragments that do not exist, so this is a restart at
 * the target: the same thing the server does for "continue watching", with the
 * position stated explicitly rather than taken from saved progress.
 */
async function seekTo(at) {
  toast.info(`正在从 ${fmtTime(at)} 开始…`)
  // Starting somewhere else is a playback the viewer asked for: it plays unless
  // they had the picture paused, and this path is only taken from a moving one.
  switchAutoPlay = true
  await loadDecision(forceMode.value, at)
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
  if (isPlaying.value && !menuOpen.value && !dmMenuOpen.value && !epMenuOpen.value &&
    !qMenuOpen.value && !panelOpen.value) {
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
  // Bilibili's own key for the layer, and the toggle button has to keep working
  // from the keyboard as the drawer's chip always did.
  else if (k === 'd' && dmControl.value) toggleDanmaku()
  else if (k === 'escape') {
    if (panelOpen.value) panelOpen.value = false
    else if (menuOpen.value) menuOpen.value = false
    else if (dmMenuOpen.value) dmMenuOpen.value = false
    else if (epMenuOpen.value) epMenuOpen.value = false
    else if (qMenuOpen.value) qMenuOpen.value = false
  }
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
  if (t instanceof Element && t.closest('.panel, .ctl-menu, .dm-menu, .ep-menu, .q-menu')) return
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
  cancelSubtitleRestore(false)
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
              @durationchange="applyDuration"
              @error="onErr"
              @timeupdate="onTimeUpdate"
              @progress="onTimeUpdate"
              @pause="saveProgress(); isPlaying = false; showControls(); stopDanmaku()"
              @play="isPlaying = true; showControls(); kickDanmaku()"
              @seeking="onSubtitleSeeking"
              @seeked="onSubtitleSeeked"
              @waiting="onWaiting"
              @playing="onPlaying"
              @canplay="onCanPlay"
              @ended="onEnded"
              @click="onTap"
              @dblclick="onDoubleTap"
              @ratechange="rate = videoEl?.playbackRate || 1"
            >
              <template v-if="!nativeSubs">
                <track
                  v-for="sub in (play?.subtitles || []).filter(s => s.url)" :key="`${sub.id}:${streamStart}`" kind="subtitles"
                  :src="subTrackSrc(sub)" :label="sub.label || sub.lang || '字幕'"
                  :default="!!sub.default"
                />
              </template>
            </video>

            <!-- 弹幕层. Under the controls (z-index 3 against their 4 and up) and
                 transparent to input, so every gesture still reaches the picture. -->
            <canvas v-show="danmakuReady && danmakuOn" ref="dmCanvas" class="dm-layer" aria-hidden="true"></canvas>

            <!-- On the immersive player the top bar carries the title; on a watch
                 page that lives in the page below, so only the way back stays on
                 the picture. The settings entry is the gear in the control bar —
                 the bar is where every other control is, and a second gear over
                 the title was one more thing to explain. -->
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

              <!-- 选集: only the theater needs it. A watch page already shows the
                   episodes under the picture (a grid on a phone, the sidebar on a
                   tablet), so the button would be a second copy of a list that is
                   already on screen. -->
              <div v-if="theaterChrome && episodes.length > 1" class="menu-wrap">
                <button
                  class="icon-btn" :class="{ on: epMenuOpen }"
                  aria-label="选集" title="选集" :aria-expanded="epMenuOpen"
                  @click="toggleEpMenu"
                >
                  <AppIcon name="grid" :size="19" />
                </button>
                <Transition name="fade">
                  <div v-if="epMenuOpen" class="ctl-menu ep-menu" @pointerdown.stop>
                    <div class="ep-head">
                      <span class="cm-title mb-0">选集</span>
                      <span class="tiny">{{ episodes.length }} 集</span>
                    </div>
                    <div ref="epListEl" class="ep-list">
                      <button
                        v-for="(ep, i) in episodes" :key="ep.id"
                        class="ep-item" :class="{ on: ep.id === play?.item_id }"
                        :aria-current="ep.id === play?.item_id ? 'true' : undefined"
                        :aria-label="epName(ep, i)"
                        :title="epSub(ep) ? `${epName(ep, i)} · ${epSub(ep)}` : epName(ep, i)"
                        @click="playEpisode(ep)"
                      >
                        <span class="ep-no num">{{ epNo(ep, i) }}</span>
                        <span class="ep-copy">
                          <span class="ep-name truncate">{{ epName(ep, i) }}</span>
                          <span v-if="epSub(ep)" class="tiny truncate">{{ epSub(ep) }}</span>
                        </span>
                        <AppIcon v-if="ep.id === play?.item_id" name="play" :size="13" filled />
                      </button>
                    </div>
                  </div>
                </Transition>
              </div>

              <button
                v-if="!nativeSubs && (play?.subtitles || []).length" class="icon-btn"
                :class="{ on: activeSub }" aria-label="字幕" title="字幕"
                @click="openPanel('subs')"
              >
                <AppIcon name="subtitles" :size="19" />
              </button>

              <!-- 清晰度: the rung's own name is the button, the way bilibili's
                   player names it, and the popup is one tap from the picture —
                   changing quality is a thing a viewer does mid-episode. A file
                   with one picture (every local source) gets no button. -->
              <div v-if="qualities.length" class="menu-wrap">
                <button
                  class="icon-btn q-btn" :class="{ on: qualityPref > 0 || qMenuOpen }"
                  :aria-label="`清晰度:${qualityButtonLabel}`"
                  :title="qualityPref === 0
                    ? `清晰度:自动${qualityFullLabel ? ' · 当前 ' + qualityFullLabel : ''}`
                    : `清晰度:${qualityButtonLabel}`"
                  :aria-expanded="qMenuOpen" @click="toggleQMenu"
                >
                  <span>{{ qualityButtonLabel }}</span>
                </button>
                <Transition name="fade">
                  <div v-if="qMenuOpen" class="ctl-menu q-menu" @pointerdown.stop>
                    <div class="cm-title">清晰度</div>
                    <button
                      class="cm-item" :class="{ on: qualityPref === 0 }"
                      :disabled="qualityBusy" @click="selectQuality({ id: 0, label: '自动' })"
                    >
                      <span class="cm-label">自动</span>
                      <span class="tiny">
                        {{ qualityLabel ? `当前 ${qualityFullLabel || qualityLabel}` : '最高可用' }}
                      </span>
                    </button>
                    <button
                      v-for="q in qualities" :key="q.id" class="cm-item"
                      :class="{ on: qualityPref === q.id }" :disabled="qualityBusy"
                      @click="selectQuality(q)"
                    >
                      <span class="cm-label">{{ q.label || q.short }}</span>
                      <span class="tiny">
                        {{ q.width }}×{{ q.height }}<template v-if="q.id === effectiveQuality"> · 当前</template>
                      </span>
                    </button>
                    <div v-if="qualityBusy" class="tiny" style="margin-top:8px">正在切换…</div>
                  </div>
                </Transition>
              </div>

              <!-- 弹幕: the switch, and the settings button that opens the panel —
                   two controls, not one. Turning the layer on and off is what a
                   viewer does mid-episode; tuning it is what they do once. They
                   live in the bar under the picture, next to the volume and the
                   subtitles, and not in the settings drawer: the drawer is the
                   picture, the tracks and the playback mode, and reaching into it
                   for a one-tap toggle means opening a panel over the video. -->
              <template v-if="dmControl">
                <button
                  class="icon-btn" :class="{ on: danmakuOn }" :aria-pressed="danmakuOn"
                  :aria-label="danmakuOn ? '关闭弹幕' : '开启弹幕'"
                  :title="danmakuOn ? '弹幕已开启' : '弹幕已关闭'" @click="toggleDanmaku"
                >
                  <AppIcon :name="danmakuOn ? 'danmaku' : 'danmakuOff'" :size="19" />
                </button>

                <div class="menu-wrap">
                  <button
                    class="icon-btn" :class="{ on: dmMenuOpen }" aria-label="弹幕设置"
                    title="弹幕设置" :aria-expanded="dmMenuOpen" @click="toggleDmMenu"
                  >
                    <AppIcon name="sliders" :size="19" />
                  </button>
                  <Transition name="fade">
                    <div v-if="dmMenuOpen" class="ctl-menu dm-menu" @pointerdown.stop>
                      <div class="dm-head">
                        <span class="cm-title mb-0">弹幕设置</span>
                        <span class="tiny">
                          {{ danmakuFailed ? '读取失败' : danmakuCount ? `${danmakuCount} 条` : '读取中…' }}
                        </span>
                      </div>
                      <div class="dm-settings">
                        <div class="pn-group">
                          <div class="label">显示区域</div>
                          <div class="pn-chips dm-opts">
                            <button
                              v-for="o in DM_AREAS" :key="o.value" class="chip sm"
                              :class="{ on: danmakuArea === o.value }" @click="setDanmakuArea(o.value)"
                            >{{ o.label }}</button>
                          </div>
                        </div>

                        <div class="pn-group">
                          <div class="label">不透明度 <span class="tiny">{{ dmOpacityPct }}%</span></div>
                          <label class="pn-range">
                            <input
                              type="range" min="0" max="100" step="1" aria-label="弹幕不透明度"
                              :value="dmOpacityPct" @input="onDanmakuOpacity"
                            />
                          </label>
                        </div>

                        <div class="pn-group">
                          <div class="label">字号</div>
                          <div class="pn-chips dm-opts">
                            <button
                              v-for="o in DM_FONTS" :key="o.value" class="chip sm"
                              :class="{ on: danmakuFont === o.value }" @click="setDanmakuFont(o.value)"
                            >{{ o.label }}</button>
                          </div>
                        </div>

                        <div class="pn-group">
                          <div class="label">速度</div>
                          <label class="pn-range">
                            <span class="tiny">慢</span>
                            <input
                              type="range" :min="DM_SPEED_MIN" :max="DM_SPEED_MAX" step="1" aria-label="弹幕速度"
                              :value="dmSpeedSlider" @input="onDanmakuSpeed"
                            />
                            <span class="tiny">快</span>
                          </label>
                        </div>

                        <div class="pn-group">
                          <div class="label">弹幕类型</div>
                          <div class="pn-chips dm-opts">
                            <button
                              v-for="t in DM_TYPES" :key="t.key" class="chip sm"
                              :class="{ on: danmakuTypes[t.key] }" @click="toggleDanmakuType(t.key)"
                            >{{ t.label }}</button>
                          </div>
                        </div>

                        <!-- The one boolean in this panel: a switch, not a chip,
                             because "off" has no name of its own. -->
                        <div class="pn-group dm-toggle">
                          <div class="label">字体黑边</div>
                          <button
                            type="button" class="switch" :class="{ on: danmakuStroke }"
                            role="switch" :aria-checked="danmakuStroke"
                            :title="danmakuStroke ? '字体黑边已开启' : '字体黑边已关闭'"
                            aria-label="字体黑边" @click="toggleDanmakuStroke"
                          ></button>
                        </div>

                        <div class="pn-group">
                          <div class="pn-chips dm-opts">
                            <button class="chip sm" @click="resetDanmakuOptions">恢复默认</button>
                          </div>
                        </div>
                      </div>
                    </div>
                  </Transition>
                </div>
              </template>

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
                    <!-- The full drawer used to hang off the top bar's list button.
                         That button is gone, so its remaining job — versions, audio
                         tracks, the reason the server picked this mode — lands here,
                         one level under the quick settings. -->
                    <button class="cm-item" @click="togglePanel">
                      <span class="cm-label"><AppIcon name="layers" :size="15" /> 全部播放设置</span>
                      <span class="tiny">版本 · 音轨</span>
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

              <!-- 清晰度: the same ladder the bar's own popup offers, in the full
                   settings list — a watch page may have the panel open already. -->
              <div v-if="drawerMode !== 'subs' && qualities.length" class="pn-group">
                <div class="label">清晰度</div>
                <div class="pn-chips">
                  <button
                    class="chip" :class="{ on: qualityPref === 0 }"
                    :disabled="qualityBusy" @click="selectQuality({ id: 0, label: '自动' })"
                  >自动</button>
                  <button
                    v-for="q in qualities" :key="q.id" class="chip"
                    :class="{ on: qualityPref === q.id }"
                    :disabled="qualityBusy" @click="selectQuality(q)"
                  >{{ q.label || q.short }}<span v-if="q.id === effectiveQuality" class="tiny"> · 当前</span></button>
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
              <BiliBadge v-if="ep.source_type === 'bilibili'" mini />
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
      <span v-if="dmControl"><kbd>D</kbd> 弹幕</span>
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

/* ============================ 弹幕 popup ================================== */
/* Bilibili's own settings panel, in the control bar's popup shape: its own button
   under the picture opens it, so it is not a block in (.panel) any more. It keeps
   the drawer's group markup — .pn-group/.pn-chips/.pn-range — because the two are
   the same kind of list; what it needs of its own is a narrow column, wrapping
   chip rows rather than the drawer's stacked ones, and a head with the count.
   The ranges carry their own endpoints and value, so those spans size to their
   text instead of taking the 62px label column. */
.dm-menu { width: min(300px, calc(100vw - 24px)); }
.dm-head { display: flex; align-items: baseline; justify-content: space-between; gap: 10px; margin-bottom: 12px; }
.dm-settings { display: flex; flex-direction: column; gap: 12px; }
.dm-settings .pn-group { gap: 6px; }
/* A popup is a short surface, so its options stay compact: the drawer's roomy
   34px chip is a touch target for a list that can afford one. */
.dm-menu .pn-chips.dm-opts { flex-direction: row; flex-wrap: wrap; gap: 6px; }
.dm-menu .pn-group .chip { min-height: 30px; padding: 5px 10px; }
.dm-menu .pn-range { max-width: none; }
.dm-menu .pn-range > span { flex: 0 0 auto; }

/* The panel's one boolean: its label and the knob share a row. It reuses the
   site's .switch, so only the button plumbing is undone here. */
.dm-settings .dm-toggle {
  flex-direction: row;
  align-items: center;
  justify-content: space-between;
  width: 100%;
  gap: 10px;
}
.dm-settings .dm-toggle .label { margin: 0; }
.dm-settings .dm-toggle .switch { appearance: none; padding: 0; }

/* A watch page has the whole page under the picture, so the popup drops from its
   button instead of rising over the video: in the theater the bar sits at the foot
   of the screen and up is the only way out, but here a popup twice the height of
   the control strip would cover the picture it is about — on a phone the picture,
   the seek bar and the buttons are all inside one screen. */
.lay-phone:not(.fs) .dm-menu, .lay-tablet:not(.fs) .dm-menu {
  top: calc(100% + 8px);
  bottom: auto;
  max-height: none;
}

/* ============================ 清晰度 popup ================================ */
/* The rung's name is the button, the way bilibili's player names it, so this is
   a text button rather than one of the bar's square icons. The popup reuses the
   control bar's menu shape and drops downward on a watch page, like 弹幕. */
.ctl-row .q-btn {
  width: auto;
  min-width: 46px;
  padding: 0 10px;
  font-size: var(--fs-12);
  font-weight: 650;
  font-variant-numeric: tabular-nums;
  letter-spacing: 0.02em;
}
.q-menu { width: min(260px, calc(100vw - 24px)); }
.lay-phone:not(.fs) .q-menu, .lay-tablet:not(.fs) .q-menu {
  top: calc(100% + 8px);
  bottom: auto;
  max-height: none;
}

/* ============================ 选集 popup ================================== */
/* The season as a list, hung off the 选集 button beside the subtitle one: number,
   name, and the air date or version count when the episode has one. The current
   episode is marked and scrolled into view, so a 24-episode season does not open
   at episode 1 every time. */
.ep-menu { width: min(360px, calc(100vw - 24px)); }
.ep-head { display: flex; align-items: baseline; justify-content: space-between; gap: 10px; margin-bottom: 8px; }
.ep-list {
  display: flex;
  flex-direction: column;
  gap: 2px;
  /* Never taller than the screen it opened on; the list scrolls itself. */
  max-height: min(420px, calc(100dvh - 220px));
  overflow-y: auto;
  overscroll-behavior: contain;
}
.ep-item {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 7px 9px;
  border-radius: var(--r-s);
  text-align: left;
  color: var(--text-2);
  transition: background var(--t-fast), color var(--t-fast);
}
.ep-item:hover { background: rgba(255, 255, 255, 0.07); color: var(--text-1); }
.ep-item.on { background: var(--brand-tint); color: var(--text-1); }
.ep-item.on svg { color: var(--brand); }
.ep-no {
  flex: 0 0 auto;
  min-width: 28px;
  height: 24px;
  padding: 0 6px;
  display: grid;
  place-items: center;
  border-radius: var(--r-xs);
  background: rgba(255, 255, 255, 0.08);
  font-size: var(--fs-12);
  font-weight: 650;
  color: var(--text-2);
}
.ep-item.on .ep-no { background: var(--brand); color: var(--brand-ink); }
.ep-copy { display: flex; flex-direction: column; gap: 1px; min-width: 0; flex: 1; }
.ep-name { font-size: var(--fs-13); }


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
  .ctl-row .q-btn { width: auto; min-width: 40px; padding: 0 7px; font-size: 11px; }
  .time { font-size: var(--fs-12); margin-left: 4px; }
  .big-play { width: 66px; height: 66px; }
}
</style>
