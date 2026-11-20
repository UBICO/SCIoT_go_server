import { redirect, type RouteObject } from 'react-router'
import { api, ApiError, isUnauthorized } from './api/client'
import type { ConfigFile } from './api/types'
import { ChangeLogPage } from './pages/ChangeLogPage'
import { ConfigPage } from './pages/ConfigPage'
import { DeviceDetailPage } from './pages/DeviceDetailPage'
import { DeviceListPage } from './pages/DeviceListPage'
import { Loading, NotFound, RouteError } from './pages/Errors'
import { Layout } from './pages/Layout'
import { LoginPage } from './pages/LoginPage'
import { ModelsPage } from './pages/ModelsPage'
import { RecordingsPage } from './pages/RecordingsPage'

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

// configLoader legge config.json per la pagina Config. null = modifica
// dalla dashboard spenta (404).
export async function configLoader(): Promise<ConfigFile | null> {
  try {
    return await api.getConfig()
  } catch (err) {
    if (isUnauthorized(err)) {
      throw redirect('/login')
    }
    if (err instanceof ApiError && err.status === 404) {
      return null
    }
    throw err
  }
}

// withLogin: un loader che va al login su 401 (sessione scaduta mentre si
// naviga), come requireLogin.
function withLogin<T>(load: () => Promise<T>) {
  return async () => {
    try {
      return await load()
    } catch (err) {
      if (isUnauthorized(err)) {
        throw redirect('/login')
      }
      throw err
    }
  }
}

export const routes: RouteObject[] = [
  { path: '/login', Component: LoginPage },
  {
    path: '/',
    Component: Layout,
    loader: requireLogin,
    ErrorBoundary: RouteError,
    HydrateFallback: Loading,
    children: [
      { index: true, loader: () => redirect('/devices') },
      { path: 'devices', Component: DeviceListPage },
      { path: 'devices/:id', Component: DeviceDetailPage },
      { path: 'config', Component: ConfigPage, loader: configLoader },
      { path: 'models', Component: ModelsPage, loader: withLogin(api.models) },
      { path: 'recordings', Component: RecordingsPage, loader: withLogin(api.recordings) },
      { path: 'changelog', Component: ChangeLogPage, loader: withLogin(() => api.changelog()) },
      { path: '*', Component: NotFound },
    ],
  },
]
