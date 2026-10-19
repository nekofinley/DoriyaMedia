<script setup>
import { computed } from 'vue'

// Inline 24px stroke icon set (no icon-font dependency, no network requests).
const P = {
  home: 'M3 10.6 12 3l9 7.6M5.5 9.2V20a1 1 0 0 0 1 1h11a1 1 0 0 0 1-1V9.2',
  film: 'M4 4h16v16H4zM4 9h16M4 15h16M9 4v16M15 4v16',
  tv: 'M3 6h18v11H3zM8 21h8M12 17v4',
  folder: 'M3 7.5A1.5 1.5 0 0 1 4.5 6h4l2 2.4h7A1.5 1.5 0 0 1 19 10v8a1.5 1.5 0 0 1-1.5 1.5h-13A1.5 1.5 0 0 1 3 18z',
  folderOpen: 'M3 8.5A1.5 1.5 0 0 1 4.5 7h3.6l2 2.4H18a1.5 1.5 0 0 1 1.5 1.5v1H6.2a2 2 0 0 0-1.9 1.4L3 17.6zM3.6 19.2 5.4 13a1.6 1.6 0 0 1 1.5-1.1h14.2l-1.7 6.4a1.6 1.6 0 0 1-1.5 1.2H4.5z',
  play: 'M7 4.6 19 12 7 19.4z',
  playCircle: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM10 8.6 16 12l-6 3.4z',
  pause: 'M8.5 5h3v14h-3zM12.5 5h3v14h-3z',
  back10: 'M11 5 4 9.5 11 14V5zM4 9.5h9a6 6 0 1 1 0 12H7',
  fwd10: 'M13 5l7 4.5L13 14V5zM20 9.5h-9a6 6 0 1 0 0 12h6',
  info: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 11v5M12 7.8h.01',
  search: 'M11 18a7 7 0 1 0 0-14 7 7 0 0 0 0 14zM20 20l-4.2-4.2',
  close: 'M6 6l12 12M18 6L6 18',
  check: 'M4.5 12.6 9.4 17.5 19.5 7',
  plus: 'M12 5v14M5 12h14',
  star: 'M12 3.6l2.6 5.4 5.9.8-4.3 4.1 1 5.9-5.2-2.8-5.2 2.8 1-5.9L3.5 9.8l5.9-.8z',
  chevronLeft: 'M14.5 5 8 12l6.5 7',
  chevronRight: 'M9.5 5 16 12l-6.5 7',
  chevronDown: 'M5 9.5 12 16l7-6.5',
  chevronUp: 'M5 14.5 12 8l7 6.5',
  arrowLeft: 'M20 12H4M10 6l-6 6 6 6',
  settings: 'M12 15.2a3.2 3.2 0 1 0 0-6.4 3.2 3.2 0 0 0 0 6.4zM19.2 14.4a1.6 1.6 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.6 1.6 0 0 0-2.7 1.1V21a2 2 0 1 1-4 0v-.2a1.6 1.6 0 0 0-2.7-1.1l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.6 1.6 0 0 0-1.1-2.7H3a2 2 0 1 1 0-4h.2a1.6 1.6 0 0 0 1.1-2.7l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.6 1.6 0 0 0 2.7-1.1V3a2 2 0 1 1 4 0v.2a1.6 1.6 0 0 0 2.7 1.1l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.6 1.6 0 0 0 1.1 2.7H21a2 2 0 1 1 0 4h-.2a1.6 1.6 0 0 0-1.5 1z',
  refresh: 'M20 11a8 8 0 1 0-1.6 5.6M20 5v6h-6',
  trash: 'M4 7h16M10 4h4M6 7l1 13h10l1-13M10 11v6M14 11v6',
  volume: 'M4 9.5h3.5L12 5.5v13L7.5 14.5H4zM16 9a4 4 0 0 1 0 6M18.6 6.6a7.5 7.5 0 0 1 0 10.8',
  volumeMute: 'M4 9.5h3.5L12 5.5v13L7.5 14.5H4zM16.5 9.5l5 5M21.5 9.5l-5 5',
  fullscreen: 'M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5',
  fullscreenExit: 'M9 4v5H4M15 4v5h5M9 20v-5H4M15 20v-5h5',
  pip: 'M4 5h16v14H4zM13 12h6v5h-6z',
  subtitles: 'M4 5h16v14H4zM7.5 11h3M13 11h3.5M7.5 14.5h5M15 14.5h1.5',
  // 弹幕: the bullet comments as a bubble of text over the picture, struck through
  // for the off state — the control bar's own toggle and its settings button.
  danmaku: 'M4 5.5h16v10.5H10.5L6.5 19.5v-3.5H4zM7.5 9.5h9M7.5 12.5h5',
  danmakuOff: 'M4 5.5h16v10.5H10.5L6.5 19.5v-3.5H4zM7.5 9.5h9M7.5 12.5h5M4.5 20.5l15-17',
  audio: 'M4 9.5h3.5L12 5.5v13L7.5 14.5H4zM16.5 9.6a3.6 3.6 0 0 1 0 4.8M19 7.4a7 7 0 0 1 0 9.2',
  speed: 'M12 20a8 8 0 1 1 8-8M12 12l4.5-3.5M12 20h.01',
  quality: 'M4 6h16v12H4zM8 10h8M8 13h5',
  external: 'M14 4h6v6M20 4l-8.5 8.5M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5',
  cloud: 'M7 18h10a4 4 0 0 0 .6-7.95A6 6 0 0 0 6.1 9.2A4.4 4.4 0 0 0 7 18z',
  server: 'M4 5h16v5H4zM4 14h16v5H4zM8 7.5h.01M8 16.5h.01',
  hdd: 'M4 6h16v10H4zM7 19h10M9.5 9h5',
  globe: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM3.5 9h17M3.5 15h17M12 3c2.5 2.4 3.8 5.4 3.8 9S14.5 18.6 12 21c-2.5-2.4-3.8-5.4-3.8-9S9.5 5.4 12 3z',
  file: 'M6 3h8l4 4v14H6zM14 3v5h4',
  video: 'M4 6h11v12H4zM15 10.5 20 8v8l-5-2.5z',
  image: 'M4 5h16v14H4zM8.5 10a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3zM4 16.5l4.5-4 4 3.5 3-2.5L20 17',
  alert: 'M12 3.5 21 19H3zM12 10v4M12 16.5h.01',
  sparkles: 'M12 4l1.6 4.4L18 10l-4.4 1.6L12 16l-1.6-4.4L6 10l4.4-1.6zM18.5 15l.8 2 2 .8-2 .8-.8 2-.8-2-2-.8 2-.8z',
  clock: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 7.5V12l3 2',
  calendar: 'M4 6h16v14H4zM4 10h16M8.5 3.5v4M15.5 3.5v4',
  list: 'M8 6h12M8 12h12M8 18h12M4 6h.01M4 12h.01M4 18h.01',
  grid: 'M4 4h7v7H4zM13 4h7v7h-7zM4 13h7v7H4zM13 13h7v7h-7z',
  sort: 'M7 5v14M7 19l-3-3M7 19l3-3M17 19V5M17 5l-3 3M17 5l3 3',
  users: 'M16 20v-1.5a4 4 0 0 0-4-4H7a4 4 0 0 0-4 4V20M9.5 10.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7zM21 20v-1.5a4 4 0 0 0-3-3.87M16.5 3.9a4 4 0 0 1 0 7.2',
  logout: 'M15 4h3a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1h-3M10 8l-4 4 4 4M6 12h9',
  lock: 'M6 11h12v9H6zM8.5 11V8a3.5 3.5 0 1 1 7 0v3',
  lockOpen: 'M6 11h12v9H6zM8.5 11V8a3.5 3.5 0 0 1 6.9-.8',
  user: 'M12 12.5a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM5 20.5a7 7 0 0 1 14 0',
  dots: 'M6 12h.01M12 12h.01M18 12h.01',
  filter: 'M4 6h16M7 12h10M10 18h4',
  download: 'M12 4v11M8 11.5l4 4 4-4M5 20h14',
  link: 'M10 13.5a3.5 3.5 0 0 0 5 0l3-3a3.5 3.5 0 0 0-5-5l-1 1M14 10.5a3.5 3.5 0 0 0-5 0l-3 3a3.5 3.5 0 0 0 5 5l1-1',
  wifi: 'M5 12.5a10 10 0 0 1 14 0M8 15.5a6 6 0 0 1 8 0M12 19h.01M2 9.5a14 14 0 0 1 20 0',
  heart: 'M12 20s-7-4.4-7-9.2A4 4 0 0 1 12 8a4 4 0 0 1 7 2.8C19 15.6 12 20 12 20z',
  bookmark: 'M6 4h12v16l-6-4-6 4z',
  eye: 'M2.5 12S6 6 12 6s9.5 6 9.5 6-3.5 6-9.5 6-9.5-6-9.5-6zM12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z',
  eyeOff: 'M4 4l16 16M9.9 5.3A9.6 9.6 0 0 1 12 5c6 0 9.5 7 9.5 7a17 17 0 0 1-2.4 3.3M6.4 7.5A16.8 16.8 0 0 0 2.5 12s3.5 7 9.5 7c1.2 0 2.3-.2 3.3-.6M9.9 9.9a3 3 0 0 0 4.2 4.2',
  zoomIn: 'M11 18a7 7 0 1 0 0-14 7 7 0 0 0 0 14zM20 20l-4.2-4.2M11 8.5v5M8.5 11h5',
  layers: 'M12 3 3 8l9 5 9-5zM3 13l9 5 9-5M3 17.5l9 5 9-5',
  power: 'M12 3v8M7.5 6.2a7 7 0 1 0 9 0',
  scan: 'M4 8V5h3M20 8V5h-3M4 16v3h3M20 16v3h-3M4 12h16',
  wand: 'M5 19 17 7M14.5 4.5l1 1M19 9l1 1M16 4l3 3M4 20l1.5-1.5',
  history: 'M4 12a8 8 0 1 0 2.5-5.8M4 5v4h4M12 8v4.5l3 1.8',
  starHalf: 'M12 3.6v11.2M12 3.6l2.6 5.4 5.9.8-4.3 4.1 1 5.9-5.2-2.8',
  // site editing toolbar
  type: 'M5 7V5h14v2M12 5v14M9 19h6',
  layout: 'M4 4h16v16H4zM10 4v16M10 11h10',
  palette: 'M12 21a9 9 0 1 1 0-18c4.9 0 9 3.6 9 8 0 2.5-2 4-4.5 4H15a2 2 0 0 0-1.5 3.3A2 2 0 0 1 12 21zM7.5 12.5h.01M10 8.5h.01M14.5 7.5h.01M17 11h.01',
  sliders: 'M4 7h9M17 7h3M4 17h3M11 17h9M15 4.2v5.6M9 14.2v5.6',
  undo: 'M8 5 3.5 9.5 8 14M3.5 9.5H14a6 6 0 0 1 0 12H9',
  redo: 'M16 5l4.5 4.5L16 14M20.5 9.5H10a6 6 0 0 0 0 12h5',
  upload: 'M12 16V4M8 8l4-4 4 4M5 16v3a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-3',
  grip: 'M9 6h.01M9 12h.01M9 18h.01M15 6h.01M15 12h.01M15 18h.01',
}

const props = defineProps({
  name: { type: String, required: true },
  size: { type: [Number, String], default: 18 },
  filled: { type: Boolean, default: false },
  stroke: { type: [Number, String], default: 1.7 },
})

const d = computed(() => P[props.name] || P.info)
const px = computed(() => (typeof props.size === 'number' ? `${props.size}px` : props.size))
const isFill = computed(() => props.filled || ['play', 'pause', 'playCircle', 'star', 'heart', 'bookmark'].includes(props.name))
</script>

<template>
  <svg
    :width="px" :height="px" viewBox="0 0 24 24"
    :fill="isFill ? 'currentColor' : 'none'"
    :stroke="isFill ? 'none' : 'currentColor'"
    :stroke-width="stroke" stroke-linecap="round" stroke-linejoin="round"
    aria-hidden="true" focusable="false"
  >
    <path :d="d" />
  </svg>
</template>
