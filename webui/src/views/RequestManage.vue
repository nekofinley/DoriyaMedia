<script setup>
import { ref, computed, onMounted, onBeforeUnmount, watch, nextTick } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from '../components/AppIcon.vue'
import EmptyState from '../components/EmptyState.vue'
import RequestPosterCard from '../components/RequestPosterCard.vue'
import SmartImage from '../components/SmartImage.vue'
import { api, fmtDate, fmtRelative } from '../api.js'
import { toast } from '../toast.js'

const route = useRoute()
const router = useRouter()
const tab = ref(route.query.tab === 'completed' ? 'completed' : 'pending')
const pending = ref([])
const completed = ref([])
const loading = ref(true)
const error = ref('')
const detail = ref(null)
const busy = ref(0)
let refreshTimer = null
let refreshInFlight = false
const modal = ref(null)

const current = computed(() => tab.value === 'completed' ? completed.value : pending.value)
const counts = computed(() => ({ pending: pending.value.length, completed: completed.value.length }))
const providerLabel = computed(() => ({
  bilibili: 'B 站片库',
  bangumi: 'Bangumi',
  manual: '手动求片',
}[detail.value?.provider] || '求片'))

watch(tab, (value) => {
  const query = { ...route.query, tab: value }
  router.replace({ query })
})

watch(detail, async (value) => {
  if (!value) return
  await nextTick()
  modal.value?.focus()
})

onMounted(() => {
  load()
  refreshTimer = setInterval(() => {
    if (!document.hidden && !detail.value) load({ silent: true })
  }, 30000)
})
onBeforeUnmount(() => clearInterval(refreshTimer))

async function load({ silent = false } = {}) {
  if (refreshInFlight) return
  refreshInFlight = true
  if (!silent) {
    loading.value = true
    error.value = ''
  }
  try {
    const out = await api.get('/api/admin/requests')
    pending.value = out.pending || []
    completed.value = out.completed || []
    error.value = ''
  } catch (e) {
    if (!silent) error.value = e.message
  } finally {
    refreshInFlight = false
    if (!silent) loading.value = false
  }
}

function replaceRequest(updated) {
  pending.value = pending.value.filter((r) => r.id !== updated.id)
  completed.value = completed.value.filter((r) => r.id !== updated.id)
  if (updated.status === 'completed') completed.value.unshift(updated)
  else pending.value.unshift(updated)
  if (detail.value?.id === updated.id) detail.value = updated
}

async function markDone(req) {
  if (!req || busy.value) return
  busy.value = req.id
  try {
    const out = await api.put(`/api/admin/requests/${req.id}`, { status: 'completed' })
    replaceRequest(out.request)
    toast.ok(`「${req.title}」已移到已处理`)
  } catch (e) {
    toast.err(e.message)
  } finally {
    busy.value = 0
  }
}

async function reopen(req) {
  if (!req || busy.value) return
  busy.value = req.id
  try {
    const out = await api.put(`/api/admin/requests/${req.id}`, { status: 'pending' })
    replaceRequest(out.request)
    toast.info(`「${req.title}」已回到待处理`)
  } catch (e) {
    toast.err(e.message)
  } finally {
    busy.value = 0
  }
}

async function remove(req) {
  if (!req || busy.value) return
  if (!confirm(`删除「${req.title}」这条求片记录？此操作不可恢复。`)) return
  busy.value = req.id
  try {
    await api.del(`/api/admin/requests/${req.id}`)
    pending.value = pending.value.filter((r) => r.id !== req.id)
    completed.value = completed.value.filter((r) => r.id !== req.id)
    if (detail.value?.id === req.id) detail.value = null
    toast.ok('求片记录已删除')
  } catch (e) {
    toast.err(e.message)
  } finally {
    busy.value = 0
  }
}

function startImport(req) {
  if (!req?.source_id || !req?.season_id) return
  router.push({
    path: `/import/${req.source_id}/${req.season_id}`,
    query: {
      library: req.library_id,
      title: req.title,
      cover: req.cover_url || '',
      year: req.year || '',
      request: req.id,
    },
  })
}

