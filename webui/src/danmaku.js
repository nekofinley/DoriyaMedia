// 弹幕(bullet comments)drawn on a canvas over the picture.
//
// Every position is a pure function of the media clock: a comment that started at
// t sits at x = W - ((now - t) / speed) * (W + w). Nothing accumulates frame to
// frame, so seeking costs nothing — jumping anywhere simply draws a different
// frame. The only state is which lane a comment was given, and that is rebuilt
// whenever the clock jumps.
//
// Comments come from the server in file time (see /api/danmaku); the caller
// passes file time in, having added whatever offset its stream starts at.

const FONT_STACK = '"PingFang SC","Hiragino Sans GB","Microsoft YaHei","Noto Sans CJK SC","Source Han Sans SC",sans-serif'
const FIXED_SECONDS = 4 // how long a top/bottom comment stays on screen
const LANE_GAP = 1.45 // lane height as a multiple of the font size

// The 弹幕类型 switches, as bits. One test per comment emitted is cheaper than
// walking three booleans, and it runs on the emit path, not every frame.
const KIND_SCROLL = 1
const KIND_TOP = 2
const KIND_BOTTOM = 4
const KIND_ALL = KIND_SCROLL | KIND_TOP | KIND_BOTTOM

function kindOf(c) {
  // 4 and 5 are the pinned bottom and top comments; everything else (scroll,
  // reverse, advanced) travels across the picture and shares the lanes.
  return c.mode === 5 ? KIND_TOP : c.mode === 4 ? KIND_BOTTOM : KIND_SCROLL
}

// Anything not explicitly off is on, so a partial object is a valid setting.
function maskOf(types) {
  return (types.scroll === false ? 0 : KIND_SCROLL)
    | (types.top === false ? 0 : KIND_TOP)
    | (types.bottom === false ? 0 : KIND_BOTTOM)
}

