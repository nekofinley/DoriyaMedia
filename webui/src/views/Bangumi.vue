<script setup>
import { computed, onMounted, ref, watch } from 'vue'
import { api } from '../api.js'
import AppIcon from '../components/AppIcon.vue'
import AccountDialog from '../components/AccountDialog.vue'
import BangumiCard from '../components/BangumiCard.vue'
import EmptyState from '../components/EmptyState.vue'
import SkeletonGrid from '../components/SkeletonGrid.vue'
import SmartImage from '../components/SmartImage.vue'

const PAGE_SIZE = 50

const filters = [
  { value: 0, label: '全部' },
  { value: 3, label: '在看' },
  { value: 1, label: '想看' },
  { value: 2, label: '看过' },
  { value: 4, label: '搁置' },
  { value: 5, label: '抛弃' },
]

const activeFilter = ref(0)
const account = ref(null)
const items = ref([])
const total = ref(0)
const offset = ref(0)
const hasMore = ref(false)
const connected = ref(false)
const initialized = ref(false)
const loading = ref(true)
const loadingMore = ref(false)
const error = ref('')
const accountOpen = ref(false)
let requestSeq = 0

const filterLabel = computed(() => filters.find((f) => f.value === activeFilter.value)?.label || '全部')
const accountName = computed(() => account.value?.nickname || account.value?.username || 'Bangumi 用户')
const avatarLetter = computed(() => accountName.value.slice(0, 1).toUpperCase())
const heroCovers = computed(() => items.value.filter((it) => it.cover_url).slice(0, 7))
const shownCount = computed(() => items.value.length)

async function load({ append = false } = {}) {
  if (append && (loadingMore.value || !hasMore.value)) return

  const seq = append ? requestSeq : ++requestSeq
  if (append) {
    loadingMore.value = true
  } else {
    loading.value = true
    error.value = ''
  }

  try {
    const q = new URLSearchParams({
      type: String(activeFilter.value),
      offset: String(append ? offset.value : 0),
      limit: String(PAGE_SIZE),
    })
    const data = await api.get(`/api/bangumi/collections?${q}`)
    if (seq !== requestSeq) return

    const safe = data?.account || data || {}
    account.value = {
      username: safe.username || '',
      nickname: safe.nickname || safe.username || '',
      avatar_url: safe.avatar_url || '',
    }
    connected.value = data?.connected ?? !!safe.username

    const incoming = Array.isArray(data?.items) ? data.items : []
    if (append) {
      const seen = new Set(items.value.map((it) => Number(it.subject_id)))
      items.value.push(...incoming.filter((it) => !seen.has(Number(it.subject_id))))
    } else {
      items.value = incoming
    }

    offset.value = incoming.length > 0
      ? Number(data?.offset || 0) + incoming.length
      : Number(data?.offset || 0)
    total.value = Number(data?.total) || 0
    hasMore.value = !!data?.has_more
    error.value = ''
  } catch (e) {
    if (seq === requestSeq) error.value = e.message
  } finally {
    if (seq === requestSeq) {
      initialized.value = true
      loading.value = false
      loadingMore.value = false
    }
  }
}

function chooseFilter(value) {
  if (activeFilter.value !== value) activeFilter.value = value
}

watch(activeFilter, () => load())
watch(accountOpen, (open, wasOpen) => {
  if (wasOpen && !open) load()
})
onMounted(load)
</script>

