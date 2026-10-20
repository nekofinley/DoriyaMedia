<script setup>
/**
 * The little source mark for 番剧 that came from the bilibili source.
 *
 * It is drawn rather than fetched: a badge must not cost a request, and the
 * bilibili CDN rejects a foreign referer anyway. The TV is a simple outline so
 * it stays legible at 10 px, with the wordmark beside it on the brand blue.
 *
 * Place it inside a positioned container (a poster, a still); it pins itself to
 * the top-right corner of that container.
 */
defineProps({
  mini: { type: Boolean, default: false },
})
</script>

<template>
  <span class="bili-badge" :class="{ mini }" title="来自 B 站番剧源" aria-label="B 站番剧">
    <svg class="tv" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
      stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <path d="M7.5 6.5 5 3.5M16.5 6.5 19 3.5" />
      <rect x="2.6" y="6.6" width="18.8" height="14" rx="4.2" />
      <path d="M8 11.5v1.8M16 11.5v1.8" />
    </svg>
    <span class="word">bilibili</span>
  </span>
</template>

<style scoped>
.bili-badge {
  position: absolute;
  top: 7px;
  right: 7px;
  z-index: 3;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  height: 18px;
  padding: 0 6px 0 4px;
  border-radius: 4px;
  background: #00a1d6;
  color: #fff;
  font-family: inherit;
  font-size: 10px;
  font-weight: 800;
  letter-spacing: 0.01em;
  line-height: 1;
  box-shadow: 0 2px 7px rgba(0, 0, 0, 0.34);
  pointer-events: none;
  user-select: none;
}
.bili-badge .tv {
  width: 12px;
  height: 12px;
  flex: none;
}
.bili-badge .word {
  transform: translateY(-0.5px);
}
.bili-badge.mini {
  top: 4px;
  right: 4px;
  height: 14px;
  gap: 3px;
  padding: 0 4px 0 3px;
  border-radius: 3px;
  font-size: 8px;
}
.bili-badge.mini .tv {
  width: 9px;
  height: 9px;
}
</style>
