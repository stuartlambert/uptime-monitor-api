import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The UI is served from the same origin as the API in production, so requests
// are plain same-origin paths and no CORS or credentials handling is needed.
// In development, Vite proxies /api to the local daemon to preserve that.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: false,
      },
    },
  },
  build: {
    outDir: 'dist',
    // Vite content-hashes asset filenames, which is what lets the deploy serve
    // /assets/* with a one-year immutable cache while index.html stays no-cache.
    assetsDir: 'assets',
    sourcemap: false,
  },
})
