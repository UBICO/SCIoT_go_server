package record

import (
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/ewma"
	"github.com/UBICO/SCIoT_go_server/internal/load"
	"github.com/UBICO/SCIoT_go_server/internal/netmeasure"
)

// Event è un evento da registrare. Si passa sempre un puntatore
// (&Heartbeat{...}): Record ne imposta l'intestazione.
type Event interface {
	kind() string
	header() *Header
}

// Header è comune a tutte le righe. Incorporato (embedded) in ogni evento:
// encoding/json scrive i suoi campi allo stesso livello degli altri.
type Header struct {
	Kind string    `json:"kind"`
	TS   time.Time `json:"ts"`   // ora UTC, per correlare con log e device
	TMS  float64   `json:"t_ms"` // ms dall'avvio, su orologio monotono: per le durate
}

func (h *Header) header() *Header { return h }

// Start è la prima riga: il file si descrive da solo.
type Start struct {
	Header
	Schema              string           `json:"schema"`
	HeartbeatIntervalMS int              `json:"heartbeat_interval_ms"`
	Dispatch            *DispatchConfig  `json:"dispatch,omitempty"` // nil senza data plane
	Models              []api.ModelEntry `json:"models"`
}

// DispatchConfig sono i parametri del dispatcher in uso.
type DispatchConfig struct {
	ServiceTauMS float64 `json:"service_tau_ms"`
	WindowS      float64 `json:"window_s"`
}

// Session: un device si è registrato (nuova sessione).
type Session struct {
	Header
	DeviceID     string           `json:"device_id"`
	SessionID    string           `json:"session_id"`
	DeviceType   string           `json:"device_type"`
	ModelKey     string           `json:"model_key"`
	Capabilities api.Capabilities `json:"capabilities"`
}

// Heartbeat: lo stato che una decisione vedrebbe in quell'istante.
type Heartbeat struct {
	Header
	DeviceID        string        `json:"device_id"`
	SessionID       string        `json:"session_id"`
	Applied         Applied       `json:"applied"`       // la policy che il device dichiara di applicare
	ServerPolicy    api.Policy    `json:"server_policy"` // quella del server dopo l'heartbeat
	Telemetry       api.Telemetry `json:"telemetry"`
	SinceLastSeenMS float64       `json:"since_last_seen_ms"`
	ServerLoad      load.Snapshot `json:"server_load"`
	Queue           *Queue        `json:"queue,omitempty"` // nil senza data plane
	// Network è la rete del device vista dal server; nil senza stream.
	Network *netmeasure.State `json:"network,omitempty"`
	// DeviceSegments è la stima del server dei tempi del device per segmento
	// (ms) dopo questo heartbeat; null per un segmento mai eseguito. Assente
	// se il device non invia segment_ms.
	DeviceSegments []*ewma.Estimate `json:"device_segments,omitempty"`
	// CycleMS è la stima del server di A, il resto del ciclo del device oltre
	// al calcolo (device.Cycle), dopo questo heartbeat; assente senza una
	// finestra utile, e nelle registrazioni precedenti alla Fase 3.9 (il
	// replay la ricalcola dalla telemetria).
	CycleMS *float64 `json:"cycle_ms,omitempty"`
	// WaitForWindow: il device ha dichiarato la capability wait_for_window
	// (la sua regola della finestra piena). Assente nelle registrazioni
	// precedenti alla Fase 3.9, i cui client non aspettavano mai.
	WaitForWindow bool `json:"wait_for_window,omitempty"`

	// Il resto dello stato che un algoritmo vede per questo device, così il
	// replay (cmd/sciot-replay algo) gli dà esattamente gli stessi ingressi:
	// il modello (segmenti 0..num_segments−1), S del server per split point
	// di quel modello, i byte che il device invia a ogni split point.
	ModelKey    string            `json:"model_key"`
	NumSegments int               `json:"num_segments"`
	Service     []SplitService    `json:"service,omitempty"`     // nil senza data plane
	InputBytes  []api.SegmentSize `json:"input_bytes,omitempty"` // nil senza data plane
	// MaxInflight è la finestra del device (frame in volo al massimo); 0
	// senza data plane.
	MaxInflight int `json:"max_inflight,omitempty"`
}

// SplitService è S stimato dal dispatcher per uno split point. Source è
// "measured" (frame di quello split) o "segments" (somma dei segmenti più
// overhead): StdMS e Samples solo per "measured".
type SplitService struct {
	SplitPoint int     `json:"split_point"`
	MeanMS     float64 `json:"mean_ms"`
	StdMS      float64 `json:"std_ms"`
	Samples    uint64  `json:"samples"`
	Source     string  `json:"source"`
}

