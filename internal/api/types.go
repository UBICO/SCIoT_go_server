// Package api contiene i tipi del protocollo v1: i messaggi JSON scambiati
// tra client e server. Sono solo dati, senza logica.
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
