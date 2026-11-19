import '@mantine/core/styles.css'
import { MantineProvider } from '@mantine/core'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { createBrowserRouter } from 'react-router'
import { RouterProvider } from 'react-router/dom'
import { routes } from './router'

// Il router si crea una volta, fuori da React (lo chiedono i docs di
// react-router per i "data router").
const router = createBrowserRouter(routes)

// defaultColorScheme "auto": chiaro o scuro come il sistema.
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <MantineProvider defaultColorScheme="auto">
      <RouterProvider router={router} />
    </MantineProvider>
  </StrictMode>,
)
