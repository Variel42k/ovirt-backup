import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

const routes = [
  {
    path: '/login',
    name: 'login',
    component: () => import('@/pages/LoginPage.vue'),
    meta: { public: true },
  },
  {
    path: '/',
    component: () => import('@/layouts/MainLayout.vue'),
    children: [
      { path: '', name: 'dashboard', component: () => import('@/pages/DashboardPage.vue') },
      { path: 'servers', name: 'servers', component: () => import('@/pages/ServersPage.vue') },
      {
        path: 'servers/:serverId',
        name: 'server',
        component: () => import('@/pages/ServerDetailPage.vue'),
        props: true,
      },
      {
        path: 'servers/:serverId/vms/:vmId',
        name: 'vm',
        component: () => import('@/pages/VMDetailPage.vue'),
        props: true,
      },
      { path: 'jobs', name: 'jobs', component: () => import('@/pages/JobsPage.vue') },
      { path: 'backups', name: 'backups', component: () => import('@/pages/BackupsPage.vue') },
      { path: 'coverage', name: 'coverage', component: () => import('@/pages/CoveragePage.vue') },
      { path: 'retention', name: 'retention', component: () => import('@/pages/RetentionPage.vue') },
      { path: 'storages', name: 'storages', component: () => import('@/pages/StoragesPage.vue') },
      { path: 'alerts', name: 'alerts', component: () => import('@/pages/AlertsPage.vue') },
      { path: 'documentation', name: 'documentation', component: () => import('@/pages/DocumentationPage.vue') },
      { path: 'settings', name: 'settings', component: () => import('@/pages/SettingsPage.vue') },
    ],
  },
  { path: '/:catchAll(.*)*', redirect: '/' },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()

  // The session is server-side, so the only way to know whether we are logged
  // in is to ask — once, on the first navigation.
  if (!auth.checked) {
    await auth.check()
  }
  if (to.meta.public) {
    return auth.authenticated && to.name === 'login' ? { name: 'dashboard' } : true
  }
  if (!auth.authenticated) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  return true
})
