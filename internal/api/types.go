// Package api contiene i tipi del protocollo v1: i messaggi JSON scambiati
// tra client e server, e le regole di validità del contratto (validate.go).
//
// Il nome del package (api) è quello che si usa per riferirsi ai suoi tipi
// dall'esterno: per esempio api.RegisterRequest.
package api

import "encoding/json"

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

// Capabilities descrive cosa sa fare il client (docs/PROTOCOL.md,
// "Capabilities"). Regole:
//   - le dichiara il codice del client, una volta, alla registrazione;
//   - assente = "non lo so fare": il server non chiede mai ciò che non è
//     dichiarato;
//   - il server IGNORA le capability che non conosce (UnmarshalJSON): un
//     client più nuovo del server deve potersi registrare. Nel resto della
//     richiesta un campo sconosciuto resta un errore.
type Capabilities struct {
	// Probe: il client risponde a "probe_request" sullo stream.
	Probe bool `json:"probe"`
}

// UnmarshalJSON decodifica le capability ignorando i nomi sconosciuti.
//
// Serve perché il server decodifica le richieste con DisallowUnknownFields
// (server.decodeJSON): quella regola vale anche per le struct annidate, a
// meno che il tipo non si decodifichi da solo implementando json.Unmarshaler,
// come qui.
func (c *Capabilities) UnmarshalJSON(data []byte) error {
	// plain non ha UnmarshalJSON: niente ricorsione infinita.
	type plain Capabilities
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*c = Capabilities(p)
	return nil
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
//	  "capabilities": {"probe": true}
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

// ModelInfo descrive il modello che il server ha associato al device. Viene
// dal manifest.json della cartella del modello (docs/MODELS.md).
//
//	{"model_key": "yolo_small_320", "num_layers": 8, "last_offloading_layer": 7,
//	 "input": {"shape": [1, 320, 320, 3], "dtype": "float32"},
//	 "postprocess": {"decoder": "yolo_pro", "labels": ["bicycle", "stop sign"],
//	                 "score_threshold": 0.5, "iou_threshold": 0.2, "max_detections": 200}}
type ModelInfo struct {
	ModelKey            string `json:"model_key"`
	NumLayers           int    `json:"num_layers"`
	LastOffloadingLayer int    `json:"last_offloading_layer"`
	// Input è il tensore di input del modello: con forma e tipo il device
	// prepara il frame (e ne conosce i byte).
	Input TensorSpec `json:"input"`
	// Postprocess dice al device come interpretare l'output in LOCAL: deve
	// essere lo stesso del server in SPLIT, per questo arriva da qui.
	Postprocess Postprocess `json:"postprocess"`
}

// TensorSpec è la forma e il tipo di un tensore, batch compreso (sempre 1).
type TensorSpec struct {
	Shape []int  `json:"shape"`
	DType string `json:"dtype"` // "float32", ...
}

// Postprocess sono i parametri della decodifica dell'output in detection.
type Postprocess struct {
	Decoder        string   `json:"decoder"` // "yolo_pro", "fomo"
	Labels         []string `json:"labels"`
	ScoreThreshold float64  `json:"score_threshold"`
	// IoUThreshold è la soglia della NMS; nil per i decoder senza NMS (FOMO).
	IoUThreshold  *float64 `json:"iou_threshold,omitempty"`
	MaxDetections int      `json:"max_detections"`
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
	// MaxInflight è la finestra del device in SPLIT: frame in volo al
	// massimo (Fase 3.7). Assente = il massimo dell'hello (stream.max_inflight);
	// sempre assente in LOCAL.
	MaxInflight *int `json:"max_inflight,omitempty"`
}

// RegisterResponse è il corpo della risposta 200 a /api/v1/register.
//
//	{
//	  "session_id": "b1c2...",
//	  "model": { ...ModelInfo... },
//	  "policy": { ...Policy... },
//	  "heartbeat_interval_ms": 1000,
//	  "data_plane": true
//	}
type RegisterResponse struct {
	SessionID           string    `json:"session_id"`
	Model               ModelInfo `json:"model"`
	Policy              Policy    `json:"policy"`
	HeartbeatIntervalMS int       `json:"heartbeat_interval_ms"`
	// DataPlane: il server ha lo stream (GET /api/v1/stream). Il device lo
	// apre subito dopo la registrazione, anche in LOCAL: ping, policy e probe
	// passano di lì. false = solo control plane, niente stream.
	DataPlane bool `json:"data_plane"`
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
	// ErrCodeDeviceNotFound: l'endpoint admin riceve un device non registrato.
	ErrCodeDeviceNotFound = "device_not_found"
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
// Tempi del device per segmento (opzionali, insieme): per ogni segmento
// 0..last del modello, la media della finestra e quante volte è stato
// eseguito. null / 0 = segmento non eseguito nella finestra. Servono al server
// per stimare il costo per il device di uno split mai provato; media mobile e
// varianza le calcola il server.
//
//	{"inference_count": 47, "inference_time_ms": 21.3,
//	 "inference_time_max_ms": 38.9, "cpu_load": 0.42, "rtt_ms": 3.1,
//	 "memory_available_bytes": 812345344,
//	 "segment_ms": [0.5, 2.7, null], "segment_count": [47, 47, 0]}
type Telemetry struct {
	// InferenceCount non è un puntatore: assente = 0 = "nessuna inferenza".
	// Un client che lo dimentica ma invia la media viene rifiutato da Validate.
	InferenceCount       int      `json:"inference_count"`
	InferenceTimeMS      *float64 `json:"inference_time_ms,omitempty"`
	InferenceTimeMaxMS   *float64 `json:"inference_time_max_ms,omitempty"`
	CPULoad              *float64 `json:"cpu_load,omitempty"` // (0..1)
	RTTMS                *float64 `json:"rtt_ms,omitempty"`
	MemoryAvailableBytes *int64   `json:"memory_available_bytes,omitempty"`
	// SegmentMS[i] è la media del segmento i nella finestra; nil se non
	// eseguito. SegmentCount[i] quante volte. Stessa lunghezza: il numero di
	// segmenti del modello (lo controlla device.Store.Heartbeat).
	SegmentMS    []*float64 `json:"segment_ms,omitempty"`
	SegmentCount []int      `json:"segment_count,omitempty"`
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

// ---------------------------------------------------------------------------
// Modelli (GET /api/v1/models)
// ---------------------------------------------------------------------------

// ModelEntry è un modello del registry insieme al suo hash.
//
// ModelInfo è "embedded" (campo senza nome): i suoi campi vengono promossi,
// quindi si scrive e.ModelKey invece di e.ModelInfo.ModelKey, e anche nel
// JSON compaiono allo stesso livello di model_hash:
//
//	{"model_hash": "2b31...", "model_key": "fomo_96x96", "num_layers": 59, ...}
type ModelEntry struct {
	ModelHash string `json:"model_hash"`
	ModelInfo
}

// ModelsResponse è il corpo di GET /api/v1/models.
type ModelsResponse struct {
	Models []ModelEntry `json:"models"`
}

// ---------------------------------------------------------------------------
// Admin (POST /api/v1/admin/devices/{id}/policy)
// ---------------------------------------------------------------------------

// SetPolicyRequest forza la policy di un device. La versione non si passa:
// la assegna il server (versione corrente + 1).
//
//	{"mode": "SPLIT", "split_point": 5}
//	{"mode": "SPLIT", "split_point": 5, "max_inflight": 2}
//	{"mode": "LOCAL", "split_point": null}
type SetPolicyRequest struct {
	Mode       ExecutionMode `json:"mode"`
	SplitPoint *int          `json:"split_point"`
	// MaxInflight è la finestra in SPLIT (Policy.MaxInflight); assente = il
	// massimo dello stream.
	MaxInflight *int `json:"max_inflight,omitempty"`
}

// SetPolicyResponse restituisce la policy che il device riceverà al
// prossimo heartbeat.
type SetPolicyResponse struct {
	DeviceID  string `json:"device_id"`
	SessionID string `json:"session_id"`
	Policy    Policy `json:"policy"`
}