function openItem(req) {
  if (req?.item_id) {
    router.push(`/item/${req.item_id}`)
    detail.value = null
  }
}
</script>

<template>
  <div class="manage-page">
    <section class="manage-hero">
      <div class="rings" aria-hidden="true"></div>
      <div class="page hero-inner">
        <div class="hero-copy">
          <router-link to="/requests" class="back-link">
            <AppIcon name="arrowLeft" :size="15" /> 返回求片
          </router-link>
          <span class="eyebrow"><AppIcon name="layers" :size="13" /> Request board</span>
          <h1 class="display-1">把等待变成<br /><em>下一张海报。</em></h1>
          <p>在这里处理用户提交的片单。能直接入库的 B 站番剧会跳转到现有导入流程，成功后自动归档。</p>
        </div>

        <div class="totals">
          <div class="total pending">
            <span class="n">{{ String(counts.pending).padStart(2, '0') }}</span>
            <span>待处理</span>
          </div>
          <div class="total done">
            <span class="n">{{ String(counts.completed).padStart(2, '0') }}</span>
            <span>已处理</span>
          </div>
          <span class="total-note">POSTER QUEUE</span>
        </div>
      </div>
    </section>

    <main class="page manage-body">
      <header class="board-toolbar">
        <div class="segmented board-tabs" role="tablist" aria-label="求片状态">
          <button
            :class="{ on: tab === 'pending' }" role="tab" :aria-selected="tab === 'pending'"
            @click="tab = 'pending'"
          >
            <AppIcon name="clock" :size="15" /> 待处理
            <i>{{ counts.pending }}</i>
          </button>
          <button
            :class="{ on: tab === 'completed' }" role="tab" :aria-selected="tab === 'completed'"
            @click="tab = 'completed'"
          >
            <AppIcon name="check" :size="15" /> 已处理
            <i>{{ counts.completed }}</i>
          </button>
        </div>
        <div class="toolbar-hint">
          <span class="dot"></span>
          {{ tab === 'pending' ? '新提交的求片会出现在这一页' : '入库完成或手动归档的求片' }}
        </div>
      </header>

      <div v-if="error" class="flash err">
        <AppIcon name="alert" :size="16" /> {{ error }}
        <button class="btn sm" style="margin-left:auto" @click="load">重试</button>
      </div>

      <div v-if="loading" class="poster-wall">
        <span v-for="i in 10" :key="i" class="sk-poster"></span>
      </div>

      <Transition v-else name="page" mode="out-in">
        <div :key="tab">
          <div v-if="current.length" class="poster-wall">
            <RequestPosterCard
              v-for="(req, i) in current" :key="req.id" :request="req" :index="i" admin
              @open="detail = $event"
              @complete="markDone"
              @reopen="reopen"
              @import="startImport"
            />
          </div>
          <EmptyState
            v-else-if="tab === 'pending'"
            icon="check" title="待处理片墙空了"
            text="所有求片都已经处理完。新的提交会自动出现在这里。"
          >
            <router-link to="/requests" class="btn">去看看求片入口</router-link>
          </EmptyState>
          <EmptyState
            v-else icon="inbox" title="还没有已处理的求片"
            text="完成入库或手动标记后，求片会从左边移到这一页。"
          />
        </div>
      </Transition>
    </main>

    <Teleport to="body">
      <Transition name="fade">
        <div v-if="detail" class="modal-mask" @click.self="detail = null">
          <div
            ref="modal" class="request-modal" role="dialog" aria-modal="true"
            :aria-label="detail.title" tabindex="-1"
            @keydown.esc="detail = null"
          >
            <button class="modal-close icon-btn" aria-label="关闭" @click="detail = null">
              <AppIcon name="close" :size="19" />
            </button>

            <div class="modal-art">
              <SmartImage :src="detail.cover_url" :alt="detail.title" :seed="detail.title" eager />
              <span class="status-ribbon" :class="detail.status">
                {{ detail.status === 'pending' ? '待处理' : '已处理' }}
              </span>
            </div>

            <div class="modal-copy">
              <span class="eyebrow">{{ providerLabel }}</span>
              <h2>{{ detail.title }}</h2>
              <div v-if="detail.original_title && detail.original_title !== detail.title" class="original">
                {{ detail.original_title }}
              </div>
              <div class="modal-chips">
                <span v-if="detail.year">{{ detail.year }}</span>
                <span v-if="detail.episodes">{{ detail.episodes }} 集</span>
                <span v-if="detail.rating">★ {{ Number(detail.rating).toFixed(1) }}</span>
                <span v-if="detail.season_id">B 站季度 {{ detail.season_id }}</span>
                <span v-if="detail.bgm_id">Bangumi #{{ detail.bgm_id }}</span>
              </div>
              <p class="overview clamp-3">{{ detail.overview || '没有简介。' }}</p>

              <div class="info-row">
                <span>提交人</span>
                <b>{{ detail.user_name || '未知' }}</b>
                <span>提交时间</span>
                <b>{{ fmtRelative(detail.created_at) }}</b>
                <template v-if="detail.completed_at">
                  <span>处理时间</span>
                  <b>{{ fmtDate(detail.completed_at) }}</b>
                </template>
              </div>
              <div v-if="detail.note" class="note-box">
                <span>用户备注</span>
                <p>{{ detail.note }}</p>
              </div>
              <div v-if="detail.admin_note" class="note-box admin-note">
                <span>管理员备注</span>
                <p>{{ detail.admin_note }}</p>
              </div>

              <div class="modal-actions">
                <button class="btn danger sm" :disabled="busy === detail.id" @click="remove(detail)">
                  <AppIcon name="trash" :size="14" /> 删除
                </button>
                <span class="spacer"></span>
                <button
                  v-if="detail.status === 'completed' && detail.item_id"
                  class="btn sm" @click="openItem(detail)"
                ><AppIcon name="play" :size="14" /> 打开条目</button>
                <button
                  v-if="detail.status === 'completed'"
                  class="btn sm" :disabled="busy === detail.id" @click="reopen(detail)"
                ><AppIcon name="refresh" :size="14" /> 退回待处理</button>
                <button
                  v-if="detail.status === 'pending' && detail.provider === 'bilibili'"
                  class="btn sm" @click="startImport(detail)"
                ><AppIcon name="download" :size="14" /> 去入库</button>
                <button
                  v-if="detail.status === 'pending'"
                  class="btn primary sm" :disabled="busy === detail.id" @click="markDone(detail)"
                ><AppIcon name="check" :size="14" /> 标记已处理</button>
              </div>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>

