import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Dev server proxies /api to the local Go service; the build output is
// embedded into the Go binary (web/dist).
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:8765'
    }
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // Avoid repo-wide ignore rules that sometimes match "assets", so the
    // embedded dist stays complete.
    assetsDir: 'static'
  }
})
