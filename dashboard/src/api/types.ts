// Le forme JSON della porta admin (docs/ADMIN_API.md). La fonte di verità
// sono le struct Go: internal/server/admin.go (device, decisioni),
// internal/api/types.go (policy, telemetria), internal/record/events.go
// (decisione, previsioni), internal/netmeasure (rete), internal/load e
// internal/dispatch (stato del server). Un campo cambiato lì si cambia qui
// nella stessa PR (CLAUDE.md).
//
// Un campo `?` è omesso dal server quando manca (omitempty); `| null` è un
// null esplicito nel JSON. Si tipizzano solo i campi che la dashboard usa o
// che servono a capire il dato.

export type Mode = 'LOCAL' | 'SPLIT'

export interface Policy {
  mode: Mode
  split_point: number | null // null in LOCAL; 0 è uno split valido
  policy_version: number
  max_inflight?: number // la finestra in SPLIT
}

// La telemetria dell'ultimo heartbeat, come la manda il device, aggregata
// sulla sua finestra di heartbeat (PROTOCOL.md).
export interface Telemetry {
  inference_count: number
  inference_time_ms?: number
  inference_time_max_ms?: number
  cpu_load?: number // 0..1
  rtt_ms?: number
  memory_available_bytes?: number
  segment_ms?: (number | null)[]
  segment_count?: number[]
  wait_ms?: number
  answer_ms?: number
}

// La rete misurata dal server sullo stream del device (RECORDING.md#network).
export interface Network {
  bandwidth_bps?: number // byte al secondo
  bandwidth_std_bps?: number
  bandwidth_age_ms?: number
  latency_ms?: number
  not_limiting: boolean
  valid_samples: number
  discarded_samples: number
}

// Una previsione del modello di costo per uno split point (COSTMODEL.md).
export interface Prediction {
  split_point: number
  window?: number
  local: boolean
  total_ms: number | null // null = coda satura
  device_ms: number
  upload_ms: number
  wait_ms: number | null
  service_ms: number
  suffix_ms: number
  server_ms: number | null
  local_ms: number
  frames_per_s: number
  server_frames_per_s: number
  server_share: number
  bound?: string
  p95_ms?: number
  rho: number | null
  saturated: boolean
  missing?: string[]
}

// Una decisione dell'algoritmo: la stessa riga "decision" della
// registrazione (RECORDING.md). ts e t_ms li imposta il recorder: senza
// registrazione sono zero, l'ora giusta è AdminDecision.at.
export interface Decision {
  kind: string
  ts: string
  t_ms: number
  device_id: string
  algorithm: string
  current: Policy // la policy prima della decisione
  mode: Mode
  split_point: number | null
  max_inflight?: number
  changed: boolean
  reason: string
  clients?: number
  lag_ms?: number
  predictions?: Prediction[]
}

export interface AdminDecision {
  at: string // ora del server, RFC 3339
  decision: Decision
}

export interface AdminDevice {
  device_id: string
  device_type: string
  session_id: string
  model_key: string
  policy: Policy
  pinned: boolean
  // Limiti di un pin: split da -1 a last_offloading_layer - 1 (il modello),
  // finestra da 1 a max_inflight_max (lo stream; null senza data plane).
  last_offloading_layer: number
  max_inflight_max: number | null
  policy_since: string
  applied_policy_version: number // 0 = nessun heartbeat ancora
  registered_at: string
  last_seen: string
  telemetry: Telemetry | null
  network: Network | null
  last_decision: AdminDecision | null
}

export interface LoadSnapshot {
  cpu_percent?: number
  cpu_count?: number
  load_avg_1_min?: number
  memory_total_bytes?: number
  memory_available_bytes?: number
  goroutines: number
  sampled_at?: string
}

export interface DispatchSnapshot {
  slots: number
  running: number
  queued: number
  window_s: number
  offered_per_s: number
  admitted_per_s: number
  utilization?: number
  predicted_wait_ms: number
}

// GET /api/v1/admin/status, uguale a GET /api/v1/server/status
// (PROTOCOL.md).
export interface ServerStatus {
  load: LoadSnapshot
  devices: number
  models: number
  started_at: string
  uptime_seconds: number
  dispatch?: DispatchSnapshot // assente senza data plane
  record?: Record<string, unknown> // assente senza registrazione
}

// Il corpo di un pin (POST …/devices/{id}/policy, PROTOCOL.md).
export interface SetPolicyRequest {
  mode: Mode
  split_point: number | null
  max_inflight?: number // assente = il massimo dello stream
}

// La risposta di pin e unpin.
export interface SetPolicyResponse {
  device_id: string
  session_id: string
  policy: Policy
}

// GET /api/v1/admin/config: il config.json del server.
export interface ConfigFile {
  path: string
  content: string
  hash: string // sha256: il base_hash da rimandare nel PUT
  modified_since_start: boolean // il server gira con un'altra versione
}

// La risposta 200 del PUT.
export interface ConfigSaveResponse {
  changed: boolean
  hash: string
  backup?: string // nome del backup, nella cartella del file
  restart_required: boolean
}

// GET /api/v1/admin/recordings: i file di record.dir, dal più recente.
export interface Recording {
  name: string // sciot-YYYYMMDD-HHMMSS.jsonl
  size_bytes: number
  modified: string
  current: boolean // in scrittura (questo avvio)
}

export interface RecordingsResponse {
  recording: boolean // false: nessuna sezione "record" nella config
  dir?: string
  files: Recording[]
}

// GET /api/v1/admin/models: i modelli del registry.
export interface AdminModel {
  model_hash: string
  model_key: string
  num_layers: number
  last_offloading_layer: number
  input: { shape: number[]; dtype: string }
  postprocess: {
    decoder: string
    labels: string[]
    score_threshold: number
    iou_threshold?: number
    max_detections: number
  }
  devices: number // device registrati con questo modello
}

// Una riga del registro delle modifiche (ADMIN_API.md#change-log).
export interface ChangeLogEntry {
  ts: string
  who: string
  action: string // pin, unpin, config, restart
  device_id?: string
  session_id?: string
  policy?: Policy
  pinned?: boolean
  config?: { path: string; backup?: string; hash: string }
}

export interface ChangeLogResponse {
  enabled: boolean // false: nessuna sezione "record"
  entries: ChangeLogEntry[] // dalla più recente
}

// Build di un modello (sciot-model build) dalla dashboard.
export type BuildState = 'running' | 'succeeded' | 'failed' | 'cancelled'

export interface BuildView {
  id: string
  model_key: string
  output: string // models_dir/<key>: il tflite_dir per config.json
  who: string
  state: BuildState
  started_at: string
  finished_at?: string
  exit_code?: number
  model_hash?: string
  log_lines: number
  log?: string[] // solo GET di un build: le righe da since
  next?: number // il since della richiesta successiva
}

export interface BuildsResponse {
  enabled: boolean // false: nessun models_dir in config.json
  models_dir?: string
  builds: BuildView[] // dal più recente
}

export interface LoginResponse {
  name: string
  expires_at: string
}

// Il corpo di ogni errore JSON (PROTOCOL.md#conventions).
export interface ErrorBody {
  error: { code: string; message: string }
}
