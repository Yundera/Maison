import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// Vite builds the SPA straight into the Go embed directory so `go build`
// picks it up via //go:embed.
export default defineConfig({
  plugins: [svelte()],
  build: {
    outDir: '../internal/ui/dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://localhost:8080',
      '/ping': 'http://localhost:8080',
      // Served by Go, not by Vite (see internal/server/pwa.go). Without the
      // manifest entry the dev server answers the <link rel="manifest"> with
      // index.html and the browser logs a parse failure on every page load.
      '/manifest.webmanifest': 'http://localhost:8080',
      '/sw.js': 'http://localhost:8080',
      '/offline.html': 'http://localhost:8080',
      // /icons is NOT proxied: those are real files under web/public, which Vite
      // serves itself. Proxying them would hand dev the last build's copies.
      '/ws': { target: 'ws://localhost:8080', ws: true },
    },
  },
})
