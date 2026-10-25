<script setup>
import { ref, computed, watch } from 'vue'

/**
 * Image with graceful behaviour:
 *  - lazy + async decode
 *  - fades in once decoded (no white flash, no pop-in)
 *  - on error / missing art shows a deterministic gradient placeholder
 *    derived from the title, so a library without posters still looks designed.
 */
const props = defineProps({
  src: { type: String, default: '' },
  alt: { type: String, default: '' },
  seed: { type: String, default: '' },
  ratio: { type: String, default: '' },
  eager: { type: Boolean, default: false },
})

const failed = ref(false)
const loaded = ref(false)

watch(() => props.src, () => { failed.value = false; loaded.value = false })

const hue = computed(() => {
  const s = props.seed || props.alt || props.src || 'doriya'
  let h = 0
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) % 997
  return h
})
const fallbackStyle = computed(() => {
  const h = hue.value
  const a = `hsl(${h % 360} 46% 22%)`
  const b = `hsl(${(h * 7) % 360} 58% 9%)`
  const c = `hsl(${(h * 13) % 360} 72% 52%)`
  return {
    background: `radial-gradient(120% 90% at 78% 12%, ${c}33, transparent 58%), linear-gradient(150deg, ${a}, ${b})`,
  }
})
const initials = computed(() => (props.seed || props.alt || '?').trim().slice(0, 2))
</script>

<template>
  <div class="smart-img" :style="ratio ? { aspectRatio: ratio } : null">
    <img
      v-if="src && !failed"
      :src="src" :alt="alt"
      :loading="eager ? 'eager' : 'lazy'"
      :fetchpriority="eager ? 'high' : 'auto'"
      referrerpolicy="no-referrer"
      decoding="async"
      :class="{ in: loaded }"
      @load="loaded = true"
      @error="failed = true"
    />
    <div v-else class="fallback" :style="fallbackStyle">
      <span class="initial">{{ initials }}</span>
    </div>
  </div>
</template>

<style scoped>
.smart-img { position: relative; width: 100%; height: 100%; overflow: hidden; background: var(--bg-2); }
.smart-img img {
  width: 100%; height: 100%; object-fit: cover;
  opacity: 0; transition: opacity 480ms var(--ease-out);
}
.smart-img img.in { opacity: 1; }
.fallback {
  width: 100%; height: 100%;
  display: grid; place-items: center;
}
.fallback .initial {
  font-family: var(--font-display);
  font-size: clamp(20px, 24%, 64px);
  font-weight: 800;
  letter-spacing: -0.04em;
  color: rgba(255, 255, 255, 0.42);
  text-shadow: 0 2px 18px rgba(0, 0, 0, 0.5);
  user-select: none;
}
</style>
