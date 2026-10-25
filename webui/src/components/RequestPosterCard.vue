<script setup>
import { computed } from 'vue'
import AppIcon from './AppIcon.vue'
import SmartImage from './SmartImage.vue'
import { fmtRelative } from '../api.js'

const props = defineProps({
  request: { type: Object, required: true },
  index: { type: Number, default: 0 },
  admin: { type: Boolean, default: false },
})
const emit = defineEmits(['open', 'complete', 'reopen', 'import'])

const providerLabel = computed(() => ({
  bilibili: 'B 站',
  bangumi: 'Bangumi',
  manual: '手动',
}[props.request.provider] || '求片'))
const pending = computed(() => props.request.status === 'pending')
const canImport = computed(() => pending.value && props.request.provider === 'bilibili'
  && props.request.source_id > 0 && props.request.season_id > 0)
const meta = computed(() => [
  props.request.year || '',
  props.request.episodes ? `${props.request.episodes} 集` : '',
].filter(Boolean).join(' · '))
const when = computed(() => fmtRelative(props.request.completed_at || props.request.created_at))
</script>

<template>
  <article class="request-card" :style="{ '--i': Math.min(index, 12) }">
    <div class="art-wrap">
      <button class="art" type="button" :aria-label="`查看求片：${request.title}`" @click="emit('open', request)">
        <SmartImage
          :src="request.cover_url"
          :alt="request.title"
          :seed="request.title"
          :eager="index < 8"
        />
        <span class="shade"></span>
        <span class="provider">{{ providerLabel }}</span>
        <span class="state" :class="{ done: !pending }">
          <AppIcon :name="pending ? 'clock' : 'check'" :size="11" />
          {{ pending ? '待处理' : '已处理' }}
        </span>
        <span class="requester">
          <i>{{ (request.user_name || '?').slice(0, 1).toUpperCase() }}</i>
          <span class="truncate">{{ request.user_name || '未知用户' }}</span>
        </span>
      </button>

      <div v-if="admin" class="card-actions">
        <button
          v-if="canImport" class="mini primary" type="button"
          @click="emit('import', request)"
        ><AppIcon name="download" :size="12" /> 入库</button>
        <button
          v-if="pending" class="mini" type="button"
          @click="emit('complete', request)"
        ><AppIcon name="check" :size="12" /> 完成</button>
        <button
          v-else class="mini" type="button"
          @click="emit('reopen', request)"
        ><AppIcon name="refresh" :size="12" /> 重开</button>
      </div>
    </div>

    <button class="cap" type="button" @click="emit('open', request)">
      <span class="title">{{ request.title }}</span>
      <span class="sub">
        <span>{{ meta || providerLabel }}</span>
        <span v-if="when">{{ when }}</span>
      </span>
    </button>
  </article>
</template>

<style scoped>
.request-card {
  min-width: 0;
  animation: cardIn 520ms var(--ease-out) backwards;
  animation-delay: calc(var(--i) * 38ms);
}
.art-wrap { position: relative; }
.art {
  position: relative;
  display: block;
  width: 100%;
  aspect-ratio: 2 / 3;
  overflow: hidden;
  border-radius: var(--r-m);
  border: 1px solid var(--line-1);
  background: var(--bg-2);
  box-shadow: var(--sh-1);
  transition: transform var(--t-mid), border-color var(--t-mid), box-shadow var(--t-mid);
}
.art :deep(.smart-img) { position: absolute; inset: 0; }
.art :deep(img) { transition: transform var(--t-slow); }
.shade {
  position: absolute;
  inset: 0;
  pointer-events: none;
  background:
    linear-gradient(to bottom, rgba(3, 4, 7, 0.34), transparent 30%),
    linear-gradient(to top, rgba(3, 4, 7, 0.96), transparent 44%);
}
.provider, .state {
  position: absolute;
  top: 9px;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  height: 23px;
  padding: 0 8px;
  border-radius: var(--r-pill);
  border: 1px solid rgba(255, 255, 255, 0.14);
  background: rgba(6, 7, 10, 0.78);
  backdrop-filter: blur(10px);
  -webkit-backdrop-filter: blur(10px);
  font-size: 10.5px;
  font-weight: 700;
  letter-spacing: 0.02em;
  white-space: nowrap;
}
.provider { left: 9px; color: var(--brand-2); }
.state { right: 9px; color: #a8d6ff; border-color: rgba(108, 182, 255, 0.28); }
.state.done { color: #9ef0bb; border-color: rgba(74, 222, 128, 0.3); }
.requester {
  position: absolute;
  left: 10px;
  right: 10px;
  bottom: 10px;
  display: flex;
  align-items: center;
  gap: 7px;
  color: rgba(255, 255, 255, 0.86);
  font-size: var(--fs-11);
  font-weight: 600;
  text-align: left;
}
.requester i {
  display: grid;
  place-items: center;
  width: 22px;
  height: 22px;
  flex: none;
  border-radius: 50%;
  background: rgba(255, 255, 255, 0.14);
  border: 1px solid rgba(255, 255, 255, 0.18);
  font-style: normal;
  font-size: 10px;
  font-weight: 800;
}
.card-actions {
  position: absolute;
  left: 8px;
  right: 8px;
  bottom: 38px;
  display: flex;
  gap: 6px;
  opacity: 0;
  transform: translateY(5px);
  transition: opacity var(--t-fast), transform var(--t-fast);
  z-index: 2;
}
.mini {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 4px;
  height: 29px;
  padding: 0 10px;
  border-radius: var(--r-xs);
  border: 1px solid rgba(255, 255, 255, 0.18);
  background: rgba(8, 9, 12, 0.88);
  color: var(--text-1);
  font-size: var(--fs-11);
  font-weight: 700;
  backdrop-filter: blur(12px);
  -webkit-backdrop-filter: blur(12px);
}
.mini.primary { background: var(--brand); border-color: transparent; color: var(--brand-ink); }
.mini:hover { border-color: var(--line-3); background: var(--bg-3); }
.mini.primary:hover { background: var(--accent-hover); }
.cap {
  display: block;
  width: 100%;
  padding: 9px 2px 0;
  text-align: left;
}
.title {
  display: block;
  overflow: hidden;
  font-size: var(--fs-13);
  font-weight: 650;
  color: var(--text-1);
  text-overflow: ellipsis;
  white-space: nowrap;
}
.sub {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin-top: 2px;
  color: var(--text-4);
  font-size: var(--fs-11);
}
.sub span:first-child {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.sub span:last-child { flex: none; }

.request-card:hover .art { border-color: var(--line-3); box-shadow: var(--sh-3); transform: translateY(-4px); }
.request-card:hover .art :deep(img) { transform: scale(1.055); }
.request-card:hover .card-actions { opacity: 1; transform: none; }
.request-card:focus-within .card-actions { opacity: 1; transform: none; }

@media (hover: none) {
  .card-actions { opacity: 1; transform: none; }
}
@media (max-width: 720px) {
  .provider, .state { top: 7px; height: 21px; padding: 0 7px; }
  .provider { left: 7px; }
  .state { right: 7px; }
  .requester { left: 8px; right: 8px; bottom: 8px; }
  .card-actions { left: 7px; right: 7px; bottom: 35px; }
  .mini { height: 27px; padding: 0 8px; }
}
</style>