// Applied è la policy che il device dichiara nell'heartbeat.
type Applied struct {
	Mode          api.ExecutionMode `json:"mode"`
	PolicyVersion uint64            `json:"policy_version"`
}

// Queue è lo stato della coda del dispatcher: la parte di
// dispatch.Snapshot che serve a una decisione, senza le liste per segmento
// (sono nella riga start e cambiano lentamente).
type Queue struct {
	Slots           int      `json:"slots"`
	Running         int      `json:"running"`
	Queued          int      `json:"queued"`
	OfferedPerSec   float64  `json:"offered_per_s"`
	AdmittedPerSec  float64  `json:"admitted_per_s"`
	Utilization     *float64 `json:"utilization,omitempty"`
	PredictedWaitMS float64  `json:"predicted_wait_ms"`
	// Intervalli tra frame offerti: media e deviazione (dispatch.Snapshot).
	InterarrivalMS    *float64 `json:"interarrival_ms,omitempty"`
	InterarrivalStdMS *float64 `json:"interarrival_std_ms,omitempty"`
	// Il carico del device dell'heartbeat, sulla stessa finestra:
	// frame ammessi al secondo ed erlang (dispatch.DeviceLoad). Assenti nelle
	// registrazioni precedenti alla Fase 3.8.
	DeviceAdmittedPerSec *float64 `json:"device_admitted_per_s,omitempty"`
	DeviceErlangs        *float64 `json:"device_erlangs,omitempty"`
}

// Policy: il server ha cambiato la policy di un device.
type Policy struct {
	Header
	DeviceID  string     `json:"device_id"`
	SessionID string     `json:"session_id"`
	Policy    api.Policy `json:"policy"`
	Reason    string     `json:"reason"` // "admin"; nella Fase 3 l'algoritmo
}

// Decision: un algoritmo di offloading ha deciso la policy di un device a
// un suo heartbeat (dal vivo dalla Fase 3.6; nel replay con sciot-replay
// algo -out). Registrata anche quando la policy non cambia: le previsioni
// spiegano perché.
type Decision struct {
	Header
	DeviceID  string     `json:"device_id"`
	Algorithm string     `json:"algorithm"`
	Current   api.Policy `json:"current"` // la policy prima della decisione
	// Mode, SplitPoint e MaxInflight (la finestra) della policy scelta.
	Mode        api.ExecutionMode `json:"mode"`
	SplitPoint  *int              `json:"split_point"`
	MaxInflight *int              `json:"max_inflight,omitempty"`
	Changed     bool              `json:"changed"`
	Reason      string            `json:"reason"`
	// PendingPerSec e PendingErlangs sono il carico prenotato da altri
	// device e non ancora nella misura, aggiunto agli altri prima di
	// prevedere (docs/OFFLOAD.md, "Reservations").
	PendingPerSec  float64 `json:"pending_per_s,omitempty"`
	PendingErlangs float64 `json:"pending_erlangs,omitempty"`
	// Clients: quanti altri device sono stati simulati come client (Fase
	// 3.8); 0 = gli altri come flusso al ritmo misurato (ripiego).
	Clients int `json:"clients,omitempty"`
	// LagMS: dall'arrivo dell'heartbeat alla decisione (la goroutine del
	// server, Fase 3.8); assente nel replay, che decide subito.
	LagMS       float64      `json:"lag_ms,omitempty"`
	Predictions []Prediction `json:"predictions,omitempty"`
}

// Prediction è una previsione del modello di costo (internal/costmodel) per
// uno split point. I valori infiniti (coda satura) sono null: il JSON non li
// rappresenta, Saturated lo dice.
type Prediction struct {
	SplitPoint int      `json:"split_point"`
	Window     int      `json:"window,omitempty"` // la finestra; assente per LOCAL
	Local      bool     `json:"local"`
	TotalMS    *float64 `json:"total_ms"`
	DeviceMS   float64  `json:"device_ms"`
	UploadMS   float64  `json:"upload_ms"`
	WaitMS     *float64 `json:"wait_ms"`
	ServiceMS  float64  `json:"service_ms"`
	SuffixMS   float64  `json:"suffix_ms"`
	ServerMS   *float64 `json:"server_ms"`
	LocalMS    float64  `json:"local_ms"`
	// Frame al secondo del device e al server, quota al server e cosa la
	// limita (costmodel.Prediction).
	FramesPerSec       float64  `json:"frames_per_s"`
	ServerFramesPerSec float64  `json:"server_frames_per_s"`
	ServerShare        float64  `json:"server_share"`
	Bound              string   `json:"bound,omitempty"`
	P95MS              float64  `json:"p95_ms,omitempty"`
	RejectedPerSec     float64  `json:"rejected_per_s,omitempty"`
	WindowFullPerSec   float64  `json:"window_full_per_s,omitempty"`
	Rho                *float64 `json:"rho"`
	Saturated          bool     `json:"saturated"`
	Missing            []string `json:"missing,omitempty"`
}

