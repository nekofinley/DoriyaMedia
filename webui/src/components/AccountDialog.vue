<script setup>
import { ref, computed, watch, onBeforeUnmount } from 'vue'
import { api, fmtRelative } from '../api.js'
import { me, resetAuth, logout } from '../auth.js'
import AppIcon from './AppIcon.vue'
import { toast } from '../toast.js'

const props = defineProps({
  open: { type: Boolean, default: false },
})
const emit = defineEmits(['update:open'])

const name = ref('')
const email = ref('')
const current = ref('')
const next = ref('')
const confirm = ref('')
const busy = ref(false)
const emailBusy = ref(false)
const error = ref('')
const notice = ref('')

const bgm = ref(null)
const bgmLoading = ref(false)
const bgmBusy = ref(false)
const bgmToken = ref('')
const bgmShowToken = ref(false)
const bgmError = ref('')
const bgmNotice = ref('')
let bgmPoll = null
let bgmFetching = false
let bgmLoadSeq = 0

const emailPending = computed(() => !!me.value?.email && !me.value?.email_verified)
const bgmLastSync = computed(() => bgm.value?.last_sync_at ? `上次同步 ${fmtRelative(bgm.value.last_sync_at)}` : '尚未同步')

function close() { emit('update:open', false) }

// Every open starts from the account as the server knows it, never from what
// was typed into a previous, abandoned edit.
watch(() => props.open, (v) => {
  if (!v) {
    bgmLoadSeq++
    bgmFetching = false
    bgmLoading.value = false
    stopBgmPolling()
    return
  }
  name.value = me.value?.name || ''
  email.value = me.value?.email || ''
  current.value = next.value = confirm.value = ''
  error.value = ''
  notice.value = ''
  bgmToken.value = ''
  bgmShowToken.value = false
  bgmError.value = bgmNotice.value = ''
  bgm.value = null
  bgmLoading.value = true
  loadBangumi()
})

onBeforeUnmount(stopBgmPolling)

function stopBgmPolling() {
  if (bgmPoll) {
    clearInterval(bgmPoll)
    bgmPoll = null
  }
}

// The worker finishes in the background after the progress save returns. A
// light poll while jobs are pending makes the counters truthful without making
// every account-dialog open pay for it.
function syncBgmPolling() {
  // A failed job will not resolve on its own, so polling it every few seconds
  // only burns requests. Pending/processing work is the only changing state.
  const active = !!bgm.value?.connected && !!bgm.value.sync_enabled && bgm.value.pending > 0
  if (active && !bgmPoll) {
    bgmPoll = setInterval(() => loadBangumi({ silent: true }), 4000)
  } else if (!active) {
    stopBgmPolling()
  }
}

async function loadBangumi({ silent = false } = {}) {
  if (bgmFetching) return
  bgmFetching = true
  const seq = ++bgmLoadSeq
  if (!silent) bgmLoading.value = true
  try {
    const data = await api.get('/api/bangumi/account')
    if (seq !== bgmLoadSeq || !props.open) return
    bgm.value = data
    bgmError.value = ''
    syncBgmPolling()
  } catch (e) {
    if (seq !== bgmLoadSeq || !props.open) return
    bgmError.value = e.message
  } finally {
    if (seq === bgmLoadSeq) {
      bgmFetching = false
      if (!silent) bgmLoading.value = false
    }
  }
}

async function connectBangumi() {
  bgmError.value = bgmNotice.value = ''
  if (!bgmToken.value.trim()) return
  bgmBusy.value = true
  try {
    bgm.value = await api.put('/api/bangumi/account', { token: bgmToken.value.trim() })
    bgmToken.value = ''
    bgmShowToken.value = false
    bgmNotice.value = `已连接 Bangumi 账户 @${bgm.value.username}`
    syncBgmPolling()
    toast.ok(bgmNotice.value)
  } catch (e) {
    bgmError.value = e.message
  } finally {
    bgmBusy.value = false
  }
}

async function toggleBangumiSync(event) {
  const wanted = !!event.target.checked
  bgmError.value = bgmNotice.value = ''
  bgmBusy.value = true
  try {
    bgm.value = await api.put('/api/bangumi/account/settings', { sync_enabled: wanted })
    syncBgmPolling()
    bgmNotice.value = wanted ? '已开启播放进度同步' : '已暂停播放进度同步'
  } catch (e) {
    event.target.checked = !wanted
    bgmError.value = e.message
  } finally {
    bgmBusy.value = false
  }
}

