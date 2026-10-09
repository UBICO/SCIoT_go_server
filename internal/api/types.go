// Package api contiene i tipi del protocollo v1: i messaggi JSON scambiati
// tra client e server, e le regole di validità del contratto (validate.go).
//
// Il nome del package (api) è quello che si usa per riferirsi ai suoi tipi
// dall'esterno: per esempio api.RegisterRequest.
package api

// ---------------------------------------------------------------------------
// Modalità di esecuzione
// ---------------------------------------------------------------------------

// ExecutionMode è un "tipo nominato" basato su string.
// Go non ha enum: si definisce un tipo e un gruppo di costanti di quel tipo.
// Vantaggio rispetto a una string semplice: una funzione che accetta un
// ExecutionMode documenta cosa si aspetta, e il compilatore impedisce di
// passarle per errore una variabile string qualsiasi.
type ExecutionMode string

const (
	ModeLocal ExecutionMode = "LOCAL"
	ModeSplit ExecutionMode = "SPLIT"
)

// ---------------------------------------------------------------------------
// Registrazione: richiesta (client → server)
// ---------------------------------------------------------------------------

// Capabilities descrive cosa sa fare il client.
// Esempio completo: guardalo come modello per le struct qui sotto.
type Capabilities struct {
	RawInputSplit bool `json:"raw_input_split"`
}

// RegisterRequest è il corpo di POST /api/v1/register.
//
// JSON atteso:
//
//	{
//	  "protocol_version": 1,
//	  "device_id": "web_192",
//	  "device_type": "python",
//	  "model_hash": "3f2a...",
//	  "capabilities": {"raw_input_split": true}
//	}
type RegisterRequest struct {
	ProtocolVersion int          `json:"protocol_version"`
	DeviceID        string       `json:"device_id"`
	DeviceType      string       `json:"device_type"`
	ModelHash       string       `json:"model_hash"`
	Capabilities    Capabilities `json:"capabilities"`
}

// ---------------------------------------------------------------------------
// Registrazione: risposta (server → client)
// ---------------------------------------------------------------------------

// ModelInfo descrive il modello che il server ha associato al device.
//
//	{"model_key": "fomo_96x96", "num_layers": 59,
//	 "last_offloading_layer": 58, "input_size_bytes": 18432}
type ModelInfo struct {
	ModelKey            string `json:"model_key"`
	NumLayers           int    `json:"num_layers"`
	LastOffloadingLayer int    `json:"last_offloading_layer"`
	InputSizeBytes      int    `json:"input_size_bytes"`
}

// Policy è la configurazione di esecuzione che il client deve applicare.
//
//	LOCAL: {"mode": "LOCAL", "split_point": null, "policy_version": 1}
//	SPLIT: {"mode": "SPLIT", "split_point": 5,    "policy_version": 2}
type Policy struct {
	Mode ExecutionMode `json:"mode"`
	// SplitPoint è *int perché deve poter valere null (nil) in LOCAL:
	// 0 è uno split point valido. Niente omitempty: il contratto vuole
	// "split_point": null esplicito.
	SplitPoint    *int   `json:"split_point"`
	PolicyVersion uint64 `json:"policy_version"`
}

// RegisterResponse è il corpo della risposta 200 a /api/v1/register.
//
//	{
//	  "session_id": "b1c2...",
//	  "model": { ...ModelInfo... },
//	  "policy": { ...Policy... },
//	  "heartbeat_interval_ms": 1000
//	}
type RegisterResponse struct {
	SessionID           string    `json:"session_id"`
	Model               ModelInfo `json:"model"`
	Policy              Policy    `json:"policy"`
	HeartbeatIntervalMS int       `json:"heartbeat_interval_ms"`
}

// ---------------------------------------------------------------------------
// Errori
// ---------------------------------------------------------------------------

// Codici di errore del protocollo. Sono costanti string non tipizzate:
// qui non serve un tipo dedicato.
const (
	ErrCodeInvalidRequest      = "invalid_request"
	ErrCodeUnsupportedProtocol = "unsupported_protocol"
	ErrCodeModelNotFound       = "model_not_found"
	// ErrCodeSessionExpired: il server non riconosce (più) il session_id.
	// Il client torna in LOCAL e ripete la registrazione.
	ErrCodeSessionExpired = "session_expired"
)

// ErrorResponse è il corpo di ogni risposta di errore:
//
//	{"error": {"code": "model_not_found", "message": "..."}}
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail è il contenuto di "error".
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// Heartbeat (client → server, 1 Hz)
// ---------------------------------------------------------------------------

// Telemetry contiene le metriche del device per l'intervallo trascorso
// dall'heartbeat precedente (la "finestra").
//
// Le metriche di inferenza sono AGGREGATE sulla finestra:
//   - inference_count: numero di inferenze completate nella finestra.
//     0 è un'informazione utile: il loop di inferenza è fermo.
//   - inference_time_ms / inference_time_max_ms: media e massimo della
//     finestra. Presenti solo se inference_count > 0: con zero inferenze la
//     media non esiste, e 0 vorrebbe dire "inferenze istantanee".
//
// Le altre metriche sono opzionali, perché non tutti i device le hanno (es.
// l'ESP32 non ha un "cpu_load" come Linux). Un campo opzionale si modella con
// un puntatore: nil = "non inviato", che è diverso da 0 ("inviato, vale zero").
//
//	{"inference_count": 47, "inference_time_ms": 21.3,
//	 "inference_time_max_ms": 38.9, "cpu_load": 0.42, "rtt_ms": 3.1,
//	 "memory_available_bytes": 812345344}
type Telemetry struct {
	// InferenceCount non è un puntatore: assente = 0 = "nessuna inferenza".
	// Un client che lo dimentica ma invia la media viene rifiutato da Validate.
	InferenceCount       int      `json:"inference_count"`
	InferenceTimeMS      *float64 `json:"inference_time_ms,omitempty"`
	InferenceTimeMaxMS   *float64 `json:"inference_time_max_ms,omitempty"`
	CPULoad              *float64 `json:"cpu_load,omitempty"` // (0..1)
	RTTMS                *float64 `json:"rtt_ms,omitempty"`
	MemoryAvailableBytes *int64   `json:"memory_available_bytes,omitempty"`
}

// HeartbeatRequest è il corpo di POST /api/v1/heartbeat.
//
//	{
//	  "device_id": "web_96",
//	  "session_id": "YVOF...",
//	  "policy_version": 1,
//	  "mode": "LOCAL",
//	  "telemetry": { ...Telemetry... }
//	}
//
// policy_version e mode sono la policy che il client sta APPLICANDO:
// è la "conferma" che permette al server di sapere se il client è allineato.
type HeartbeatRequest struct {
	DeviceID      string        `json:"device_id"`
	SessionID     string        `json:"session_id"`
	PolicyVersion uint64        `json:"policy_version"`
	Mode          ExecutionMode `json:"mode"`
	Telemetry     Telemetry     `json:"telemetry"`
}

// HeartbeatResponse è il corpo della risposta 200: sempre la policy corrente
// (opzione A).
//
//	{"policy": {"mode": "LOCAL", "split_point": null, "policy_version": 1}}
type HeartbeatResponse struct {
	Policy Policy `json:"policy"`
}