<style scoped>
.manage-page { min-height: calc(100dvh - var(--header-h)); }
.manage-hero {
  position: relative;
  overflow: hidden;
  padding: clamp(32px, 5vw, 68px) 0 clamp(32px, 4vw, 54px);
  border-bottom: 1px solid var(--line-1);
  background:
    radial-gradient(50% 90% at 88% 20%, rgba(240, 169, 46, 0.18), transparent 65%),
    radial-gradient(40% 80% at 4% 100%, rgba(50, 101, 162, 0.14), transparent 70%),
    linear-gradient(180deg, #0b0d12, var(--bg-0));
}
.rings {
  position: absolute;
  width: 520px;
  height: 520px;
  right: -80px;
  top: -250px;
  border-radius: 50%;
  border: 1px solid rgba(240, 169, 46, 0.14);
  box-shadow: 0 0 0 55px rgba(240, 169, 46, 0.025), 0 0 0 110px rgba(240, 169, 46, 0.018);
}
.hero-inner {
  position: relative;
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 40px;
  align-items: end;
  padding-top: 0;
  padding-bottom: 0;
}
.back-link {
  display: flex;
  width: fit-content;
  align-items: center;
  gap: 6px;
  margin-bottom: 28px;
  color: var(--text-3);
  font-size: var(--fs-12);
  font-weight: 600;
  transition: color var(--t-fast), transform var(--t-fast);
}
.back-link:hover { color: var(--text-1); transform: translateX(-3px); }
.hero-copy .eyebrow { display: inline-flex; align-items: center; gap: 7px; }
.hero-copy h1 { margin: 12px 0 15px; }
.hero-copy h1 em { color: var(--brand-2); font-style: normal; }
.hero-copy p { max-width: 62ch; margin: 0; color: var(--text-2); line-height: 1.75; }
.totals {
  display: flex;
  align-items: stretch;
  gap: 1px;
  position: relative;
  padding-top: 26px;
  border: 1px solid var(--line-2);
  border-radius: var(--r-l);
  overflow: hidden;
  background: var(--line-2);
  box-shadow: var(--sh-3);
  backdrop-filter: blur(18px);
  -webkit-backdrop-filter: blur(18px);
}
.total {
  display: flex;
  flex-direction: column;
  justify-content: flex-end;
  min-width: 128px;
  padding: 20px 22px;
  background: rgba(11, 13, 18, 0.92);
}
.total .n {
  font-family: var(--font-display);
  font-size: 44px;
  font-weight: 800;
  line-height: 1;
  letter-spacing: -0.055em;
}
.total.pending .n { color: var(--brand-2); }
.total.done .n { color: #9ef0bb; }
.total > span:last-child { margin-top: 7px; color: var(--text-4); font-size: var(--fs-11); }
.total-note {
  position: absolute;
  left: 22px;
  top: 13px;
  color: var(--text-4);
  font-family: var(--font-mono);
  font-size: 9px;
  letter-spacing: 0.14em;
}

.manage-body { padding-top: clamp(24px, 3.5vw, 44px); }
.board-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 18px;
  margin-bottom: var(--sp-6);
}
.board-tabs { padding: 4px; background: rgba(10, 12, 16, 0.8); }
.board-tabs button { height: 36px; padding: 0 17px; display: inline-flex; align-items: center; gap: 7px; }
.board-tabs button i {
  display: grid;
  place-items: center;
  min-width: 20px;
  height: 18px;
  padding: 0 5px;
  border-radius: var(--r-pill);
  background: rgba(255, 255, 255, 0.06);
  color: var(--text-4);
  font-style: normal;
  font-size: 10px;
  font-variant-numeric: tabular-nums;
}
.board-tabs button.on i { background: var(--brand-tint); color: var(--brand); }
.toolbar-hint { display: flex; align-items: center; gap: 8px; color: var(--text-4); font-size: var(--fs-12); }
.toolbar-hint .dot { width: 6px; height: 6px; border-radius: 50%; background: var(--brand); box-shadow: 0 0 10px rgba(240, 169, 46, 0.65); }
.poster-wall {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(min(100%, 158px), 1fr));
  gap: clamp(24px, 3.2vw, 46px) clamp(12px, 1.4vw, 22px);
}
.sk-poster {
  display: block;
  aspect-ratio: 2 / 3;
  border-radius: var(--r-m);
  background: linear-gradient(100deg, var(--bg-2) 20%, var(--bg-3) 38%, var(--bg-2) 56%);
  background-size: 220% 100%;
  animation: shimmer 1.4s linear infinite;
}