async function backfillBangumi() {
  bgmError.value = bgmNotice.value = ''
  bgmBusy.value = true
  try {
    const res = await api.post('/api/bangumi/account/sync')
    bgm.value = res.status
    syncBgmPolling()
    bgmNotice.value = res.queued > 0
      ? `已把 ${res.queued} 条看完记录放进同步队列`
      : '没有需要补同步的看完记录'
    toast.ok(bgmNotice.value)
  } catch (e) {
    bgmError.value = e.message
  } finally {
    bgmBusy.value = false
  }
}

async function disconnectBangumi() {
  if (!window.confirm('断开后，本账号将停止同步 Bangumi，服务器也会删除保存的令牌。继续吗？')) return
  bgmError.value = bgmNotice.value = ''
  bgmBusy.value = true
  try {
    await api.del('/api/bangumi/account')
    bgm.value = { connected: false, sync_enabled: false, pending: 0, failed: 0, synced: 0 }
    syncBgmPolling()
    bgmShowToken.value = false
    bgmNotice.value = '已断开 Bangumi 账户'
    toast.ok(bgmNotice.value)
  } catch (e) {
    bgmError.value = e.message
  } finally {
    bgmBusy.value = false
  }
}

async function rename() {
  error.value = ''
  notice.value = ''
  if (!name.value.trim() || name.value === me.value?.name) return
  busy.value = true
  try {
    const res = await api.put('/api/auth/profile', { name: name.value })
    if (res?.user) me.value = res.user
    toast.ok('用户名已更新')
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}

// Binding an address is what makes "forgot password" possible, so it is offered
// to everyone — not only to people who registered after that feature existed.
async function saveEmail() {
  error.value = ''
  notice.value = ''
  const wanted = email.value.trim()
  if (!wanted || wanted === me.value?.email) return
  emailBusy.value = true
  try {
    const res = await api.put('/api/auth/email', { email: wanted })
    if (res?.user) me.value = res.user
    if (res?.ok) {
      notice.value = res.message || '验证邮件已发送'
      toast.ok(notice.value)
    } else {
      // The address was saved but the mail did not go out; saying "success"
      // here would leave someone waiting for a message that never comes.
      error.value = res?.error || '验证邮件发送失败'
    }
  } catch (e) {
    error.value = e.message
  } finally {
    emailBusy.value = false
  }
}

async function resendVerification() {
  error.value = ''
  notice.value = ''
  emailBusy.value = true
  try {
    const res = await api.post('/api/auth/email/resend')
    notice.value = res?.message || '验证邮件已重新发送'
  } catch (e) {
    error.value = e.message
  } finally {
    emailBusy.value = false
  }
}

async function changePassword() {
  error.value = ''
  notice.value = ''
  if (next.value !== confirm.value) {
    error.value = '两次输入的新密码不一致'
    return
  }
  busy.value = true
  try {
    await api.post('/api/auth/password', { current_password: current.value, new_password: next.value })
    current.value = next.value = confirm.value = ''
    toast.ok('密码已修改,其他设备需要重新登录')
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}

function signOut() {
  close()
  logout()
}
</script>

<template>
  <Transition name="modal">
    <div v-if="open" class="modal-mask" @click.self="close">
      <div class="modal acct" role="dialog" aria-modal="true" aria-label="账号设置">
        <div class="modal-head">
          <span class="av lg">{{ (me?.name || 'D').slice(0, 1).toUpperCase() }}</span>
          <div class="grow">
            <div class="acct-name">{{ me?.name }}</div>
            <div class="tiny">
              <span class="badge" :class="me?.role === 'admin' ? 'rating' : ''">
                {{ me?.role === 'admin' ? '管理员' : '普通用户' }}
              </span>
            </div>
          </div>
          <button class="icon-btn" aria-label="关闭" @click="close"><AppIcon name="close" :size="18" /></button>
        </div>

        <Transition name="fade">
          <div v-if="error" class="flash err"><AppIcon name="alert" :size="16" /> {{ error }}</div>
        </Transition>
        <Transition name="fade">
          <div v-if="notice" class="flash ok"><AppIcon name="check" :size="16" /> {{ notice }}</div>
        </Transition>

        <div class="field">
          <label for="acct-name">用户名</label>
          <div class="inline">
            <input id="acct-name" v-model="name" :disabled="busy" @keyup.enter="rename" />
            <button class="btn sm" :disabled="busy || !name.trim() || name === me?.name" @click="rename">改名</button>
          </div>
          <span class="hint">登录时使用的名字,改完立即生效。</span>
        </div>

        <!-- 邮箱:忘记密码时唯一的自救途径,所以每个账号都能绑 -->
        <div class="field">
          <label for="acct-email">邮箱</label>
          <div class="inline">
            <input
              id="acct-email" v-model="email" type="email" autocomplete="email"
              :placeholder="me?.email ? '' : 'you@example.com'" :disabled="emailBusy"
              @keyup.enter="saveEmail"
            />
            <button
              class="btn sm"
              :disabled="emailBusy || !email.trim() || email.trim() === (me?.email || '')"
              @click="saveEmail"
            >{{ me?.email ? '换绑' : '绑定' }}</button>
          </div>
          <span v-if="me?.email" class="hint">
            <template v-if="me.email_verified">
              <span class="badge ok">已验证</span> 忘记密码时可以用这个邮箱重置。
            </template>
            <template v-else>
              <span class="badge warn">未验证</span> 还没点邮件里的链接
              <button class="linkish" :disabled="emailBusy" @click="resendVerification">重新发送</button>
            </template>
          </span>
          <span v-else class="hint">绑定后忘记密码可以自己重置,不必找管理员。</span>
        </div>

        <!-- Bangumi:每个账号连接自己的令牌,同步只写入这个账号的收藏 -->
        <section class="bgm">
          <div class="label-row bgm-title">
            <AppIcon name="tv" :size="15" />
            <span>Bangumi 追番同步</span>
            <a
              class="bgm-help"
              href="https://next.bgm.tv/demo/access-token"
              target="_blank"
              rel="noopener noreferrer"
            >获取个人令牌 <AppIcon name="external" :size="12" /></a>
          </div>

          <div v-if="bgmLoading" class="bgm-loading">
            <span class="sk bgm-sk"></span>
            <span class="tiny">正在读取连接状态…</span>
          </div>

          <template v-else-if="bgm?.connected">
            <div class="bgm-account">
              <span class="bgm-avatar">
                <img
                  v-if="bgm.avatar_url"
                  :src="bgm.avatar_url"
                  :alt="bgm.nickname || bgm.username"
                  referrerpolicy="no-referrer"
                />
                <span v-else>{{ (bgm.nickname || bgm.username || 'B').slice(0, 1) }}</span>
              </span>
              <div class="grow">
                <div class="bgm-name">{{ bgm.nickname || bgm.username }}</div>
                <div class="tiny">@{{ bgm.username }} · {{ bgmLastSync }}</div>
              </div>
              <span class="badge ok"><AppIcon name="check" :size="11" /> 已连接</span>
            </div>

            <div class="bgm-stats">
              <span><b>{{ bgm.synced || 0 }}</b> 条已同步</span>
              <span><b>{{ bgm.pending || 0 }}</b> 条等待中</span>
              <span :class="{ err: bgm.failed > 0 }"><b>{{ bgm.failed || 0 }}</b> 条失败</span>
            </div>

            <router-link to="/bangumi" class="bgm-shelf-link" @click="close">
              <span><AppIcon name="bookmark" :size="15" /> 打开追番海报墙</span>
              <AppIcon name="chevronRight" :size="15" />
            </router-link>

            <label class="bgm-switch">
              <input
                type="checkbox"
                :checked="bgm.sync_enabled"
                :disabled="bgmBusy"
                @change="toggleBangumiSync"
              />
              <span>
                播放到 <b>90%</b> 时，自动把这一集标记为「看过」
                <small>
                  只处理已匹配 Bangumi 条目的番剧剧集；条目不在收藏中时会自动加入「在看」，电影不会同步。
                  多季番剧请在各季详情中用「按 ID 匹配」绑定对应条目，避免集数写到其他季度。
                </small>
              </span>
            </label>

            <div v-if="bgm.last_error" class="bgm-error">
              <AppIcon name="alert" :size="14" />
              <span>{{ bgm.last_error }}</span>
            </div>

            <div class="bgm-actions">
              <button class="btn sm" :disabled="bgmBusy || !bgm.sync_enabled" @click="backfillBangumi">
                <AppIcon name="history" :size="14" /> 补同步已看完剧集
              </button>
              <button class="btn ghost sm" :disabled="bgmBusy" @click="loadBangumi">
                <AppIcon name="refresh" :size="14" /> 刷新
              </button>
              <button class="btn ghost sm" :disabled="bgmBusy" @click="bgmShowToken = !bgmShowToken">
                <AppIcon name="lock" :size="14" /> {{ bgmShowToken ? '取消更换' : '更换令牌' }}
              </button>
              <button class="btn ghost sm bgm-disconnect" :disabled="bgmBusy" @click="disconnectBangumi">
                <AppIcon name="trash" :size="14" /> 断开
              </button>
            </div>
          </template>

          <template v-else>
            <p class="bgm-intro">
              连接后，这台服务器只同步<b>你个人</b>的 Bangumi 收藏与章节进度。每个用户可以使用不同的 Bangumi 账户。
            </p>
          </template>

          <div v-if="!bgmLoading && (!bgm?.connected || bgmShowToken)" class="bgm-connect">
            <div class="inline">
              <input
                v-model="bgmToken"
                type="password"
                autocomplete="off"
                spellcheck="false"
                :placeholder="bgm?.connected ? '输入新的个人访问令牌' : '粘贴 Bangumi 个人访问令牌'"
                :disabled="bgmBusy"
                @keyup.enter="connectBangumi"
              />
              <button class="btn sm primary" :disabled="bgmBusy || !bgmToken.trim()" @click="connectBangumi">
                <AppIcon :name="bgmBusy ? 'refresh' : 'link'" :size="14" :class="{ spin: bgmBusy }" />
                {{ bgmBusy ? '验证中…' : (bgm?.connected ? '更换' : '连接') }}
              </button>
            </div>
            <span class="hint">
              在 Bangumi 的「个人令牌」页生成后直接粘贴。令牌会加密保存在服务器上，接口永远不会回显。
            </span>
          </div>

          <Transition name="fade">
            <div v-if="bgmError" class="flash err mt-3"><AppIcon name="alert" :size="15" /> {{ bgmError }}</div>
          </Transition>
          <Transition name="fade">
            <div v-if="bgmNotice" class="flash ok mt-3"><AppIcon name="check" :size="15" /> {{ bgmNotice }}</div>
          </Transition>
        </section>

        <form class="pw" @submit.prevent="changePassword">
          <div class="label-row"><AppIcon name="lock" :size="14" /> 修改密码</div>
          <div class="field">
            <label for="acct-cur">当前密码</label>
            <input id="acct-cur" v-model="current" type="password" autocomplete="current-password" :disabled="busy" required />
          </div>
          <div class="field">
            <label for="acct-new">新密码</label>
            <input id="acct-new" v-model="next" type="password" autocomplete="new-password" :disabled="busy" required />
          </div>
          <div class="field">
            <label for="acct-confirm">确认新密码</label>
            <input id="acct-confirm" v-model="confirm" type="password" autocomplete="new-password" :disabled="busy" required />
            <span class="hint">改完只有当前设备保持登录,其他设备需要重新登录。</span>
          </div>
          <button class="btn primary block" :disabled="busy || !current || !next">
            {{ busy ? '请稍候…' : '修改密码' }}
          </button>
        </form>

        <div class="acct-foot">
          <button class="btn ghost sm" @click="signOut"><AppIcon name="logout" :size="15" /> 退出登录</button>
        </div>
      </div>
    </div>
  </Transition>
</template>

<style scoped>
.acct { max-width: 500px; width: 100%; }
.modal-head .av.lg { width: 42px; height: 42px; font-size: var(--fs-16); }
.grow { flex: 1; min-width: 0; }
.acct-name { font-size: var(--fs-16); font-weight: 700; margin-bottom: 3px; }
.icon-btn { color: var(--text-4); }
.icon-btn:hover { color: var(--text-1); background: rgba(255, 255, 255, 0.07); }
.inline { display: flex; gap: 8px; align-items: center; }
.inline input { flex: 1; }
.bgm {
  border-top: 1px solid var(--line-1);
  border-bottom: 1px solid var(--line-1);
  margin-top: var(--sp-4);
  padding: var(--sp-4) 0;
}
.bgm-title { margin-bottom: var(--sp-3); }
.bgm-help {
  margin-left: auto;
  display: inline-flex;
  align-items: center;
  gap: 3px;
  color: var(--brand);
  font-size: var(--fs-11);
  font-weight: 500;
  text-decoration: none;
}
.bgm-help:hover { text-decoration: underline; text-underline-offset: 3px; }
.bgm-loading { display: flex; align-items: center; gap: 10px; }
.bgm-sk { width: 34px; height: 34px; border-radius: 50%; flex: none; }
.bgm-account { display: flex; align-items: center; gap: 10px; }
.bgm-avatar {
  width: 38px; height: 38px; flex: none; overflow: hidden;
  border-radius: 50%; display: grid; place-items: center;
  background: linear-gradient(145deg, #31343a, #181a1e);
  border: 1px solid var(--line-2);
  color: var(--text-2); font-weight: 700;
}
.bgm-avatar img { width: 100%; height: 100%; object-fit: cover; }
.bgm-name { font-weight: 650; color: var(--text-1); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.bgm-stats {
  display: grid; grid-template-columns: repeat(3, 1fr); gap: 8px;
  margin: var(--sp-3) 0;
}
.bgm-stats span {
  display: flex; flex-direction: column; gap: 2px;
  padding: 8px 10px; border-radius: var(--r-m);
  background: rgba(255, 255, 255, 0.035);
  border: 1px solid var(--line-1);
  color: var(--text-4); font-size: var(--fs-11);
}
.bgm-stats b { color: var(--text-1); font-size: var(--fs-15); font-variant-numeric: tabular-nums; }
.bgm-stats span.err b { color: #ff9a9a; }
.bgm-shelf-link {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  margin: -4px 0 var(--sp-3);
  padding: 9px 11px;
  border-radius: var(--r-m);
  border: 1px solid rgba(240, 145, 168, 0.22);
  background: rgba(240, 145, 168, 0.07);
  color: #f3b1c1;
  font-size: var(--fs-12);
  font-weight: 650;
  transition: background var(--t-fast), border-color var(--t-fast);
}
.bgm-shelf-link:hover {
  background: rgba(240, 145, 168, 0.12);
  border-color: rgba(240, 145, 168, 0.34);
}
.bgm-shelf-link span { display: inline-flex; align-items: center; gap: 7px; }
.bgm-switch {
  display: flex; align-items: flex-start; gap: 9px;
  padding: 10px 11px; border-radius: var(--r-m);
  background: rgba(255, 255, 255, 0.025);
  color: var(--text-2); font-size: var(--fs-12); line-height: 1.45;
  cursor: pointer;
}
.bgm-switch input { margin-top: 3px; accent-color: var(--brand); }
.bgm-switch small { display: block; margin-top: 2px; color: var(--text-4); font-size: var(--fs-11); }
.bgm-error {
  display: flex; gap: 7px; align-items: flex-start;
  margin-top: 9px; padding: 8px 10px; border-radius: var(--r-m);
  color: #ffb4b4; background: rgba(255, 107, 107, 0.08);
  border: 1px solid rgba(255, 107, 107, 0.22);
  font-size: var(--fs-11); line-height: 1.45;
}
.bgm-error svg { flex: none; margin-top: 1px; }
.bgm-actions { display: flex; flex-wrap: wrap; gap: 7px; margin-top: var(--sp-3); }
.bgm-disconnect { color: #e9a2a2; }
.bgm-intro { margin: 0 0 var(--sp-3); color: var(--text-4); font-size: var(--fs-11); line-height: 1.65; }
.bgm-intro b { color: var(--text-2); }
.bgm-connect { margin-top: var(--sp-3); }
.bgm-connect .hint { display: block; margin-top: 7px; }
.spin { animation: spin 1s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }
.pw { border-top: 1px solid var(--line-1); margin-top: var(--sp-4); padding-top: var(--sp-4); }
.label-row {
  display: flex; align-items: center; gap: 7px;
  font-size: var(--fs-12); font-weight: 600; color: var(--text-3);
  margin-bottom: var(--sp-3);
}
.acct-foot { border-top: 1px solid var(--line-1); margin-top: var(--sp-4); padding-top: var(--sp-3); display: flex; justify-content: flex-end; }
.linkish {
  font-size: var(--fs-12);
  color: var(--brand);
  text-decoration: underline;
  text-underline-offset: 3px;
}
.linkish:disabled { opacity: 0.5; }

@media (max-width: 520px) {
  .bgm-stats { grid-template-columns: 1fr 1fr 1fr; gap: 5px; }
  .bgm-stats span { padding: 7px; }
  .bgm-actions .btn { flex: 1 1 auto; justify-content: center; }
  .bgm-help { font-size: 0; gap: 0; }
  .bgm-help svg { width: 15px; height: 15px; }
}
</style>
