<script setup>
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import AppIcon from './AppIcon.vue'
import SmartImage from './SmartImage.vue'

const props = defineProps({
  item: { type: Object, required: true },
  index: { type: Number, default: 0 },
})

const labels = {
  1: { text: '想看', tone: 'wish' },
  2: { text: '看过', tone: 'done' },
  3: { text: '在看', tone: 'doing' },
  4: { text: '搁置', tone: 'hold' },
  5: { text: '抛弃', tone: 'drop' },
}

const title = computed(() => props.item.name_cn || props.item.name || `Bangumi #${props.item.subject_id}`)
const original = computed(() => {
  if (!props.item.name_cn || props.item.name_cn === props.item.name) return ''
  return props.item.name || ''
})
const year = computed(() => String(props.item.date || '').slice(0, 4))
const local = computed(() => Number(props.item.local_item_id) > 0)
const target = computed(() => local.value
  ? `/item/${props.item.local_item_id}`
  : `https://bgm.tv/subject/${props.item.subject_id}`)
const tag = computed(() => local.value ? RouterLink : 'a')
const linkAttrs = computed(() => local.value
  ? { to: target.value }
  : { href: target.value, target: '_blank', rel: 'noopener noreferrer' })

const status = computed(() => labels[Number(props.item.collection_type)] || { text: '收藏', tone: 'wish' })
const watched = computed(() => Math.max(0, Number(props.item.ep_status) || 0))
const episodes = computed(() => Math.max(0, Number(props.item.eps) || 0))
const percent = computed(() => episodes.value > 0
  ? Math.min(100, Math.round((watched.value / episodes.value) * 100))
  : 0)
const progressText = computed(() => {
  if (episodes.value > 0) return `${watched.value} / ${episodes.value} 集`
  return watched.value > 0 ? `已看 ${watched.value} 集` : '尚未记录集数'
})
const score = computed(() => Number(props.item.score) > 0 ? Number(props.item.score).toFixed(1) : '')
const meta = computed(() => [year.value, score.value ? `★ ${score.value}` : ''].filter(Boolean).join(' · '))
</script>

<template>
  <component
    :is="tag"
    v-bind="linkAttrs"
    class="bgm-card"
    :style="{ '--i': index }"
    :aria-label="`${title}，${progressText}${local ? '，媒体库可播放' : '，媒体库未收录'}`"
  >
    <div class="art">
      <SmartImage
        :src="item.cover_url"
        :alt="title"
        :seed="title"
        :eager="index < 10"
      />
      <div class="shade"></div>

      <span class="status" :class="status.tone">{{ status.text }}</span>
      <span class="source" :class="{ local }">
        <AppIcon :name="local ? 'play' : 'external'" :size="11" :filled="local" />
        {{ local ? '媒体库' : '未收录' }}
      </span>

      <div class="watch">
        <div class="watch-row">
          <span>{{ progressText }}</span>
          <b v-if="percent">{{ percent }}%</b>
        </div>
        <div class="watch-bar"><i :style="{ width: percent + '%' }"></i></div>
      </div>
    </div>

    <div class="cap">
      <div class="title" :title="title">{{ title }}</div>
      <div class="sub">
        <span v-if="meta">{{ meta }}</span>
        <span v-if="original" class="original" :title="original">{{ original }}</span>
      </div>
    </div>
  </component>
</template>

