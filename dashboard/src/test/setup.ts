import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// Senza i globals di vitest, Testing Library non pulisce da sé il DOM tra
// un test e l'altro.
afterEach(cleanup)

// jsdom non ha matchMedia né ResizeObserver; Mantine li usa (tema
// automatico, useElementSize). Bastano versioni che non fanno nulla.
Object.defineProperty(window, 'matchMedia', {
  writable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  }),
})

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
window.ResizeObserver = ResizeObserverStub
