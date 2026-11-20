// Client-only app: the Go API does all the work, so there is no server rendering.
export default defineNuxtConfig({
  ssr: false,
  compatibilityDate: '2026-11-01',
  devtools: { enabled: false },
  css: ['~/assets/main.css'],
  app: {
    head: {
      title: 'Data Analyst Agent',
      meta: [{ name: 'viewport', content: 'width=device-width, initial-scale=1' }],
    },
  },
  nitro: {
    // `npm run dev` talks to a Go API on localhost:8080.
    devProxy: { '/api': { target: 'http://localhost:8080/api', changeOrigin: true } },
  },
  typescript: { strict: true },
})