export function createDanmaku(canvas) {
  const ctx = canvas.getContext('2d')
  const opt = { opacity: 1, speed: 8, area: 0.6, fontScale: 1, mask: KIND_ALL }

  let comments = [] // sorted by time
  let cursor = 0 // next comment to emit
  let active = [] // scrolling comments currently on screen
  let laneFreeAt = [] // per lane: the time its tail clears the right edge
  let topLanes = [] // per slot: the time a top comment expires
  let bottomLanes = []
  let W = 0
  let H = 0
  let dpr = 1
  let fontSize = 24
  let laneH = 34
  let laneCount = 1
  let lastTime = -1

  function resize() {
    const box = canvas.getBoundingClientRect()
    const w = Math.max(1, Math.round(box.width))
    const h = Math.max(1, Math.round(box.height))
    dpr = Math.min(2, window.devicePixelRatio || 1)
    W = w
    H = h
    canvas.width = Math.round(w * dpr)
    canvas.height = Math.round(h * dpr)
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    // Bilibili publishes sizes against a 25px baseline, and its own player draws
    // them at about that size on a desktop stage; on a phone that is far too big
    // for the picture, so the layer scales with the canvas instead.
    const prevFont = fontSize
    fontSize = Math.max(13, Math.min(25, Math.round(h * 0.05))) * opt.fontScale
    laneH = Math.round(fontSize * LANE_GAP)
    laneCount = Math.max(1, Math.floor((H * opt.area) / laneH))
    // Text width is measured against the font size, so a cached width belongs to
    // the size it was measured at. A font change is a user action; clearing the
    // cache costs one measure per comment on the next emit, not per frame.
    if (fontSize !== prevFont) for (const c of comments) c._w = undefined
    reset(lastTime < 0 ? 0 : lastTime)
  }

  // reset drops every placement. Called on a seek and whenever the geometry or
  // the speed changes, because the lanes were worked out against the old ones.
  function reset(t) {
    active = []
    laneFreeAt = new Array(laneCount).fill(0)
    topLanes = []
    bottomLanes = []
    cursor = lowerBound(t)
    lastTime = t
  }

  // lowerBound is the first comment that could still be on screen at t.
  function lowerBound(t) {
    let lo = 0
    let hi = comments.length
    while (lo < hi) {
      const mid = (lo + hi) >> 1
      if (comments[mid].t < t - opt.speed) lo = mid + 1
      else hi = mid
    }
    return lo
  }

  function width(c) {
    if (c._w === undefined) {
      ctx.font = `${fontWeight(c)} ${sizeOf(c)}px ${FONT_STACK}`
      c._w = ctx.measureText(c.text).width
    }
    return c._w
  }

  function sizeOf(c) {
    const s = c.size > 0 ? c.size : 25
    return Math.max(12, Math.round(fontSize * (s / 25)))
  }

  // The bigger the comment the heavier it is drawn, which is how bilibili's own
  // player reads at a glance.
  function fontWeight(c) {
    return sizeOf(c) >= fontSize ? '600' : '500'
  }

  function colorOf(c) {
    const n = (c.color | 0) & 0xffffff
    return `#${n.toString(16).padStart(6, '0')}`
  }

  function emit(c, t) {
    // A type that is switched off is skipped before it can claim a lane: a
    // comment nobody can see must not push the visible ones aside.
    if (!(opt.mask & kindOf(c))) return
    if (c.mode === 4 || c.mode === 5) {
      emitFixed(c, t)
      return
    }
    const w = width(c)
    let lane = -1
    for (let i = 0; i < laneCount; i++) {
      if (laneFreeAt[i] <= t) {
        lane = i
        break
      }
    }
    if (lane < 0) {
      // Crowded: share the lane that frees up soonest rather than dropping the
      // comment — a busy stretch of a popular episode is exactly when a viewer
      // expects to see a wall of them.
      let best = 0
      for (let i = 1; i < laneCount; i++) if (laneFreeAt[i] < laneFreeAt[best]) best = i
      lane = best
    }
    // The lane is clear again once this comment's tail has crossed the right edge.
    laneFreeAt[lane] = t + opt.speed * (w / (W + w))
    active.push({ c, lane, w })
  }

  function emitFixed(c, t) {
    const pool = c.mode === 4 ? bottomLanes : topLanes
    let slot = pool.findIndex((expires) => expires <= t)
    if (slot < 0) {
      if (pool.length >= Math.max(1, Math.floor(laneCount / 2))) return
      slot = pool.length
    }
    pool[slot] = t + FIXED_SECONDS
    active.push({ c, fixed: true, slot, w: width(c) })
  }

  function draw(t) {
    ctx.clearRect(0, 0, W, H)
    if (!W || !H || !comments.length) return
    if (lastTime < 0 || t < lastTime || t - lastTime > 2) reset(t)
    lastTime = t

    while (cursor < comments.length && comments[cursor].t <= t) {
      emit(comments[cursor], t)
      cursor++
    }

    ctx.globalAlpha = opt.opacity
    ctx.textBaseline = 'top'
    ctx.lineJoin = 'round'
    // The area setting is the band the layer may use, measured from the top.
    // Bottom comments sit on the bottom of that band rather than of the picture:
    // with 显示区域 at 1/4 屏 the whole layer is meant to stay in the top quarter.
    const areaH = H * opt.area
    const keep = []
    for (const a of active) {
      const c = a.c
      const size = sizeOf(c)
      ctx.font = `${fontWeight(c)} ${size}px ${FONT_STACK}`
      ctx.lineWidth = Math.max(2, Math.round(size / 7))
      ctx.strokeStyle = 'rgba(0,0,0,0.82)'
      let x
      let y
      if (a.fixed) {
        const slots = c.mode === 4 ? bottomLanes.length : topLanes.length
        const idx = Math.min(a.slot, Math.max(0, slots - 1))
        x = (W - a.w) / 2
        y = c.mode === 4
          ? areaH - (idx + 1) * laneH
          : idx * laneH
        if (t > (c.mode === 4 ? bottomLanes[idx] : topLanes[idx])) continue
      } else {
        const p = (t - c.t) / opt.speed
        if (p < 0) {
          keep.push(a)
          continue
        }
        if (p > 1) continue
        x = W - p * (W + a.w)
        y = a.lane * laneH
      }
      if (y < -laneH || y > H) continue
      ctx.strokeText(c.text, x, y)
      ctx.fillStyle = colorOf(c)
      ctx.fillText(c.text, x, y)
      keep.push(a)
    }
    active = keep
    ctx.globalAlpha = 1
  }

  return {
    /** Replaces the whole set. The list must be sorted by time. */
    setComments(list) {
      comments = Array.isArray(list) ? list : []
      for (const c of comments) c._w = undefined
      resize()
    },
    setOptions(next) {
      const speedChanged = next.speed !== undefined && next.speed !== opt.speed
      const areaChanged = next.area !== undefined && next.area !== opt.area
      const scaleChanged = next.fontScale !== undefined && next.fontScale !== opt.fontScale
      let typesChanged = false
      if (next.types !== undefined) {
        const mask = maskOf(next.types)
        if (mask !== opt.mask) {
          opt.mask = mask
          typesChanged = true
        }
      }
      if (next.opacity !== undefined) opt.opacity = next.opacity
      if (next.speed !== undefined) opt.speed = Math.max(3, next.speed)
      if (next.area !== undefined) opt.area = next.area
      if (next.fontScale !== undefined) opt.fontScale = next.fontScale
      if (typesChanged) {
        // Drop what a switched-off type already had on screen, and forget its
        // lanes, so it cannot hold a slot against a type that is still on. The
        // comments themselves are kept: later ones of that type come back
        // without another fetch.
        active = active.filter((a) => (opt.mask & kindOf(a.c)) !== 0)
        if (!(opt.mask & KIND_TOP)) topLanes = []
        if (!(opt.mask & KIND_BOTTOM)) bottomLanes = []
        if (!(opt.mask & KIND_SCROLL)) laneFreeAt = new Array(laneCount).fill(0)
      }
      if (speedChanged || areaChanged || scaleChanged || next.force) resize()
    },
    resize,
    /** Draws one frame at `t` seconds of file time. */
    draw,
    clear() {
      comments = []
      reset(0)
      ctx.clearRect(0, 0, W, H)
    },
    get count() {
      return comments.length
    },
  }
}
