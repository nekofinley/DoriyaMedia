<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, ApiError } from '../api.js'
import { me, resetAuth } from '../auth.js'
import AppIcon from '../components/AppIcon.vue'
import { toast } from '../toast.js'

const route = useRoute()
const router = useRouter()

const codeInput = ref('')
const authorization = ref(null)
const csrfToken = ref('')
const loading = ref(false)
const busy = ref(false)
const decisionConflict = ref(false)
const codeError = ref('')
const pageError = ref('')
const now = ref(Date.now())
const codeInputEl = ref(null)
const statusHeading = ref(null)
const resultHeading = ref(null)
const pendingDecision = ref('')

const platformLabels = {
  android: 'Android',
  ios: 'iOS',
  macos: 'macOS',
  windows: 'Windows',
}

let ticker = 0
let mounted = false
let lookupSeq = 0

function queryString(value) {
  if (Array.isArray(value)) value = value[0]
  return typeof value === 'string' ? value : ''
}

// Deliberately looser than the server's canonical XXXXX-XXXXX format. Spaces,
// hyphens and case are normalized here; the server remains the authority.
function normalizeCode(value) {
  return String(value || '').trim().toUpperCase().replace(/[\s-]+/g, '')
}

function codeValidationMessage(code) {
  if (!code) return '请输入设备上显示的授权码。'
  if (!/^[A-Z0-9]{4,64}$/.test(code)) return '授权码应为 4–64 位字母或数字，请核对后重试。'
  return ''
}

const expiryMs = computed(() => {
  const value = authorization.value?.expires_at
  if (!value) return 0
  const parsed = new Date(value).getTime()
  return Number.isFinite(parsed) ? parsed : 0
})

const state = computed(() => {
  if (decisionConflict.value) return 'conflict'
  const raw = String(authorization.value?.state || '').trim().toLowerCase()
  if ((raw === 'pending' || raw === 'authorized') && expiryMs.value && expiryMs.value <= now.value) {
    return 'expired'
  }
  return raw
})

const isPending = computed(() => state.value === 'pending')
const canDecide = computed(() => isPending.value && !loading.value && !busy.value)

const stateBadge = computed(() => {
  switch (state.value) {
    case 'pending': return { label: '等待确认', tone: 'warn' }
    case 'authorized': return { label: '已批准', tone: 'ok' }
    case 'denied': return { label: '已拒绝', tone: 'err' }
    case 'expired': return { label: '已过期', tone: 'err' }
    case 'consumed': return { label: '已完成', tone: 'ok' }
    case 'conflict': return { label: '已处理', tone: 'warn' }
    default: return { label: '状态未知', tone: 'warn' }
  }
})

const outcome = computed(() => {
  switch (state.value) {
    case 'authorized':
      return {
        tone: 'ok',
        icon: 'check',
        title: '此设备已获批准',
        body: '请返回 DoriyaMedia 应用完成登录。这个授权页面现在可以安全关闭。',
      }
    case 'denied':
      return {
        tone: 'err',
        icon: 'close',
        title: '已拒绝此设备',
        body: '请不要关闭应用中的提示；返回应用后可重新发起一次设备授权。这个页面可以安全关闭。',
      }
    case 'expired':
      return {
        tone: 'warn',
        icon: 'clock',
        title: '授权请求已过期',
        body: '为避免批准一条过时的请求，此授权已不能提交。请返回应用重新发起设备登录。',
      }
    case 'consumed':
      return {
        tone: 'ok',
        icon: 'check',
        title: '授权流程已完成',
        body: '应用已经领取了设备凭证。这个页面不再接受重复提交，可以安全关闭。',
      }
    default:
      return {
        tone: 'warn',
        icon: 'alert',
        title: '无法继续此授权',
        body: '这个请求的状态已经变化。请返回应用重新发起授权，或输入另一个授权码。',
      }
  }
})

