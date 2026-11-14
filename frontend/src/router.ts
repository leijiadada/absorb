import { createRouter, createWebHashHistory } from 'vue-router'

import StatusView from './views/StatusView.vue'
import ModelsView from './views/ModelsView.vue'
import SettingsView from './views/SettingsView.vue'

export const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', redirect: '/status' },
    { path: '/status', component: StatusView },
    { path: '/models', component: ModelsView },
    { path: '/settings', component: SettingsView },
  ],
})
