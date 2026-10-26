// Minimal API client with cookie auth.
export class ApiError extends Error {
  constructor(status, message, code = '', details = null) {
    super(message)
    this.status = status
    this.code = code
    this.details = details
  }
}

async function request(method, url, body, options = {}) {
  const opts = { method, credentials: 'include', headers: { ...(options.headers || {}) } }
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json'
    opts.body = JSON.stringify(body)
  }
  const resp = await fetch(url, opts)
  const text = await resp.text()
  let data = null
  try { data = text ? JSON.parse(text) : null } catch { data = text }
  if (!resp.ok) {
    const payload = data && data.error
    const message = typeof payload === 'string'
      ? payload
      : payload && typeof payload.message === 'string'
        ? payload.message
        : `HTTP ${resp.status}`
    const code = payload && typeof payload.code === 'string' ? payload.code : ''
    const details = payload && payload.details && typeof payload.details === 'object' ? payload.details : null
    throw new ApiError(resp.status, message, code, details)
  }
  return data
}

export const api = {
  get: (url) => request('GET', url),
  post: (url, body, options) => request('POST', url, body ?? {}, options),
  put: (url, body) => request('PUT', url, body),
  del: (url) => request('DELETE', url),
}

export function imgUrl(itemId, kind) {
  return `/api/img/${itemId}/${kind}`
}

/** Best available artwork for a card: episode still → backdrop → poster. */
export function artUrl(item) {
  if (!item) return ''
  if (item.still_path) return imgUrl(item.id, 'still')
  if (item.backdrop_path) return imgUrl(item.id, 'backdrop')
  if (item.poster_path) return imgUrl(item.id, 'poster')
  return ''
}

/**
 * URL for one specific artwork kind — but only when that art actually exists.
 * Returning '' lets <SmartImage> draw its own gradient placeholder instead of
 * the server's generic "no poster" SVG.
 */
export function artIf(item, kind) {
  if (!item) return ''
  const has = { poster: item.poster_path, backdrop: item.backdrop_path, still: item.still_path }[kind]
  return has ? imgUrl(item.id, kind) : ''
}

export function hasArt(item) {
  return !!(item && (item.poster_path || item.backdrop_path || item.still_path))
}

// Client capability class mirrors the server-side decision.
export function detectClient() {
  const ua = navigator.userAgent
  const isIOS = /iPhone|iPad|iPod/.test(ua) || (/Macintosh/.test(ua) && 'ontouchend' in document)
  if (isIOS) return 'safari'
  const isSafari = /Safari/.test(ua) && !/Chrome|Chromium|Edg|OPR/.test(ua)
  if (isSafari) return 'safari'
  if (/Firefox/.test(ua)) return 'firefox'
  return 'chrome'
}

export function fmtSize(n) {
  if (!n) return '-'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++ }
  return `${v.toFixed(v >= 10 || i === 0 ? 0 : 1)} ${units[i]}`
}

export function fmtTime(sec) {
  if (!sec || sec < 0 || !isFinite(sec)) return '0:00'
  const s = Math.floor(sec % 60)
  const m = Math.floor((sec / 60) % 60)
  const h = Math.floor(sec / 3600)
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}` : `${m}:${String(s).padStart(2, '0')}`
}

/** "还剩 42 分钟" style hint for partially watched items. */
export function fmtRemaining(position, duration) {
  if (!duration || !isFinite(duration)) return ''
  const left = Math.max(0, duration - (position || 0))
  if (left < 60) return '即将结束'
  const m = Math.round(left / 60)
  if (m < 60) return `还剩 ${m} 分钟`
  const h = Math.floor(m / 60)
  return `还剩 ${h} 小时 ${m % 60} 分`
}

export function fmtDate(v) {
  if (!v) return ''
  const d = new Date(v)
  if (isNaN(d)) return String(v)
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}

export function fmtRelative(v) {
  if (!v) return ''
  const d = new Date(v)
  if (isNaN(d)) return ''
  const diff = Date.now() - d.getTime()
  const day = 86400000
  if (diff < 3600000) return '刚刚'
  if (diff < day) return `${Math.floor(diff / 3600000)} 小时前`
  if (diff < day * 30) return `${Math.floor(diff / day)} 天前`
  if (diff < day * 365) return `${Math.floor(diff / (day * 30))} 个月前`
  return `${Math.floor(diff / (day * 365))} 年前`
}

export function parseGenres(item) {
  if (!item?.genres) return []
  try {
    const g = JSON.parse(item.genres)
    return Array.isArray(g) ? g.filter(Boolean) : []
  } catch { return [] }
}

export function episodeLabel(item) {
  if (!item || item.type !== 'episode') return ''
  const s = item.season ? `S${String(item.season).padStart(2, '0')}` : ''
  const e = item.episode ? `E${String(item.episode).padStart(2, '0')}` : ''
  return s + e
}

/** Human label for a media stream map (used on detail + player). */
export function streamBadges(media) {
  const out = []
  if (!media?.video) return out
  const v = media.video
  if (v.height >= 2000 || v.width >= 3800) out.push('4K')
  else if (v.height >= 1000) out.push('1080p')
  else if (v.height) out.push(`${v.height}p`)
  if (v.codec) out.push(String(v.codec).toUpperCase())
  if (v.dv_profile) out.push(`杜比视界 P${v.dv_profile}`)
  if (v.tag && v.tag !== '[0][0][0][0]') out.push(v.tag)
  return out
}