const deviceName = computed(() => String(authorization.value?.device_name || '').trim() || '未知设备')
const clientVersion = computed(() => String(authorization.value?.client_version || '').trim() || '未提供')
const platform = computed(() => {
  const value = String(authorization.value?.platform || '').trim()
  return platformLabels[value.toLowerCase()] || value || '未知平台'
})
const expiresLabel = computed(() => {
  const ms = expiryMs.value
  if (!ms) return '未提供'
  const date = new Date(ms)
  if (Number.isNaN(date.getTime())) return '未提供'
  return new Intl.DateTimeFormat('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(date)
})
const remainingLabel = computed(() => {
  if (!expiryMs.value) return ''
  const remaining = expiryMs.value - now.value
  if (remaining <= 0) return '已过期'
  const minutes = Math.ceil(remaining / 60000)
  if (minutes < 60) return `约 ${minutes} 分钟后过期`
  const hours = Math.floor(minutes / 60)
  const rest = minutes % 60
  return rest ? `约 ${hours} 小时 ${rest} 分钟后过期` : `约 ${hours} 小时后过期`
})

function pickAuthorization(data, requireID = true) {
  const body = data && typeof data === 'object' ? data : null
  const source = body?.authorization && typeof body.authorization === 'object' ? body.authorization : body
  if (!source) return null

  const safe = {}
  const id = typeof source.authorization_id === 'string' ? source.authorization_id.trim() : ''
  if (requireID && !id) return null
  if (id) safe.authorization_id = id

  for (const key of ['device_name', 'platform', 'client_version', 'state', 'expires_at']) {
    if (typeof source[key] === 'string' && source[key].trim()) safe[key] = source[key].trim()
  }
  return Object.keys(safe).length ? safe : null
}

function pickCSRFToken(data) {
  const token = data && typeof data.csrf_token === 'string' ? data.csrf_token.trim() : ''
  return /^[A-Za-z0-9._~-]{32,512}$/.test(token) ? token : ''
}

function safeError(error, action) {
  if (!(error instanceof ApiError)) {
    return '无法连接服务器，请检查网络后重试。'
  }
  switch (error.code) {
    case 'feature_disabled':
      return '设备授权功能当前未开放，请联系服务器管理员。'
    case 'authorization_denied':
      return '这个设备授权已经被拒绝，请回到应用重新发起。'
    case 'authorization_expired':
      return '这个设备授权已经过期，请回到应用重新发起。'
    case 'authorization_consumed':
      return '这个设备授权已经处理过，请回到应用重新发起。'
    case 'rate_limited':
      return '请求过于频繁，请稍后再试。'
    case 'invalid_request':
    case 'invalid_grant':
      return action === 'lookup'
        ? '授权码未通过校验，请核对设备上显示的代码后重试。'
        : '这次操作没有被服务器接受，请刷新后重试。'
  }
  switch (error.status) {
    case 400:
      return action === 'lookup'
        ? '授权码未通过校验，请核对设备上显示的代码后重试。'
        : '这次操作没有被服务器接受，请刷新后重试。'
    case 403:
      return '当前账号不能进行设备授权，请联系管理员。'
    case 404:
      return action === 'lookup'
        ? '没有找到这个授权请求，请核对设备上显示的授权码。'
        : '这个授权请求已经不存在，请回到应用重新发起。'
    case 409:
    case 410:
      return '这个授权请求已失效或已经处理，不能再提交。'
    case 429:
      return '请求过于频繁，请稍后再试。'
    default:
      return error.status >= 500
        ? '服务暂时不可用，请稍后再试。'
        : '请求没有完成，请稍后再试。'
  }
}

function returnToLogin() {
  resetAuth()
  router.replace({
    path: '/login',
    query: { redirect: route.fullPath },
  }).catch(() => {})
}

function showError(error, action, withToast = false) {
  if (error instanceof ApiError && error.status === 401) {
    returnToLogin()
    return true
  }
  const message = safeError(error, action)
  pageError.value = message
  if (withToast) toast.err(message)
  return false
}

async function focusHeading() {
  await nextTick()
  statusHeading.value?.focus()
}

async function focusResult() {
  await nextTick()
  resultHeading.value?.focus()
}

async function lookup() {
  if (loading.value) return
  const code = normalizeCode(codeInput.value)
  const validation = codeValidationMessage(code)
  codeError.value = validation
  pageError.value = ''
  if (validation) {
    authorization.value = null
    await nextTick()
    codeInputEl.value?.focus()
    return
  }

  const seq = ++lookupSeq
  loading.value = true
  decisionConflict.value = false
  authorization.value = null
  csrfToken.value = ''
  try {
    const data = await api.get(`/api/v1/auth/device/authorizations/lookup?user_code=${encodeURIComponent(code)}`)
    if (seq !== lookupSeq) return
    const safe = pickAuthorization(data)
    if (!safe) {
      pageError.value = '服务器返回的授权信息不完整，请稍后重试。'
      return
    }
    const token = pickCSRFToken(data)
    if (safe.state === 'pending' && !token) {
      pageError.value = '服务器未返回审批校验信息，请稍后重新查找。'
      return
    }
    authorization.value = safe
    csrfToken.value = token
    if (state.value === 'pending') await focusHeading()
    else await focusResult()
  } catch (error) {
    if (seq !== lookupSeq) return
    if (showError(error, 'lookup')) return
    await nextTick()
    codeInputEl.value?.focus()
  } finally {
    if (seq === lookupSeq) loading.value = false
  }
}

async function decide(decision) {
  if (!canDecide.value) return
  const id = authorization.value?.authorization_id
  if (!id) {
    pageError.value = '这个授权请求缺少必要信息，请重新输入授权码。'
    return
  }
  const approvalToken = csrfToken.value
  if (!approvalToken) {
    pageError.value = '审批校验已失效，请重新查找这个授权请求。'
    return
  }

  busy.value = true
  decisionConflict.value = false
  pendingDecision.value = decision
  pageError.value = ''
  try {
    const data = await api.post(
      `/api/v1/auth/device/authorizations/${encodeURIComponent(id)}/decision`,
      { decision },
      { headers: { 'X-CSRF-Token': approvalToken } },
    )
    const safe = pickAuthorization(data, false)
    authorization.value = {
      ...authorization.value,
      ...(safe || {}),
      state: safe?.state || (decision === 'approve' ? 'authorized' : 'denied'),
    }
    csrfToken.value = ''
    toast.ok(decision === 'approve' ? '已批准此设备' : '已拒绝此设备')
    await focusResult()
  } catch (error) {
    if (showError(error, 'decision', true)) return
    if (error instanceof ApiError && (error.status === 403 || error.status === 409 || error.status === 410)) {
      decisionConflict.value = true
      csrfToken.value = ''
      // The decision may have landed in another tab. Re-read only the public
      // authorization metadata so this page cannot offer a duplicate action.
      try {
        const fresh = await api.get(
          `/api/v1/auth/device/authorizations/lookup?user_code=${encodeURIComponent(normalizeCode(codeInput.value))}`,
        )
        const safe = pickAuthorization(fresh)
        const token = pickCSRFToken(fresh)
        if (safe && safe.state === 'pending' && token) {
          authorization.value = safe
          csrfToken.value = token
          decisionConflict.value = false
          pageError.value = '审批校验已更新，请再次确认操作。'
        } else if (safe && safe.state && safe.state !== 'pending') {
          decisionConflict.value = false
          authorization.value = safe
          await focusResult()
        }
      } catch {}
    }
  } finally {
    busy.value = false
    pendingDecision.value = ''
  }
}

function resetCode() {
  lookupSeq++
  codeInput.value = ''
  authorization.value = null
  csrfToken.value = ''
  pageError.value = ''
  codeError.value = ''
  loading.value = false
  busy.value = false
  decisionConflict.value = false
  if (queryString(route.query.code)) router.replace({ path: '/device' }).catch(() => {})
  nextTick(() => codeInputEl.value?.focus())
}

onMounted(async () => {
  mounted = true
  ticker = window.setInterval(() => { now.value = Date.now() }, 30000)
  const code = queryString(route.query.code)
  if (code) {
    codeInput.value = code
    await lookup()
  } else {
    await nextTick()
    codeInputEl.value?.focus()
  }
})

watch(() => route.query.code, async (value) => {
  if (!mounted) return
  const code = queryString(value)
  if (!code || normalizeCode(code) === normalizeCode(codeInput.value)) return
  codeInput.value = code
  await lookup()
})

onBeforeUnmount(() => {
  mounted = false
  window.clearInterval(ticker)
})
</script>

<template>
  <div class="device-page">
    <div class="atmosphere" aria-hidden="true"></div>

    <header class="brand-row">
      <router-link to="/" class="brand" aria-label="DoriyaMedia 首页">
        <span class="logo"><AppIcon name="play" :size="14" filled /></span>
        <span class="wordmark">Doriya<span>Media</span></span>
      </router-link>
      <span v-if="me" class="account-chip">
        <AppIcon name="user" :size="14" />
        <span>{{ me.name }}</span>
      </span>
    </header>

    <main class="stage">
      <section class="auth-card" :aria-busy="loading" aria-labelledby="device-title">
        <div class="card-eyebrow">
          <span class="eyebrow">设备授权</span>
          <span class="eyebrow-line" aria-hidden="true"></span>
        </div>

        <template v-if="!authorization">
          <div class="intro">
            <span class="intro-icon"><AppIcon name="lock" :size="24" /></span>
            <div>
              <h1 id="device-title">连接一台新设备</h1>
              <p>输入 DoriyaMedia 应用显示的授权码。我们会先展示请求设备的信息，再由你决定是否批准。</p>
            </div>
          </div>

          <form class="code-form" @submit.prevent="lookup">
            <div class="field">
              <label for="device-code">授权码</label>
              <div class="code-row">
                <input
                  id="device-code"
                  ref="codeInputEl"
                  v-model="codeInput"
                  type="text"
                  inputmode="text"
                  autocomplete="off"
                  autocapitalize="characters"
                  spellcheck="false"
                  maxlength="96"
                  placeholder="ABCD-EFGH"
                  :disabled="loading"
                  :aria-invalid="!!codeError"
                  aria-describedby="code-hint code-error"
                  @input="codeError = ''; pageError = ''"
                />
                <button class="btn primary lookup-btn" :disabled="loading">
                  <AppIcon v-if="!loading" name="search" :size="16" />
                  <span v-if="loading" class="spinner" aria-hidden="true"></span>
                  {{ loading ? '正在查找…' : '查找请求' }}
                </button>
              </div>
              <span id="code-hint" class="hint">不区分大小写，空格和连字符可以省略。</span>
            </div>

            <Transition name="fade">
              <div v-if="codeError" id="code-error" class="flash err" role="alert">
                <AppIcon name="alert" :size="16" />
                {{ codeError }}
              </div>
            </Transition>

            <Transition name="fade">
              <div v-if="pageError" class="flash err" role="alert">
                <AppIcon name="alert" :size="16" />
                {{ pageError }}
              </div>
            </Transition>
          </form>

          <div class="session-note">
            <AppIcon name="lock" :size="16" />
            <p>你正在使用当前的 Web 登录账号批准请求。网页登录凭证不会被复制给设备。</p>
          </div>
        </template>

        <template v-else>
          <div class="request-head">
            <div class="request-title">
              <span class="intro-icon"><AppIcon name="tv" :size="24" /></span>
              <div>
                <span class="eyebrow">登录请求</span>
                <h1 id="device-title" ref="statusHeading" tabindex="-1">确认此设备</h1>
                <p>批准前，请确认设备信息与你刚刚发起登录的应用一致。</p>
              </div>
            </div>
            <span class="badge" :class="stateBadge.tone">{{ stateBadge.label }}</span>
          </div>

          <div class="device-summary">
            <div class="device-mark" aria-hidden="true">
              <AppIcon name="tv" :size="28" />
              <span class="signal"></span>
            </div>
            <div class="device-copy">
              <span class="field-label">请求设备</span>
              <strong>{{ deviceName }}</strong>
              <span>{{ platform }} · 客户端 {{ clientVersion }}</span>
            </div>
          </div>

          <dl class="facts">
            <div>
              <dt>平台</dt>
              <dd>{{ platform }}</dd>
            </div>
            <div>
              <dt>客户端版本</dt>
              <dd>{{ clientVersion }}</dd>
            </div>
            <div class="wide">
              <dt>授权过期</dt>
              <dd>
                {{ expiresLabel }}
                <span v-if="remainingLabel" class="remaining" :class="{ expired: state === 'expired' }">
                  {{ remainingLabel }}
                </span>
              </dd>
            </div>
          </dl>

          <template v-if="isPending">
            <div class="risk-note" role="note">
              <AppIcon name="alert" :size="19" />
              <div>
                <strong>只在你刚发起这次请求时批准</strong>
                <p>如果你没有在 DoriyaMedia 应用中发起设备登录，请拒绝并检查账号安全。</p>
              </div>
            </div>

            <Transition name="fade">
              <div v-if="pageError" class="flash err" role="alert">
                <AppIcon name="alert" :size="16" />
                {{ pageError }}
              </div>
            </Transition>

            <div class="actions">
              <button class="btn danger reject" :disabled="!canDecide" @click="decide('deny')">
                <span v-if="pendingDecision === 'deny'" class="spinner" aria-hidden="true"></span>
                <AppIcon v-else name="close" :size="17" />
                拒绝
              </button>
              <button class="btn primary approve" :disabled="!canDecide" @click="decide('approve')">
                <span v-if="pendingDecision === 'approve'" class="spinner" aria-hidden="true"></span>
                <AppIcon v-else name="check" :size="17" />
                批准此设备
              </button>
            </div>

            <p class="submission-note">
              <AppIcon name="info" :size="14" />
              提交后不能在此页面重复操作，设备令牌只会交给正在请求的应用。
            </p>
          </template>

          <div v-else class="result" :class="outcome.tone" role="status" aria-live="polite">
            <span class="result-icon"><AppIcon :name="outcome.icon" :size="24" /></span>
            <div>
              <h2 ref="resultHeading" tabindex="-1">{{ outcome.title }}</h2>
              <p>{{ outcome.body }}</p>
            </div>
          </div>

          <div class="card-foot">
            <span><AppIcon name="lock" :size="14" /> Web 登录凭证不会提供给设备</span>
            <button class="btn ghost sm" @click="resetCode">输入其他授权码</button>
          </div>
        </template>
      </section>

      <aside class="guide" aria-label="设备授权安全检查">
        <span class="guide-index">01</span>
        <h2>核对，再批准</h2>
        <p>设备授权只确认账号可以登录，不会在此页面展示设备访问令牌。</p>
        <ol>
          <li>
            <span>设备</span>
            <p>确认名称和平台与你正在使用的应用一致。</p>
          </li>
          <li>
            <span>时机</span>
            <p>只有你刚刚在应用里点过登录，才应该看到这条请求。</p>
          </li>
          <li>
            <span>安全</span>
            <p>任何情况下都不要把网页 Cookie、密码或令牌发给设备。</p>
          </li>
        </ol>
        <div class="guide-seal">
          <AppIcon name="lock" :size="16" />
          <span>由当前 Web 会话确认</span>
        </div>
      </aside>
    </main>

    <footer class="page-foot">
      <span>DoriyaMedia 设备授权</span>
      <router-link to="/">返回媒体库</router-link>
    </footer>
  </div>
</template>

<style scoped>
.device-page {
  position: relative;
  min-height: 100dvh;
  overflow: hidden;
  display: flex;
  flex-direction: column;
  padding: max(24px, env(safe-area-inset-top)) var(--gutter) max(22px, env(safe-area-inset-bottom));
  background:
    radial-gradient(58% 54% at 82% 8%, rgba(240, 169, 46, 0.15), transparent 68%),
    radial-gradient(45% 45% at 8% 18%, rgba(74, 108, 255, 0.11), transparent 66%),
    linear-gradient(155deg, #0b0d12 0%, var(--bg-0) 58%, #090b10 100%);
}

.atmosphere {
  position: absolute;
  inset: 0;
  pointer-events: none;
  background-image:
    linear-gradient(rgba(255, 255, 255, 0.025) 1px, transparent 1px),
    linear-gradient(90deg, rgba(255, 255, 255, 0.025) 1px, transparent 1px);
  background-size: 64px 64px;
  mask-image: radial-gradient(74% 68% at 61% 40%, #000 8%, transparent 80%);
  -webkit-mask-image: radial-gradient(74% 68% at 61% 40%, #000 8%, transparent 80%);
}

.brand-row,
.stage,
.page-foot {
  position: relative;
  z-index: 1;
  width: min(1040px, 100%);
  margin-inline: auto;
}

.brand-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-4);
}

.brand { display: inline-flex; align-items: center; gap: 10px; }
.logo {
  display: grid;
  place-items: center;
  width: 32px;
  height: 32px;
  border-radius: 10px;
  background: linear-gradient(150deg, var(--brand-2), var(--brand) 70%);
  color: var(--brand-ink);
  box-shadow: 0 6px 20px var(--brand-tint), inset 0 1px 0 rgba(255, 255, 255, 0.5);
}
.wordmark {
  font-family: var(--font-display);
  font-size: 18px;
  font-weight: 800;
  letter-spacing: -0.035em;
}
.wordmark span { color: var(--text-3); font-weight: 600; }
.account-chip {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  min-width: 0;
  padding: 7px 11px;
  border: 1px solid var(--line-1);
  border-radius: var(--r-pill);
  background: rgba(255, 255, 255, 0.035);
  color: var(--text-3);
  font-size: var(--fs-12);
}

.stage {
  flex: 1;
  display: grid;
  grid-template-columns: minmax(0, 1fr) 272px;
  gap: clamp(24px, 5vw, 56px);
  align-items: center;
  padding-block: clamp(36px, 7vh, 76px);
}

.auth-card {
  min-width: 0;
  padding: clamp(24px, 4.2vw, 44px);
  border: 1px solid var(--line-2);
  border-radius: var(--r-xl);
  background:
    linear-gradient(145deg, rgba(255, 255, 255, 0.055), rgba(255, 255, 255, 0.012) 46%),
    rgba(11, 13, 18, 0.94);
  box-shadow: 0 38px 90px rgba(0, 0, 0, 0.54), inset 0 1px 0 rgba(255, 255, 255, 0.04);
  backdrop-filter: blur(18px);
  -webkit-backdrop-filter: blur(18px);
}

.card-eyebrow { display: flex; align-items: center; gap: 12px; margin-bottom: clamp(22px, 4vh, 38px); }
.eyebrow-line { flex: 1; height: 1px; background: linear-gradient(90deg, var(--line-2), transparent); }

.intro {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr);
  gap: 17px;
  align-items: start;
}
.intro-icon {
  display: grid;
  place-items: center;
  width: 50px;
  height: 50px;
  flex: none;
  border-radius: 15px;
  color: var(--brand);
  background: linear-gradient(150deg, rgba(240, 169, 46, 0.18), rgba(240, 169, 46, 0.055));
  border: 1px solid rgba(240, 169, 46, 0.24);
  box-shadow: 0 10px 28px rgba(240, 169, 46, 0.1);
}
.intro h1,
.request-title h1 {
  margin: 5px 0 9px;
  font-size: clamp(27px, 3.5vw, 38px);
}
.intro p,
.request-title p {
  max-width: 54ch;
  margin: 0;
  color: var(--text-3);
  line-height: 1.7;
}

.code-form { margin-top: clamp(28px, 4.5vh, 42px); }
.code-form .field { margin-bottom: 0; }
.code-row { display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 10px; }
.code-row input {
  font-family: var(--font-mono);
  font-size: var(--fs-16);
  font-weight: 650;
  letter-spacing: 0.1em;
  text-transform: uppercase;
}
.lookup-btn { min-width: 132px; height: 42px; }
.code-form .flash { margin: 14px 0 0; }

.session-note {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  margin-top: clamp(26px, 4vh, 38px);
  padding-top: 20px;
  border-top: 1px solid var(--line-1);
  color: var(--text-4);
}
.session-note svg { color: var(--ok); margin-top: 2px; }
.session-note p { margin: 0; font-size: var(--fs-12); line-height: 1.65; }

.request-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--sp-4);
}
.request-title {
  display: flex;
  align-items: flex-start;
  gap: 16px;
  min-width: 0;
}
.request-head .badge { flex: none; margin-top: 4px; padding-inline: 10px; }
.request-title h1:focus-visible { outline-offset: 5px; }