<template>
  <div class="page bangumi-page">
    <header class="hero" :class="{ empty: initialized && !connected }">
      <div v-if="connected && heroCovers.length" class="hero-mosaic" aria-hidden="true">
        <SmartImage
          v-for="(cover, i) in heroCovers"
          :key="cover.subject_id"
          :src="cover.cover_url"
          :alt="cover.name_cn || cover.name"
          :seed="cover.name_cn || cover.name"
          :style="{ '--i': i }"
        />
      </div>
      <div class="hero-glow"></div>
      <div class="hero-scrim"></div>

      <div class="hero-inner">
        <div class="hero-copy">
          <div class="eyebrow-row">
            <span class="brand-mark">B</span>
            <span class="eyebrow">Bangumi archive</span>
          </div>
          <h1>我的追番<span>.</span></h1>
          <p v-if="connected">
            把 Bangumi 的收藏、观看进度和本地媒体库放在同一面海报墙上。
          </p>
          <p v-else>
            绑定你的 Bangumi 账户，在这里查看自己的收藏与追番进度。
          </p>
        </div>

        <div v-if="connected" class="identity">
          <span class="avatar">
            <img
              v-if="account?.avatar_url"
              :src="account.avatar_url"
              :alt="accountName"
              referrerpolicy="no-referrer"
            />
            <span v-else>{{ avatarLetter }}</span>
          </span>
          <span class="identity-copy">
            <b>{{ accountName }}</b>
            <span>@{{ account?.username || 'bangumi' }}</span>
          </span>
          <span class="live"><i></i> 已连接</span>
        </div>

        <div v-if="connected" class="hero-count">
          <b>{{ total }}</b>
          <span>部{{ filterLabel }}收藏</span>
        </div>
      </div>
    </header>

    <div v-if="error && connected" class="flash err mt-4">
      <AppIcon name="alert" :size="16" />
      <span>{{ error }}</span>
      <button class="retry" @click="load()">重试</button>
    </div>

    <template v-if="loading && !initialized">
      <div class="loading-head">
        <div class="sk line" style="width:260px;height:24px"></div>
        <div class="sk line mt-3" style="width:170px;height:11px"></div>
      </div>
      <SkeletonGrid :count="14" />
    </template>

    <template v-else-if="!connected && error">
      <EmptyState
        class="mt-5"
        icon="alert"
        title="暂时无法读取 Bangumi 收藏"
        text="账户不一定失效，可能是 Bangumi 或网络暂时不可用。稍后再试一次。"
      >
        <button class="btn" @click="load()">
          <AppIcon name="refresh" :size="15" /> 重新读取
        </button>
      </EmptyState>
    </template>

    <template v-else-if="!connected">
      <section class="connect-panel">
        <div class="connect-icon"><AppIcon name="bookmark" :size="28" /></div>
        <div class="connect-copy">
          <span class="eyebrow">Connect once</span>
          <h2>把 Bangumi 收藏搬到这里</h2>
          <p>
            每位本地用户都可以绑定自己的 Bangumi 账户。绑定后，这里会同步展示你的动画收藏、已看集数，
            以及哪些番已经在媒体库中可以直接播放。
          </p>
          <div class="connect-actions">
            <button class="btn primary" @click="accountOpen = true">
              <AppIcon name="link" :size="16" /> 连接 Bangumi
            </button>
            <a
              class="btn"
              href="https://next.bgm.tv/demo/access-token"
              target="_blank"
              rel="noopener noreferrer"
            >
              <AppIcon name="external" :size="15" /> 获取个人令牌
            </a>
          </div>
        </div>
        <div class="connect-steps">
          <div><b>01</b><span>绑定账户</span><small>令牌加密保存在服务器</small></div>
          <div><b>02</b><span>读取收藏</span><small>弹幕站和本地库分开标记</small></div>
          <div><b>03</b><span>继续观看</span><small>已收录内容直接进入详情</small></div>
        </div>
      </section>
    </template>

    <template v-else>
      <div class="collection-toolbar">
        <div class="filter-scroll no-scrollbar" role="tablist" aria-label="收藏状态">
          <button
            v-for="f in filters"
            :key="f.value"
            class="filter-chip"
            :class="{ on: activeFilter === f.value }"
            role="tab"
            :aria-selected="activeFilter === f.value"
            @click="chooseFilter(f.value)"
          >{{ f.label }}</button>
        </div>
        <div class="toolbar-actions">
          <a
            class="bangumi-link"
            :href="`https://bgm.tv/user/${account?.username || ''}/collections`"
            target="_blank"
            rel="noopener noreferrer"
          >
            在 Bangumi 打开 <AppIcon name="external" :size="12" />
          </a>
          <button class="icon-btn round" title="刷新收藏" :disabled="loading" @click="load()">
            <AppIcon name="refresh" :size="17" :class="{ spin: loading }" />
          </button>
          <button class="icon-btn round" title="账号设置" @click="accountOpen = true">
            <AppIcon name="settings" :size="17" />
          </button>
        </div>
      </div>

      <div class="result-head">
        <div>
          <span class="eyebrow">{{ filterLabel === '全部' ? 'Collection' : filterLabel }}</span>
          <h2>{{ filterLabel === '全部' ? '全部动画收藏' : `${filterLabel}的动画` }}</h2>
        </div>
        <span class="result-count">
          <b>{{ shownCount }}</b> / {{ total }} 部
        </span>
      </div>

      <div v-if="items.length" v-reveal.stagger class="bgm-grid">
        <BangumiCard
          v-for="(item, i) in items"
          :key="item.subject_id"
          :item="item"
          :index="Math.min(i, 18)"
        />
      </div>

      <EmptyState
        v-else-if="!loading"
        icon="bookmark"
        :title="activeFilter ? `没有${filterLabel}的动画` : 'Bangumi 收藏还是空的'"
        text="换一个收藏状态看看，或者到 Bangumi 添加一些动画。"
      >
        <a
          class="btn"
          href="https://bgm.tv/anime"
          target="_blank"
          rel="noopener noreferrer"
        ><AppIcon name="external" :size="15" /> 浏览 Bangumi 动画</a>
      </EmptyState>

      <div v-if="hasMore" class="load-more">
        <button class="btn" :disabled="loadingMore" @click="load({ append: true })">
          <AppIcon name="refresh" :size="15" :class="{ spin: loadingMore }" />
          {{ loadingMore ? '正在读取…' : `继续加载（还有 ${Math.max(0, total - shownCount)} 部）` }}
        </button>
      </div>
    </template>

    <AccountDialog v-model:open="accountOpen" />
  </div>
