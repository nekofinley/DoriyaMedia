import { createRouter, createWebHistory } from 'vue-router'
import { loadMe, resetAuth } from './auth.js'
import { toast } from './toast.js'
import { site } from './site/store.js'

const routes = [
  { path: '/login', component: () => import('./views/Login.vue'), meta: { public: true, chrome: false } },
  // The links in outgoing mail land here. Public and bare: someone following a
  // reset link has no session, and does not need the site chrome around a form.
  { path: '/verify-email', component: () => import('./views/VerifyEmail.vue'), meta: { public: true, chrome: false, title: '验证邮箱' } },
  { path: '/reset-password', component: () => import('./views/ResetPassword.vue'), meta: { public: true, chrome: false, title: '重设密码' } },
  // The native client opens this in the system browser. It stays behind the
  // normal Web session guard; the guard's redirect query brings the user back
  // to the full /device?code=... URL after signing in.
  { path: '/device', component: () => import('./views/DeviceAuthorize.vue'), meta: { chrome: false, title: '设备授权' } },
  { path: '/', component: () => import('./views/Home.vue'), meta: { title: '首页', overlay: true } },
  { path: '/bangumi', component: () => import('./views/Bangumi.vue'), meta: { title: '追番' } },
  { path: '/requests', component: () => import('./views/Requests.vue'), meta: { title: '求片' } },
  { path: '/requests/manage', component: () => import('./views/RequestManage.vue'), meta: { title: '求片管理', admin: true } },
  { path: '/library/:id', component: () => import('./views/Library.vue'), meta: { title: '媒体库', overlay: true } },
  { path: '/item/:id', component: () => import('./views/Detail.vue'), meta: { title: '详情', overlay: true } },
  { path: '/season/:id', component: () => import('./views/Season.vue'), meta: { title: '剧集', overlay: true } },
  // A catalogue hit being imported: its own live loading page, then the show.
  { path: '/import/:sourceId/:seasonId', component: () => import('./views/Import.vue'), meta: { title: '入库' } },
  { path: '/browse/:id?', component: () => import('./views/Browse.vue'), meta: { title: '文件' } },
  { path: '/play/:fileId', component: () => import('./views/Player.vue'), meta: { fullscreen: true, title: '播放' } },
  // admin: the settings backend is the admin's alone. The API already answers
  // 403 to everyone else; this keeps a viewer from landing on a page of failed
  // requests in the first place.
  { path: '/settings', component: () => import('./views/Settings.vue'), meta: { title: '设置', admin: true } },
  { path: '/:pathMatch(.*)*', redirect: '/' },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior(to, from, saved) {
    if (saved) return saved
    if (to.path === from.path) return {}
    return { top: 0 }
  },
})

router.beforeEach(async (to) => {
  if (to.meta.public) return true
  const user = await loadMe()
  window.__doriyamediaAuth = resetAuth
  if (!user) return { path: '/login', query: { redirect: to.fullPath } }
  if (to.meta.admin && user.role !== 'admin') {
    toast.err('需要管理员权限')
    return { path: '/' }
  }
  return true
})

router.afterEach((to) => {
  // The site name is editable from the toolbar, so the tab title follows it.
  const base = site.doc.site.title || 'DoriyaMedia'
  document.title = to.meta.title ? `${to.meta.title} · ${base}` : base
})