// Esiti di un frame.
const (
	OutcomeResult            = "result"
	OutcomeServerBusy        = api.ErrCodeServerBusy
	OutcomeDeadlineExceeded  = api.ErrCodeDeadlineExceeded
	OutcomeWorkerUnavailable = api.ErrCodeWorkerUnavailable
)

// Frame: l'esito di un frame SPLIT, ammesso o no.
type Frame struct {
	Header
	DeviceID      string  `json:"device_id"`
	SessionID     string  `json:"session_id"`
	SequenceID    uint64  `json:"sequence_id"`
	PolicyVersion uint64  `json:"policy_version"`
	SplitPoint    int     `json:"split_point"`
	RequestBytes  int     `json:"request_bytes"`
	DeadlineMS    float64 `json:"deadline_ms"`
	// Ricezione del frame (netmeasure.Sample): anche per i frame rifiutati,
	// la rete li ha comunque trasportati. ReceiveValid dice se il campione è
	// entrato nella stima della banda.
	ReceiveMS          float64 `json:"receive_ms"`
	ReceiveWaitedShare float64 `json:"receive_waited_share"`
	ReceiveValid       bool    `json:"receive_valid"`
	Outcome            string  `json:"outcome"` // Outcome*
	// Admission è la previsione del dispatcher: presente se il frame è stato
	// ammesso o rifiutato per la scadenza (non se il worker non c'era).
	Admission *Admission `json:"admission,omitempty"`
	// Timing e Detections solo per OutcomeResult.
	Timing     *api.Timing `json:"timing,omitempty"`
	Detections *int        `json:"detections,omitempty"` // quante; i box non si registrano
}

// Probe: il device ha risposto a un probe_request (misura della banda senza
// frame, internal/stream). Stessi campi di ricezione di Frame.
type Probe struct {
	Header
	DeviceID           string  `json:"device_id"`
	SessionID          string  `json:"session_id"`
	ProbeID            uint64  `json:"probe_id"`
	Bytes              int     `json:"bytes"`
	ReceiveMS          float64 `json:"receive_ms"`
	ReceiveWaitedShare float64 `json:"receive_waited_share"`
	ReceiveValid       bool    `json:"receive_valid"`
}

// Profile: il server ha profilato un modello quando il worker è diventato
// pronto (internal/dispatch, profile.go). Error non vuoto = profilo
// interrotto; le esecuzioni già misurate (Runs) sono comunque nelle stime.
type Profile struct {
	Header
	ModelKey   string    `json:"model_key"`
	Runs       int       `json:"runs"`
	SegmentMS  []float64 `json:"segment_ms"`
	OverheadMS float64   `json:"overhead_ms"`
	DurationMS float64   `json:"duration_ms"`
	Error      string    `json:"error,omitempty"`
}

// Admission è ciò che il dispatcher ha previsto per il frame.
type Admission struct {
	PredictedWaitMS float64 `json:"predicted_wait_ms"`
	ServiceMS       float64 `json:"service_ms"` // S usata
	Known           bool    `json:"known"`      // S da misure (false = stima prudente)
}

// End è l'ultima riga, scritta da Close.
type End struct {
	Header
	Written uint64 `json:"written"` // righe scritte prima di questa
	Dropped uint64 `json:"dropped"` // eventi scartati con il buffer pieno
}

func (*Start) kind() string     { return "start" }
func (*Session) kind() string   { return "session" }
func (*Heartbeat) kind() string { return "heartbeat" }
func (*Policy) kind() string    { return "policy" }
func (*Frame) kind() string     { return "frame" }
func (*Probe) kind() string     { return "probe" }
func (*Decision) kind() string  { return "decision" }
func (*Profile) kind() string   { return "profile" }
func (*End) kind() string       { return "end" }