.device-summary {
  display: flex;
  align-items: center;
  gap: 16px;
  margin-top: 26px;
  padding: 17px;
  border: 1px solid var(--line-2);
  border-radius: var(--r-m);
  background:
    radial-gradient(100% 160% at 10% 0%, rgba(240, 169, 46, 0.08), transparent 70%),
    rgba(255, 255, 255, 0.025);
}
.device-mark {
  position: relative;
  display: grid;
  place-items: center;
  width: 58px;
  height: 58px;
  flex: none;
  border-radius: var(--r-m);
  color: var(--brand);
  background: rgba(240, 169, 46, 0.09);
  border: 1px solid rgba(240, 169, 46, 0.22);
}
.signal {
  position: absolute;
  right: 8px;
  bottom: 8px;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--ok);
  box-shadow: 0 0 0 3px rgba(74, 222, 128, 0.12);
}
.device-copy { min-width: 0; display: flex; flex-direction: column; gap: 3px; }
.device-copy strong {
  overflow-wrap: anywhere;
  font-family: var(--font-display);
  font-size: clamp(19px, 2.2vw, 24px);
  letter-spacing: -0.02em;
}
.device-copy > span:last-child { color: var(--text-3); font-size: var(--fs-12); }
.field-label {
  color: var(--text-4);
  font-size: var(--fs-11);
  font-weight: 700;
  letter-spacing: 0.1em;
  text-transform: uppercase;
}