</template>

<style scoped>
.bangumi-page { padding-top: clamp(20px, 3vw, 36px); }

.hero {
  position: relative;
  min-height: 270px;
  overflow: hidden;
  display: flex;
  align-items: flex-end;
  border-radius: var(--r-xl);
  border: 1px solid rgba(255, 255, 255, 0.09);
  background:
    radial-gradient(80% 140% at 92% -20%, rgba(240, 169, 46, 0.28), transparent 55%),
    radial-gradient(70% 100% at 10% 120%, rgba(70, 117, 189, 0.22), transparent 62%),
    linear-gradient(135deg, #15171c, #090a0d 72%);
  box-shadow: var(--sh-2);
}
.hero.empty { min-height: 220px; }
.hero-mosaic {
  position: absolute;
  inset: -30% -8% -35% 44%;
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 10px;
  transform: rotate(-8deg) skewY(2deg);
  opacity: 0.62;
}
.hero-mosaic :deep(.smart-img) {
  min-height: 0;
  border-radius: 9px;
  box-shadow: 0 18px 42px rgba(0, 0, 0, 0.5);
}
.hero-mosaic :deep(.smart-img:nth-child(2n)) { transform: translateY(42px); }
.hero-mosaic :deep(.smart-img:nth-child(3n)) { transform: translateY(-24px); }
.hero-mosaic :deep(img) { filter: saturate(0.78) brightness(0.72); }
.hero-mosaic :deep(.fallback) { filter: saturate(0.8) brightness(0.7); }
.hero-glow {
  position: absolute;
  width: 360px;
  height: 360px;
  left: 28%;
  top: -250px;
  border-radius: 50%;
  background: rgba(255, 202, 98, 0.26);
  filter: blur(80px);
  pointer-events: none;
}
.hero-scrim {
  position: absolute;
  inset: 0;
  background:
    linear-gradient(90deg, rgba(7, 8, 11, 0.97) 0%, rgba(7, 8, 11, 0.82) 40%, rgba(7, 8, 11, 0.16) 76%),
    linear-gradient(to top, rgba(7, 8, 11, 0.92), transparent 56%);
}
.hero-inner {
  position: relative;
  z-index: 2;
  width: 100%;
  padding: clamp(26px, 4vw, 44px);
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  align-items: end;
  gap: 24px 44px;
}
.hero-copy { grid-column: 1; max-width: 680px; }
.eyebrow-row { display: flex; align-items: center; gap: 9px; margin-bottom: 12px; }
.brand-mark {
  display: grid;
  place-items: center;
  width: 27px;
  height: 27px;
  border-radius: 8px;
  background: #f091a8;
  color: #32101a;
  font-family: var(--font-display);
  font-size: 12px;
  font-weight: 900;
  box-shadow: 0 6px 20px rgba(240, 145, 168, 0.26);
}
.hero-copy .eyebrow { color: #f3b1c1; }
.hero h1 {
  font-family: var(--font-display);
  font-size: clamp(42px, 6.2vw, 82px);
  font-weight: 850;
  letter-spacing: -0.055em;
  line-height: 0.96;
}
.hero h1 span { color: var(--brand); }
.hero-copy p {
  max-width: 48ch;
  margin: 16px 0 0;
  color: var(--text-3);
  font-size: var(--fs-13);
}
.identity {
  grid-column: 1;
  display: flex;
  align-items: center;
  gap: 10px;
  width: fit-content;
  margin-top: -8px;
  padding: 7px 11px 7px 7px;
  border: 1px solid rgba(255, 255, 255, 0.1);
  border-radius: var(--r-pill);
  background: rgba(5, 6, 9, 0.52);
  backdrop-filter: blur(12px);
}
.avatar {
  display: grid;
  place-items: center;
  width: 34px;
  height: 34px;
  overflow: hidden;
  flex: none;
  border-radius: 50%;
  border: 1px solid rgba(255, 255, 255, 0.18);
  background: linear-gradient(145deg, #494d57, #20242b);
  color: #fff;
  font-size: var(--fs-12);
  font-weight: 750;
}
.avatar img { width: 100%; height: 100%; object-fit: cover; }
.identity-copy { display: flex; flex-direction: column; line-height: 1.25; }
.identity-copy b { font-size: var(--fs-12); font-weight: 700; }
.identity-copy span { color: var(--text-4); font-size: 10.5px; }
.live {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  margin-left: 4px;
  color: #a9e7bb;
  font-size: 10.5px;
  font-weight: 650;
}
.live i {
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: var(--ok);
  box-shadow: 0 0 8px rgba(74, 222, 128, 0.72);
}
.hero-count {
  grid-column: 2;
  grid-row: 1 / span 2;
  align-self: center;
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  padding-left: 30px;
  text-align: right;
}
.hero-count b {
  font-family: var(--font-display);
  font-size: clamp(56px, 8vw, 104px);
  font-weight: 800;
  line-height: 0.8;
  letter-spacing: -0.065em;
  font-variant-numeric: tabular-nums;
  text-shadow: 0 8px 32px rgba(0, 0, 0, 0.55);
}
.hero-count span {
  margin-top: 13px;
  color: rgba(255, 255, 255, 0.62);
  font-size: var(--fs-12);
  font-weight: 600;
  letter-spacing: 0.04em;
  white-space: nowrap;
}

.retry {
  margin-left: auto;
  color: var(--brand);
  font-size: var(--fs-12);
  font-weight: 650;
}
.loading-head { margin: var(--sp-6) 0 var(--sp-5); }

.connect-panel {
  position: relative;
  display: grid;
  grid-template-columns: auto minmax(0, 1fr);
  gap: 22px;
  margin-top: var(--sp-5);
  padding: clamp(24px, 4vw, 40px);
  overflow: hidden;
  border: 1px solid var(--line-1);
  border-radius: var(--r-l);
  background:
    radial-gradient(80% 160% at 100% 0%, rgba(240, 169, 46, 0.08), transparent 60%),
    var(--bg-1);
}
.connect-icon {
  display: grid;
  place-items: center;
  width: 58px;
  height: 58px;
  border-radius: 17px;
  color: #f3b1c1;
  border: 1px solid rgba(240, 145, 168, 0.25);
  background: rgba(240, 145, 168, 0.08);
}
.connect-copy { max-width: 780px; }
.connect-copy h2 { margin-top: 5px; font-size: clamp(24px, 3vw, 36px); letter-spacing: -0.035em; }
.connect-copy p { max-width: 68ch; margin: 12px 0 0; color: var(--text-3); line-height: 1.72; }
.connect-actions { display: flex; flex-wrap: wrap; gap: 10px; margin-top: 20px; }
.connect-steps {
  grid-column: 1 / -1;
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 1px;
  margin-top: 6px;
  overflow: hidden;
  border: 1px solid var(--line-1);
  border-radius: var(--r-m);
  background: var(--line-1);
}
.connect-steps div {
  display: grid;
  grid-template-columns: auto 1fr;
  gap: 2px 10px;
  padding: 14px 16px;
  background: rgba(255, 255, 255, 0.022);
}
.connect-steps b { grid-row: 1 / span 2; color: var(--brand); font-family: var(--font-mono); font-size: 11px; }
.connect-steps span { font-size: var(--fs-12); font-weight: 650; }
.connect-steps small { color: var(--text-4); font-size: 10.5px; }

.collection-toolbar {
  position: sticky;
  top: calc(var(--header-h) + 10px);
  z-index: 650;
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  margin: var(--sp-5) 0 var(--sp-6);
  padding: 9px;
  border: 1px solid var(--line-1);
  border-radius: var(--r-pill);
  background: rgba(10, 12, 16, 0.84);
  box-shadow: 0 10px 34px rgba(0, 0, 0, 0.25);
  backdrop-filter: blur(20px) saturate(1.3);
  -webkit-backdrop-filter: blur(20px) saturate(1.3);
}
.filter-scroll { display: flex; align-items: center; gap: 3px; overflow-x: auto; flex: 1; }
.filter-chip {
  height: 34px;
  padding: 0 16px;
  border-radius: var(--r-pill);
  color: var(--text-3);
  font-size: var(--fs-12);
  font-weight: 650;
  white-space: nowrap;
  transition: color var(--t-fast), background var(--t-fast), box-shadow var(--t-fast);
}
.filter-chip:hover { color: var(--text-1); background: rgba(255, 255, 255, 0.05); }
.filter-chip.on {
  color: var(--brand-ink);
  background: var(--brand);
  box-shadow: 0 5px 16px rgba(240, 169, 46, 0.2);
}
.toolbar-actions { display: flex; align-items: center; gap: 2px; flex: none; }
.bangumi-link {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  margin-right: 4px;
  color: var(--text-4);
  font-size: 10.5px;
  font-weight: 600;
}
.bangumi-link:hover { color: var(--text-2); }

.result-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 20px;
  margin-bottom: var(--sp-4);
}
.result-head h2 { margin-top: 5px; font-size: clamp(22px, 2.7vw, 32px); letter-spacing: -0.035em; }
.result-count { color: var(--text-4); font-size: var(--fs-12); font-variant-numeric: tabular-nums; }
.result-count b { color: var(--text-2); font-size: var(--fs-16); }
.bgm-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(min(100%, 164px), 1fr));
  gap: 30px 18px;
}
.load-more { display: flex; justify-content: center; margin-top: var(--sp-7); }
.spin { animation: spin 1s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }

