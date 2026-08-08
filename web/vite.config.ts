import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { quasar, transformAssetUrls } from '@quasar/vite-plugin'

// Quasar is used through its Vite plugin rather than the Quasar CLI: the app is
// served by the Go binary from a plain `dist/`, so an extra build framework
// would only add moving parts.
export default defineConfig({
  plugins: [
    vue({ template: { transformAssetUrls } }),
    quasar({ sassVariables: fileURLToPath(new URL('./src/css/quasar.variables.scss', import.meta.url)) }),
  ],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 9000,
    // In development the SPA runs on its own port and talks to the Go server;
    // the proxy keeps cookies same-origin so sessions work without CORS games.
    proxy: {
      '/api': {
        target: process.env.JHV_API_TARGET ?? 'http://localhost:8080',
        changeOrigin: false,
      },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 1200,
  },
})