.facts {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 0;
  margin: 18px 0 0;
  padding: 0;
  border: 1px solid var(--line-1);
  border-radius: var(--r-m);
  background: rgba(0, 0, 0, 0.16);
}
.facts > div { min-width: 0; padding: 13px 15px; }
.facts > div + div { border-left: 1px solid var(--line-1); }
.facts > .wide {
  grid-column: 1 / -1;
  border-top: 1px solid var(--line-1);
  border-left: 0;
}
.facts dt {
  color: var(--text-4);
  font-size: var(--fs-11);
  font-weight: 650;
  letter-spacing: 0.04em;
}
.facts dd {
  margin: 3px 0 0;
  color: var(--text-2);
  font-size: var(--fs-13);
  overflow-wrap: anywhere;
}
.remaining { margin-left: 8px; color: var(--warn); }
.remaining.expired { color: var(--danger); }

.risk-note {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  margin-top: 18px;
  padding: 14px 16px;
  border: 1px solid rgba(251, 191, 36, 0.24);
  border-radius: var(--r-m);
  background: linear-gradient(120deg, var(--warn-soft), rgba(251, 191, 36, 0.035));
  color: #fcd34d;
}
.risk-note svg { margin-top: 2px; }
.risk-note strong { display: block; font-size: var(--fs-13); }
.risk-note p { margin: 3px 0 0; color: rgba(253, 230, 138, 0.72); font-size: var(--fs-12); line-height: 1.6; }

