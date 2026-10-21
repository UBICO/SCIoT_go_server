package record

import (
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/load"
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
	ServiceEWMAAlpha float64 `json:"service_ewma_alpha"`
	WindowS          float64 `json:"window_s"`
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
}

// Policy: il server ha cambiato la policy di un device.
type Policy struct {
	Header
	DeviceID  string     `json:"device_id"`
	SessionID string     `json:"session_id"`
	Policy    api.Policy `json:"policy"`
	Reason    string     `json:"reason"` // "admin"; nella Fase 3 l'algoritmo
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
	Outcome       string  `json:"outcome"` // Outcome*
	// Admission è la previsione del dispatcher: presente se il frame è stato
	// ammesso o rifiutato per la scadenza (non se il worker non c'era).
	Admission *Admission `json:"admission,omitempty"`
	// Timing e Detections solo per OutcomeResult.
	Timing     *api.Timing `json:"timing,omitempty"`
	Detections *int        `json:"detections,omitempty"` // quante; i box non si registrano
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
func (*End) kind() string       { return "end" }