.modal-mask { padding: clamp(12px, 4vw, 48px); }
.request-modal {
  position: relative;
  display: grid;
  grid-template-columns: minmax(180px, 230px) minmax(0, 1fr);
  gap: clamp(20px, 4vw, 38px);
  width: min(880px, 100%);
  max-height: min(86dvh, 760px);
  overflow: auto;
  padding: clamp(16px, 3vw, 28px);
  border: 1px solid var(--line-2);
  border-radius: var(--r-xl);
  background:
    radial-gradient(70% 80% at 5% 0%, rgba(240, 169, 46, 0.07), transparent 65%),
    var(--bg-1);
  box-shadow: var(--sh-4);
  animation: panelIn 300ms var(--ease-out);
}
.request-modal:focus-visible { outline: 2px solid var(--brand); outline-offset: 3px; }
.modal-close { position: absolute; right: 12px; top: 12px; z-index: 3; background: rgba(8, 9, 12, 0.65); }
.modal-art {
  position: relative;
  aspect-ratio: 2 / 3;
  overflow: hidden;
  border: 1px solid var(--line-2);
  border-radius: var(--r-l);
  box-shadow: var(--sh-3);
}
.modal-art :deep(.smart-img) { position: absolute; inset: 0; }
.status-ribbon {
  position: absolute;
  left: 10px;
  top: 10px;
  padding: 4px 9px;
  border-radius: var(--r-pill);
  background: rgba(7, 8, 12, 0.78);
  border: 1px solid rgba(108, 182, 255, 0.32);
  color: #a8d6ff;
  font-size: 10px;
  font-weight: 700;
  backdrop-filter: blur(10px);
  -webkit-backdrop-filter: blur(10px);
}
.status-ribbon.completed { color: #9ef0bb; border-color: rgba(74, 222, 128, 0.3); }
.modal-copy { min-width: 0; padding-top: 6px; }
.modal-copy h2 { margin: 8px 0 3px; font-size: clamp(24px, 3.2vw, 38px); letter-spacing: -0.03em; }
.original { color: var(--text-3); font-size: var(--fs-13); }
.modal-chips { display: flex; flex-wrap: wrap; gap: 6px; margin: 14px 0; }
.modal-chips span {
  padding: 4px 9px;
  border: 1px solid var(--line-2);
  border-radius: var(--r-pill);
  color: var(--text-3);
  font-size: var(--fs-11);
}
.overview { color: var(--text-2); line-height: 1.75; font-size: var(--fs-13); }
.info-row {
  display: grid;
  grid-template-columns: auto 1fr auto 1fr;
  gap: 6px 12px;
  padding: 12px 0;
  border-top: 1px solid var(--line-1);
  border-bottom: 1px solid var(--line-1);
  font-size: var(--fs-11);
}
.info-row span { color: var(--text-4); }
.info-row b { color: var(--text-2); font-weight: 600; }
.note-box {
  margin-top: 12px;
  padding: 11px 13px;
  border: 1px solid var(--line-1);
  border-radius: var(--r-s);
  background: rgba(255, 255, 255, 0.025);
}
.note-box > span { color: var(--text-4); font-size: 10px; font-weight: 700; letter-spacing: 0.08em; text-transform: uppercase; }
.note-box p { margin: 4px 0 0; color: var(--text-2); font-size: var(--fs-13); line-height: 1.65; white-space: pre-wrap; }
.note-box.admin-note { border-color: rgba(240, 169, 46, 0.2); background: var(--brand-tint); }
.modal-actions { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-top: 18px; }
.modal-actions .spacer { flex: 1; }

@keyframes shimmer { to { background-position: -120% 0; } }

@media (max-width: 820px) {
  .hero-inner { grid-template-columns: 1fr; align-items: start; }
  .totals { justify-self: start; }
}
@media (max-width: 720px) {
  .manage-hero { padding-top: 24px; }
  .back-link { margin-bottom: 22px; }
  .hero-copy p { font-size: var(--fs-13); }
  .totals { padding-top: 22px; }
  .total { min-width: 104px; padding: 16px 18px; }
  .total .n { font-size: 34px; }
  .total-note { left: 18px; top: 10px; }
  .board-toolbar { align-items: flex-start; flex-direction: column; }
  .board-tabs { width: 100%; }
  .board-tabs button { flex: 1; justify-content: center; }
  .toolbar-hint { padding-left: 2px; }
  .poster-wall { grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 20px 9px; }
  .request-modal { grid-template-columns: 112px minmax(0, 1fr); gap: 14px; padding: 14px; }
  .modal-close { right: 8px; top: 8px; }
  .modal-copy { padding-top: 22px; }
  .modal-copy h2 { font-size: var(--fs-20); }
  .overview { display: none; }
  .info-row { grid-template-columns: auto 1fr; }
  .modal-actions { justify-content: stretch; }
  .modal-actions .spacer { display: none; }
  .modal-actions .btn { flex: 1; }
}
@media (max-width: 430px) {
  .request-modal { grid-template-columns: 92px minmax(0, 1fr); }
  .modal-chips span:nth-child(n+3) { display: none; }
}
</style>
