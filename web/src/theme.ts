import { Dark } from 'quasar'
import { computed, ref } from 'vue'

export type ThemeMode = 'system' | 'light' | 'dark'

const STORAGE_KEY = 'jhv-theme-mode'
const stored = globalThis.localStorage?.getItem(STORAGE_KEY)

export const themeMode = ref<ThemeMode>(
  stored === 'light' || stored === 'dark' || stored === 'system' ? stored : 'system',
)

export const themeIcon = computed(() => {
  switch (themeMode.value) {
    case 'dark':
      return 'dark_mode'
    case 'light':
      return 'light_mode'
    default:
      return 'brightness_auto'
  }
})

export function setThemeMode(mode: ThemeMode): void {
  themeMode.value = mode
  globalThis.localStorage?.setItem(STORAGE_KEY, mode)
  Dark.set(mode === 'system' ? 'auto' : mode === 'dark')
  document.documentElement.dataset.theme = mode
}

export function initialiseTheme(): void {
  setThemeMode(themeMode.value)
}
