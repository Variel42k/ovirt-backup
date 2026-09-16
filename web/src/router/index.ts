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
      { path: '', name: 'dashboard', component: () => import('@/pages/DashboardPage.vue'), meta: { perm: 'monitoring.read' } },
      { path: 'servers', name: 'servers', component: () => import('@/pages/ServersPage.vue'), meta: { perm: 'servers.read' } },
      {
        path: 'servers/:serverId',
        name: 'server',
        component: () => import('@/pages/ServerDetailPage.vue'),
        props: true,
        meta: { perm: 'servers.read' },
      },
      {
        path: 'servers/:serverId/vms/:vmId',
        name: 'vm',
        component: () => import('@/pages/VMDetailPage.vue'),
        props: true,
        meta: { perm: 'servers.read' },
      },
      { path: 'jobs', name: 'jobs', component: () => import('@/pages/JobsPage.vue'), meta: { perm: 'jobs.read' } },
      { path: 'backups', name: 'backups', component: () => import('@/pages/BackupsPage.vue'), meta: { perm: 'backups.read' } },
      { path: 'engine-config', name: 'engine-config', component: () => import('@/pages/EngineConfigPage.vue'), meta: { perm: 'engine_config.read' } },
			{ path: 'file-backups', name: 'file-backups', component: () => import('@/pages/FileBackupsPage.vue'), meta: { perm: 'file_backups.read' } },
      { path: 'coverage', name: 'coverage', component: () => import('@/pages/CoveragePage.vue'), meta: { perm: 'monitoring.read' } },
      { path: 'retention', name: 'retention', component: () => import('@/pages/RetentionPage.vue'), meta: { perm: 'backups.read' } },
      { path: 'storages', name: 'storages', component: () => import('@/pages/StoragesPage.vue'), meta: { perm: 'storages.read' } },
      { path: 'alerts', name: 'alerts', component: () => import('@/pages/AlertsPage.vue'), meta: { perm: 'alerts.read' } },
      { path: 'documentation', name: 'documentation', component: () => import('@/pages/DocumentationPage.vue') },
      { path: 'settings', name: 'settings', component: () => import('@/pages/SettingsPage.vue'), meta: { settingsTab: 'system' } },
      {
        path: 'administration/identity', name: 'identity-settings',
        component: () => import('@/pages/IdentityPage.vue'), meta: { perm: 'users.admin' },
      },
      {
        path: 'administration/users', name: 'users-access',
        component: () => import('@/pages/UsersAccessPage.vue'), meta: { perm: 'users.admin' },
      },
      {
        path: 'administration/access', name: 'access-settings',
        component: () => import('@/pages/SettingsPage.vue'), meta: { settingsTab: 'roles', perm: 'users.admin' },
      },
      {
        path: 'operations/approvals', name: 'approvals',
        component: () => import('@/pages/SettingsPage.vue'), meta: { settingsTab: 'approvals' },
      },
    ],
  },
  { path: '/:catchAll(.*)*', redirect: '/' },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

const landingRoutes = [
  { name: 'dashboard', perm: 'monitoring.read' },
  { name: 'servers', perm: 'servers.read' },
  { name: 'jobs', perm: 'jobs.read' },
  { name: 'backups', perm: 'backups.read' },
  { name: 'file-backups', perm: 'file_backups.read' },
  { name: 'storages', perm: 'storages.read' },
  { name: 'alerts', perm: 'alerts.read' },
  { name: 'settings', perm: '' },
]

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
  const requiredPermission = String(to.meta.perm ?? '')
  if (requiredPermission && !auth.can(requiredPermission)) {
    return { name: landingRoutes.find((candidate) => !candidate.perm || auth.can(candidate.perm))?.name ?? 'settings' }
  }
  return true
})