<style scoped>
.bgm-card {
  min-width: 0;
  display: block;
  color: var(--text-1);
  transition: transform var(--t-mid);
}
.art {
  position: relative;
  aspect-ratio: 2 / 3;
  overflow: hidden;
  border-radius: var(--r-m);
  border: 1px solid var(--line-1);
  background: var(--bg-2);
  box-shadow: var(--sh-1);
  transition: border-color var(--t-mid), box-shadow var(--t-mid);
}
.art :deep(.smart-img) { position: absolute; inset: 0; }
.art :deep(img) { transition: transform var(--t-slow), opacity var(--t-mid); }
.shade {
  position: absolute;
  inset: 0;
  background:
    linear-gradient(to bottom, rgba(3, 4, 7, 0.18), transparent 32%),
    linear-gradient(to top, rgba(3, 4, 7, 0.94), rgba(3, 4, 7, 0.02) 42%);
  pointer-events: none;
}
.status, .source {
  position: absolute;
  top: 9px;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  height: 23px;
  padding: 0 8px;
  border-radius: var(--r-pill);
  border: 1px solid rgba(255, 255, 255, 0.16);
  background: rgba(7, 8, 11, 0.76);
  backdrop-filter: blur(10px);
  -webkit-backdrop-filter: blur(10px);
  color: var(--text-1);
  font-size: 10.5px;
  font-weight: 700;
  letter-spacing: 0.02em;
  white-space: nowrap;
}
.status { left: 9px; }
.status.doing { color: #ffd27b; border-color: rgba(240, 169, 46, 0.42); background: rgba(62, 42, 13, 0.82); }
.status.done { color: #9ef0bb; border-color: rgba(74, 222, 128, 0.34); background: rgba(15, 55, 31, 0.8); }
.status.wish { color: #a8d6ff; border-color: rgba(108, 182, 255, 0.34); background: rgba(16, 42, 66, 0.8); }
.status.hold { color: #e4c38c; border-color: rgba(218, 174, 104, 0.32); background: rgba(55, 43, 23, 0.82); }
.status.drop { color: #d4b8b8; border-color: rgba(196, 140, 140, 0.28); background: rgba(52, 29, 29, 0.82); }
.source { right: 9px; color: var(--text-3); }
.source.local { color: #fff; border-color: rgba(255, 255, 255, 0.24); background: rgba(240, 169, 46, 0.86); color: #211500; }
.watch {
  position: absolute;
  left: 12px;
  right: 12px;
  bottom: 11px;
}
.watch-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin-bottom: 6px;
  color: rgba(255, 255, 255, 0.82);
  font-size: var(--fs-11);
  font-weight: 600;
  font-variant-numeric: tabular-nums;
  text-shadow: 0 1px 8px rgba(0, 0, 0, 0.7);
}
.watch-row b { color: #fff; }
.watch-bar {
  height: 3px;
  overflow: hidden;
  border-radius: var(--r-pill);
  background: rgba(255, 255, 255, 0.25);
  box-shadow: 0 1px 8px rgba(0, 0, 0, 0.35);
}
.watch-bar i {
  display: block;
  height: 100%;
  border-radius: inherit;
  background: var(--brand);
  transition: width 500ms var(--ease-out);
}
.cap { padding: 9px 2px 0; }
.title {
  overflow: hidden;
  color: var(--text-1);
  font-size: var(--fs-13);
  font-weight: 650;
  letter-spacing: -0.006em;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.sub {
  display: flex;
  align-items: center;
  gap: 6px;
  min-height: 18px;
  margin-top: 2px;
  color: var(--text-4);
  font-size: var(--fs-11);
  white-space: nowrap;
}
.original {
  overflow: hidden;
  text-overflow: ellipsis;
}
.original::before { content: "·"; margin-right: 6px; }

.bgm-card:hover { transform: translateY(-5px); }
.bgm-card:hover .art { border-color: var(--line-3); box-shadow: var(--sh-3); }
.bgm-card:hover .art :deep(img) { transform: scale(1.055); }
.bgm-card:active { transform: translateY(-2px) scale(0.99); }

@media (max-width: 720px) {
  .status, .source {
    top: 7px;
    height: 21px;
    padding: 0 7px;
  }
  .status { left: 7px; }
  .source { right: 7px; }
  .watch { left: 9px; right: 9px; bottom: 8px; }
}
@media (max-width: 430px) {
  .source { font-size: 0; gap: 0; width: 23px; justify-content: center; padding: 0; }
}
</style>
