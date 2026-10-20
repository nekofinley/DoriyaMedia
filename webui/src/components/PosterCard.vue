<script setup>
import { computed } from 'vue'
import AppIcon from './AppIcon.vue'
import BiliBadge from './BiliBadge.vue'
import SmartImage from './SmartImage.vue'
import { imgUrl, artIf } from '../api.js'

const props = defineProps({
  item: { type: Object, required: true },
  to: { type: [String, Object], default: '' },
  quickTo: { type: [String, Object], default: '' },
  subtitle: { type: String, default: '' },
  progress: { type: Number, default: 0 },
  rank: { type: Number, default: 0 },
  eager: { type: Boolean, default: false },
  badge: { type: String, default: '' },
})

const target = computed(() => props.to || `/item/${props.item.id}`)
const quick = computed(() => props.quickTo || target.value)
const poster = computed(() => artIf(props.item, 'poster'))
const year = computed(() => (props.item.year ? String(props.item.year) : ''))
const rating = computed(() => (props.item.rating ? Number(props.item.rating).toFixed(1) : ''))
const kind = computed(() => (props.item.type === 'show' ? '剧集' : props.item.type === 'movie' ? '电影' : ''))
const sub = computed(() => props.subtitle || [year.value, kind.value].filter(Boolean).join(' · '))
const pct = computed(() => Math.max(0, Math.min(100, props.progress || 0)))
</script>

<template>
  <div class="poster">
    <router-link :to="target" class="art" :aria-label="item.name">
      <SmartImage :src="poster" :alt="item.name" :seed="item.name" :eager="eager" />
      <BiliBadge v-if="item.source_type === 'bilibili'" />
      <span v-if="rank" class="rank">{{ rank }}</span>
      <span v-if="badge" class="corner-badge">{{ badge }}</span>
      <div class="overlay">
        <div class="meta">
          <span v-if="rating" class="badge rating"><AppIcon name="star" :size="11" filled />{{ rating }}</span>
          <span v-if="year" class="badge">{{ year }}</span>
        </div>
      </div>
      <span class="quick" :title="`播放 ${item.name}`"><AppIcon name="play" :size="20" filled /></span>
      <div v-if="pct > 0" class="progress"><i :style="{ width: pct + '%' }"></i></div>
    </router-link>
    <router-link :to="target" class="cap">
      <div class="t">{{ item.name }}</div>
      <div class="s">{{ sub }}</div>
    </router-link>
    <router-link v-if="quickTo" :to="quick" class="quick-hit" :aria-label="`播放 ${item.name}`" />
  </div>
</template>

<style scoped>
.corner-badge {
  position: absolute;
  top: 8px;
  left: 8px;
  padding: 2px 7px;
  border-radius: var(--r-xs);
  background: rgba(6, 7, 10, 0.72);
  backdrop-filter: blur(8px);
  border: 1px solid var(--line-2);
  font-size: var(--fs-11);
  font-weight: 650;
  color: var(--text-1);
  z-index: 2;
}
.quick-hit { position: absolute; inset: 0; z-index: 4; }
</style>