.actions {
  display: grid;
  grid-template-columns: minmax(0, 0.72fr) minmax(0, 1.28fr);
  gap: 10px;
  margin-top: 20px;
}
.actions .btn { height: 48px; border-radius: var(--r-m); font-size: var(--fs-15); }
.actions .approve { box-shadow: 0 12px 30px rgba(240, 169, 46, 0.16); }
.actions .spinner { border-color: currentColor; border-right-color: transparent; }
.submission-note {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 7px;
  margin: 12px 0 0;
  color: var(--text-4);
  font-size: var(--fs-11);
  text-align: center;
}

.result {
  display: flex;
  gap: 14px;
  margin-top: 20px;
  padding: 18px;
  border-radius: var(--r-m);
  border: 1px solid var(--line-2);
  background: var(--bg-2);
}
.result.ok { border-color: rgba(74, 222, 128, 0.28); background: var(--ok-soft); }
.result.err { border-color: rgba(255, 107, 107, 0.3); background: var(--danger-soft); }
.result.warn { border-color: rgba(251, 191, 36, 0.28); background: var(--warn-soft); }
.result-icon {
  display: grid;
  place-items: center;
  width: 42px;
  height: 42px;
  flex: none;
  border-radius: 50%;
  background: rgba(255, 255, 255, 0.07);
}
.result.ok .result-icon { color: var(--ok); }
.result.err .result-icon { color: var(--danger); }
.result.warn .result-icon { color: var(--warn); }
.result h2 { margin: 1px 0 5px; font-size: var(--fs-18); }
.result p { margin: 0; color: var(--text-3); font-size: var(--fs-13); line-height: 1.68; }

