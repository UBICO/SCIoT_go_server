import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

// docs/DASHBOARD.md spiega il flusso: npm run dev in sviluppo, npm run build
// per il binario.
export default defineConfig({
  plugins: [react()],
  build: {
    // Il server Go include questa cartella con go:embed
    // (internal/dashboard/embed.go). emptyOutDir va detto esplicitamente:
    // la cartella è fuori da dashboard/. Il segnaposto che git tiene lì
    // viene da public/ e la build lo riscrive.
    outDir: '../internal/dashboard/dist',
    emptyOutDir: true,
    // Un solo bundle da ~530 kB (170 kB gzip, quasi tutto Mantine e React):
    // si serve dal binario sulla LAN, l'avviso dei 500 kB qui non conta.
    chunkSizeWarningLimit: 1000,
  },
  server: {
    // In sviluppo il browser parla con Vite (5173); le chiamate all'API
    // passano al server Go avviato con -admin-addr 127.0.0.1:8081, o
    // all'indirizzo in SCIOT_ADMIN_URL (un altro server già sulla 8081). Per
    // il browser è la stessa origine: il cookie e CrossOriginProtection
    // funzionano come in produzione.
    proxy: { '/api': process.env.SCIOT_ADMIN_URL ?? 'http://127.0.0.1:8081' },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
  },
})
