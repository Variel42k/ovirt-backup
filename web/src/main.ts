import { createApp } from 'vue'
import { createPinia } from 'pinia'
import { Quasar, Notify, Dialog, Loading } from 'quasar'
import quasarLang from 'quasar/lang/ru'

import '@quasar/extras/material-icons/material-icons.css'
import 'quasar/src/css/index.sass'
import './css/app.scss'

import App from './App.vue'
import { router } from './router'

const app = createApp(App)

app.use(createPinia())
app.use(router)
app.use(Quasar, {
  plugins: { Notify, Dialog, Loading },
  lang: quasarLang,
  config: {
    // Крестик задаётся здесь, а не отдельным action: action рисуется второй
    // строкой плашки, и вместе со встроенным крестиком их получалось два —
    // разного вида, в разных местах и с одинаковым действием.
    //
    // Время жизни задаёт notify() из api/client.ts: она же снимает плашку с
    // учёта, и два независимых умолчания разъезжались бы между собой.
    notify: { position: 'top-right', closeBtn: '✕' },
  },
})

app.mount('#app')
