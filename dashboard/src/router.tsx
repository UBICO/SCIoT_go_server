import { redirect, type RouteObject } from 'react-router'
import { api, isUnauthorized } from './api/client'
import { DeviceDetailPage } from './pages/DeviceDetailPage'
import { DeviceListPage } from './pages/DeviceListPage'
import { NotFound, RouteError } from './pages/Errors'
import { Layout } from './pages/Layout'
import { LoginPage } from './pages/LoginPage'

// Le route della dashboard (react-router, "data mode": createBrowserRouter
// in main.tsx). Una pagina nuova: una route qui e una voce in `nav`
// (pages/Layout.tsx).
//
// Gli URL sono veri ("/devices/pi_a"): dopo un reload il browser chiede
// quel percorso al server Go, che risponde con index.html
// (internal/dashboard/embed.go); poi la pagina la sceglie il router.

// requireLogin è il loader dell'area protetta: GET /status prima di
// mostrare qualunque pagina. 401 = nessuna sessione (o scaduta, o server
// riavviato): al login. Lo stato letto è il primo valore dell'header, prima
// che arrivi lo stream.
export async function requireLogin() {
  try {
    return await api.status()
  } catch (err) {
    if (isUnauthorized(err)) {
      throw redirect('/login')
    }
    throw err
  }
}

export const routes: RouteObject[] = [
  { path: '/login', Component: LoginPage },
  {
    path: '/',
    Component: Layout,
    loader: requireLogin,
    ErrorBoundary: RouteError,
    children: [
      { index: true, loader: () => redirect('/devices') },
      { path: 'devices', Component: DeviceListPage },
      { path: 'devices/:id', Component: DeviceDetailPage },
      { path: '*', Component: NotFound },
    ],
  },
]