@media (max-width: 900px) {
  .hero-inner { grid-template-columns: minmax(0, 1fr) auto; }
  .hero-mosaic { left: 52%; opacity: 0.48; }
  .bangumi-link { display: none; }
}
@media (max-width: 720px) {
  .bangumi-page { padding-top: 14px; }
  .hero { min-height: 236px; border-radius: var(--r-l); }
  .hero-mosaic { inset: -18% -30% -24% 46%; grid-template-columns: repeat(3, 1fr); opacity: 0.34; }
  .hero-inner {
    padding: 24px 20px;
    gap: 20px;
    grid-template-columns: 1fr;
  }
  .hero-copy p { max-width: 34ch; }
  .identity { grid-column: 1; margin-top: -5px; }
  .hero-count {
    position: absolute;
    right: 20px;
    top: 24px;
    padding: 0;
    align-self: auto;
  }
  .hero-count b { font-size: 48px; }
  .hero-count span { margin-top: 8px; font-size: 10.5px; }
  .connect-panel { grid-template-columns: 1fr; padding: 22px; }
  .connect-icon { width: 48px; height: 48px; border-radius: 14px; }
  .connect-steps { grid-template-columns: 1fr; }
  .connect-steps div { padding: 11px 13px; }
  .collection-toolbar {
    top: calc(var(--header-h) + 6px);
    margin: var(--sp-4) 0 var(--sp-5);
    border-radius: var(--r-m);
    padding: 7px;
  }
  .filter-chip { height: 32px; padding: 0 13px; }
  .toolbar-actions .icon-btn { width: 32px; height: 32px; }
  .result-head { margin-top: var(--sp-2); }
  .bgm-grid { gap: 24px 12px; }
}
@media (max-width: 430px) {
  .hero h1 { font-size: 46px; }
  .hero-copy p { font-size: var(--fs-12); }
  .live { display: none; }
  .connect-actions .btn { flex: 1 1 auto; justify-content: center; }
  .bgm-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .result-count { font-size: 10.5px; }
}
</style>