.card-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-3);
  margin-top: 20px;
  padding-top: 16px;
  border-top: 1px solid var(--line-1);
}
.card-foot > span {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  color: var(--text-4);
  font-size: var(--fs-11);
}

.guide { padding-block: 6px; }
.guide-index {
  display: block;
  margin-bottom: 16px;
  color: var(--brand);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  font-weight: 700;
  letter-spacing: 0.16em;
}
.guide h2 { margin-bottom: 9px; font-size: var(--fs-24); }
.guide > p { color: var(--text-3); font-size: var(--fs-13); line-height: 1.7; }
.guide ol {
  display: flex;
  flex-direction: column;
  gap: 18px;
  margin: 28px 0 0;
  padding: 0;
  list-style: none;
  border-left: 1px solid var(--line-2);
}
.guide li { position: relative; padding-left: 18px; }
.guide li::before {
  content: "";
  position: absolute;
  left: -3px;
  top: 5px;
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: var(--brand);
  box-shadow: 0 0 0 4px var(--bg-0);
}
.guide li > span {
  display: block;
  color: var(--text-2);
  font-size: var(--fs-12);
  font-weight: 700;
}
.guide li p { margin: 4px 0 0; color: var(--text-4); font-size: var(--fs-12); line-height: 1.62; }
.guide-seal {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  margin-top: 28px;
  padding: 9px 12px;
  border: 1px solid var(--line-1);
  border-radius: var(--r-pill);
  color: var(--text-3);
  font-size: var(--fs-11);
  background: rgba(255, 255, 255, 0.025);
}
.guide-seal svg { color: var(--ok); }

.page-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-4);
  padding-top: 20px;
  border-top: 1px solid var(--line-1);
  color: var(--text-4);
  font-size: var(--fs-11);
}
.page-foot a { color: var(--text-3); transition: color var(--t-fast); }
.page-foot a:hover { color: var(--text-1); }

.spinner {
  width: 16px;
  height: 16px;
  border: 2px solid currentColor;
  border-right-color: transparent;
  border-radius: 50%;
  animation: spin 0.8s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }

@media (max-width: 900px) {
  .stage { grid-template-columns: minmax(0, 1fr); padding-block: clamp(28px, 6vh, 52px); }
  .guide { display: none; }
}

@media (max-width: 620px) {
  .device-page { padding-inline: 16px; }
  .account-chip { max-width: 44%; }
  .account-chip :deep(span) { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .stage { padding-block: 24px; }
  .auth-card { padding: 22px 18px; border-radius: var(--r-l); }
  .card-eyebrow { margin-bottom: 20px; }
  .intro { grid-template-columns: 1fr; gap: 14px; }
  .intro-icon { width: 44px; height: 44px; border-radius: 13px; }
  .intro h1, .request-title h1 { font-size: 27px; }
  .code-row { grid-template-columns: 1fr; }
  .lookup-btn { width: 100%; }
  .request-head { flex-direction: column-reverse; gap: 14px; }
  .request-title { gap: 12px; }
  .request-head .badge { align-self: flex-start; margin: 0; }
  .facts { grid-template-columns: 1fr; }
  .facts > div + div { border-top: 1px solid var(--line-1); border-left: 0; }
  .facts > .wide { grid-column: auto; }
  .actions { grid-template-columns: 1fr 1.35fr; }
  .actions .btn { padding-inline: 10px; }
  .card-foot { align-items: flex-start; flex-direction: column; }
  .card-foot .btn { width: 100%; }
  .page-foot { align-items: flex-start; flex-direction: column; gap: 8px; }
}

@media (max-width: 380px) {
  .actions { grid-template-columns: 1fr; }
  .actions .approve { grid-row: 1; }
  .account-chip { display: none; }
}
</style>
