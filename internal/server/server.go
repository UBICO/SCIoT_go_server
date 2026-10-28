// Package server espone il protocollo v1 via HTTP.
//
// Collega i pezzi: decodifica le richieste (tipi di internal/api), consulta
// il Registry dei modelli e aggiorna lo Store dei device.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/device"
	"github.com/UBICO/SCIoT_go_server/internal/dispatch"
	"github.com/UBICO/SCIoT_go_server/internal/load"
	"github.com/UBICO/SCIoT_go_server/internal/netmeasure"
	"github.com/UBICO/SCIoT_go_server/internal/record"
	"github.com/UBICO/SCIoT_go_server/internal/registry"
	"github.com/UBICO/SCIoT_go_server/internal/stream"
)

// supportedProtocolVersion è l'unica versione del protocollo accettata.
const supportedProtocolVersion = 1

// Config contiene i parametri del server.
type Config struct {
	// HeartbeatInterval viene comunicato al client in fase di registrazione.
	// time.Duration è un int64 di nanosecondi con metodi comodi:
	// time.Second, d.Milliseconds(), ...
	HeartbeatInterval time.Duration

	// MaxBodyBytes è la dimensione massima accettata per il corpo di una
	// richiesta di controllo. Protegge da client che inviano corpi enormi.
	MaxBodyBytes int64

	// Verbose stampa una riga di tabella per ogni heartbeat accettato
	// (vedi table.go). Disattivo di default: a 1 Hz × N device è molto output.
	Verbose bool

	// Admin abilita POST /api/v1/admin/devices/{id}/policy. L'endpoint non
	// ha autenticazione: è pensato per esperimenti in laboratorio, quindi è
	// disattivo di default e la route non viene nemmeno registrata.
	Admin bool
}

// DefaultConfig restituisce i valori consigliati per la Fase 1.
func DefaultConfig() Config {
	return Config{
		HeartbeatInterval: time.Second,
		MaxBodyBytes:      64 << 10, // 64 KiB (<< è lo shift: 64 * 1024)
	}
}

// Server contiene le dipendenze degli handler.
//
// È "dependency injection" senza framework: invece di variabili globali,
// le dipendenze sono campi della struct e gli handler sono metodi. Nei test
// si costruisce un Server con un Registry e uno Store di prova.
// LoadSource fornisce l'ultimo campione del carico del server.
//
// *load.Monitor la soddisfa. Il Server dipende solo da questa interfaccia,
// non dal Monitor: nei test basta una struct che restituisce uno snapshot
// fisso, senza goroutine né letture dal sistema operativo.
// Convenzione Go: l'interfaccia la definisce chi la USA, non chi la implementa.
type LoadSource interface {
	Snapshot() load.Snapshot
}

// noLoad è il LoadSource usato quando non ne viene passato uno: nessuna metrica.
type noLoad struct{}

func (noLoad) Snapshot() load.Snapshot { return load.Snapshot{} }

type Server struct {
	registry  *registry.Registry
	devices   *device.Store
	load      LoadSource
	startedAt time.Time
	cfg       Config

	// Data plane (opzionale, vedi SetStream).
	stream   http.Handler
	notifier StreamNotifier
	queue    QueueSource // nil senza data plane

	rec *record.Recorder // nil = registrazione spenta

	table *heartbeatTable  // usata solo se cfg.Verbose
	now   func() time.Time // orario delle righe; sostituibile nei test
}

// New crea un Server.
// src può essere nil: lo stato del server riporterà allora un carico vuoto.
func New(reg *registry.Registry, devices *device.Store, cfg Config, src LoadSource) *Server {
	if src == nil {
		src = noLoad{}
	}
	return &Server{
		registry:  reg,
		devices:   devices,
		load:      src,
		startedAt: time.Now(),
		cfg:       cfg,
		table:     newHeartbeatTable(os.Stdout),
		now:       time.Now,
	}
}

// Routes restituisce il router con tutti gli endpoint del protocollo.
//
// http.Handler è un'interfaccia con un solo metodo, ServeHTTP. *http.ServeMux
// la implementa, quindi possiamo restituirlo come http.Handler: chi chiama
// sa solo che "gestisce richieste HTTP", non come è fatto dentro.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	// s.handleRegister è un "method value": il metodo legato a questo s.
	// Ha esattamente la firma che HandleFunc si aspetta.
	mux.HandleFunc("POST /api/v1/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /api/v1/models", s.handleModels)
	// {hash} è un "wildcard": il valore si legge con r.PathValue("hash").
	mux.HandleFunc("GET /api/v1/models/{hash}", s.handleModel)
	mux.HandleFunc("GET /api/v1/server/status", s.handleServerStatus)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	if s.stream != nil {
		mux.Handle("GET /api/v1/stream", s.stream)
	}
	if s.cfg.Admin {
		mux.HandleFunc("POST /api/v1/admin/devices/{id}/policy", s.handleSetPolicy)
	}
	return mux
}

// ---------------------------------------------------------------------------
// GET /healthz
// ---------------------------------------------------------------------------

// healthResponse è il corpo di GET /healthz. Non fa parte del protocollo
// client↔server (è per chi monitora il processo), quindi resta qui e non
// in internal/api.
type healthResponse struct {
	Status string `json:"status"`
}

// handleHealthz risponde {"status":"ok"}.
//
// Prima era una funzione in main.go; ora è un metodo di Server, come gli
// altri handler, così tutte le route sono definite in Routes().
// Il parametro r non serve: si può chiamare _ per dirlo esplicitamente.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// ---------------------------------------------------------------------------
// Helper di risposta
// ---------------------------------------------------------------------------

// writeJSON scrive v come JSON con lo status indicato.
//
// any è un alias di interface{}: un valore di qualunque tipo. json.Encoder
// accetta any e usa i tag della struct concreta che gli passi.
func writeJSON(w http.ResponseWriter, status int, v any) {
	// stesso schema di handleHealthz in main.go, ma con lo status
	//   ricevuto come parametro invece di http.StatusOK.
	//   Ordine: header → WriteHeader(status) → Encode.
	//   Messaggio di log con contesto, es. "write json response: %v".

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json response: %v", err)
	}
}

// writeError scrive una risposta di errore nel formato del protocollo:
//
//	{"error": {"code": "...", "message": "..."}}
func writeError(w http.ResponseWriter, status int, code, message string) {
	// una riga: writeJSON con un api.ErrorResponse.
	//   Costruisci il letterale annidato: api.ErrorResponse{Error: api.ErrorDetail{...}}
	writeJSON(w, status, api.ErrorResponse{
		Error: api.ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}

// ---------------------------------------------------------------------------
// Decodifica comune
// ---------------------------------------------------------------------------

// decodeJSON legge il corpo della richiesta in dst, che deve essere un
// puntatore (es. &req). Applica il limite di dimensione e rifiuta le chiavi
// sconosciute.
//
// Restituisce false se la decodifica è fallita: in quel caso ha GIÀ scritto
// la risposta 400, e chi chiama deve solo fare return:
//
//	var req api.HeartbeatRequest
//	if !s.decodeJSON(w, r, &req) {
//		return
//	}
//
// È un metodo di Server (e non una funzione) perché gli serve s.cfg.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	//var req api.RegisterRequest

	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, "invalid JSON body")
		return false
	}

	return true
}

// ---------------------------------------------------------------------------
// POST /api/v1/register
// ---------------------------------------------------------------------------

// handleRegister registra un device.
//
// Esiti:
//
//	200 → api.RegisterResponse
//	400 invalid_request      → JSON malformato, chiavi sconosciute, corpo
//	                           troppo grande, device_id o model_hash vuoti
//	400 unsupported_protocol → protocol_version diverso da 1
//	404 model_not_found      → model_hash non presente nel Registry
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req api.RegisterRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	// writeError non interrompe l'handler: ogni errore va seguito da return,
	// altrimenti si scriverebbe una seconda risposta.
	if req.ProtocolVersion != supportedProtocolVersion {
		writeError(w, http.StatusBadRequest, api.ErrCodeUnsupportedProtocol,
			fmt.Sprintf("unsupported protocol version: expected %d, got %d",
				supportedProtocolVersion, req.ProtocolVersion))
		return
	}
	if req.DeviceID == "" {
		writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, "device_id is required")
		return
	}
	if req.ModelHash == "" {
		writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, "model_hash is required")
		return
	}

	model, ok := s.registry.Lookup(req.ModelHash)
	if !ok {
		writeError(w, http.StatusNotFound, api.ErrCodeModelNotFound,
			fmt.Sprintf("unknown model_hash %q", req.ModelHash))
		return
	}

	d := s.devices.Register(req.DeviceID, req.DeviceType, model, req.Capabilities)
	// La registrazione crea una sessione nuova: lo stream ancora aperto della
	// sessione precedente va chiuso. notifier è nil senza data plane.
	if s.notifier != nil {
		s.notifier.EndSession(req.DeviceID)
	}
	log.Printf("registered device=%s type=%s model=%s session=%s",
		req.DeviceID, d.Type, d.Model.ModelKey, d.SessionID)
	s.rec.Record(&record.Session{DeviceID: d.ID, SessionID: d.SessionID, DeviceType: d.Type,
		ModelKey: d.Model.ModelKey, Capabilities: d.Capabilities})
	writeJSON(w, http.StatusOK, api.RegisterResponse{
		SessionID: d.SessionID,
		Model:     d.Model,
		Policy:    d.Policy,
		// Milliseconds() restituisce int64: le conversioni numeriche in Go
		// sono sempre esplicite.
		HeartbeatIntervalMS: int(s.cfg.HeartbeatInterval.Milliseconds()),
		DataPlane:           s.stream != nil,
	})
}

// ---------------------------------------------------------------------------
// POST /api/v1/heartbeat
// ---------------------------------------------------------------------------

// handleHeartbeat registra la telemetria del device e restituisce la policy
// corrente (sempre, opzione A).
//
// Esiti:
//
//	200 → api.HeartbeatResponse
//	400 invalid_request  → JSON malformato, device_id o session_id vuoti,
//	                       mode non valido, telemetria fuori range
//	409 session_expired  → device sconosciuto o session_id non corrente:
//	                       il client torna in LOCAL e si registra di nuovo
//
// Nessun log per gli heartbeat riusciti: a 1 Hz × N device sarebbe rumore.
// Si registrano solo gli eventi (sessione scaduta). Con cfg.Verbose ogni
// heartbeat accettato produce una riga di tabella (table.go).
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	// Conta i byte del corpo (colonna "HB req (B)"). Va avvolto PRIMA di
	// decodeJSON, che a sua volta avvolge r.Body con MaxBytesReader.
	body := &countingReader{r: r.Body}
	r.Body = body

	var req api.HeartbeatRequest

	if !s.decodeJSON(w, r, &req) {
		return
	}

	if req.DeviceID == "" {
		writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, "device_id is required")
		return
	}

	if req.SessionID == "" {
		writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, "session_id is required")
		return
	}

	if !req.Mode.Valid() {
		writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, fmt.Sprintf("invalid mode %q", req.Mode))
		return
	}

	if err := req.Telemetry.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, fmt.Sprintf("invalid telemetry: %v", err))
		return
	}

	res, err := s.devices.Heartbeat(req.DeviceID, req.SessionID, req.PolicyVersion, req.Telemetry)

	if errors.Is(err, device.ErrSessionExpired) {
		log.Printf("session expired: device=%s session=%s", req.DeviceID, req.SessionID)
		writeError(w, http.StatusConflict, api.ErrCodeSessionExpired, "session expired")
		return
	}

	if errors.Is(err, device.ErrInvalidTelemetry) {
		writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, err.Error())
		return
	}

	if err != nil {
		log.Printf("heartbeat error: device=%s: %v", req.DeviceID, err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	serverLoad := s.load.Snapshot()
	network := s.network(req.DeviceID)
	s.rec.Record(&record.Heartbeat{
		DeviceID:        req.DeviceID,
		SessionID:       req.SessionID,
		Applied:         record.Applied{Mode: req.Mode, PolicyVersion: req.PolicyVersion},
		ServerPolicy:    res.Policy,
		Telemetry:       req.Telemetry,
		SinceLastSeenMS: float64(res.SinceLastSeen.Microseconds()) / 1000,
		ServerLoad:      serverLoad,
		Queue:           s.queueState(),
		Network:         network,
	})

	if s.cfg.Verbose {
		s.table.print(heartbeatRow{
			at:             s.now(),
			deviceID:       req.DeviceID,
			appliedMode:    req.Mode,
			appliedVersion: req.PolicyVersion,
			serverVersion:  res.Policy.PolicyVersion,
			telemetry:      req.Telemetry,
			sinceLastSeen:  res.SinceLastSeen,
			requestBytes:   body.n,
			serverLoad:     serverLoad,
			network:        network,
		})
	}

	writeJSON(w, http.StatusOK, api.HeartbeatResponse{Policy: res.Policy})
}

// ---------------------------------------------------------------------------
// GET /api/v1/models, GET /api/v1/models/{hash}
// ---------------------------------------------------------------------------

// handleModels elenca i modelli serviti, ordinati per model_key.
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, api.ModelsResponse{Models: s.registry.Models()})
}

// handleModel restituisce un modello dato il suo hash, o 404 model_not_found.
func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	info, ok := s.registry.Lookup(hash)
	if !ok {
		writeError(w, http.StatusNotFound, api.ErrCodeModelNotFound,
			fmt.Sprintf("unknown model_hash %q", hash))
		return
	}
	writeJSON(w, http.StatusOK, api.ModelEntry{ModelHash: hash, ModelInfo: info})
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/devices/{id}/policy
// ---------------------------------------------------------------------------

// handleSetPolicy forza la policy di un device (solo con cfg.Admin).
//
// Esiti:
//
//	200 → api.SetPolicyResponse
//	400 invalid_request  → JSON malformato o policy non valida per il modello
//	404 device_not_found → device non registrato
func (s *Server) handleSetPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req api.SetPolicyRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	d, err := s.devices.SetPolicy(id, req)
	switch {
	case errors.Is(err, device.ErrDeviceNotFound):
		writeError(w, http.StatusNotFound, api.ErrCodeDeviceNotFound,
			fmt.Sprintf("device %q is not registered", id))
		return
	case errors.Is(err, device.ErrInvalidPolicy):
		writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, err.Error())
		return
	case err != nil:
		log.Printf("set policy error: device=%s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	log.Printf("admin policy: device=%s session=%s policy=%s v%d",
		id, d.SessionID, d.Policy.Mode, d.Policy.PolicyVersion)
	// Via veloce: la policy arriva subito sullo stream, se aperto. L'heartbeat
	// resta la via affidabile. notifier è nil senza data plane.
	if s.notifier != nil {
		s.notifier.NotifyPolicy(id, d.Policy)
	}
	s.rec.Record(&record.Policy{DeviceID: id, SessionID: d.SessionID, Policy: d.Policy, Reason: "admin"})
	writeJSON(w, http.StatusOK, api.SetPolicyResponse{
		DeviceID:  id,
		SessionID: d.SessionID,
		Policy:    d.Policy,
	})
}

// ---------------------------------------------------------------------------
// GET /api/v1/server/status
// ---------------------------------------------------------------------------

// statusResponse è lo stato del server. Come /healthz serve a chi monitora il
// processo (e agli esperimenti), non ai client: per questo il tipo è qui e
// non in internal/api.
type statusResponse struct {
	Load          load.Snapshot `json:"load"`
	Devices       int           `json:"devices"`
	Models        int           `json:"models"`
	StartedAt     time.Time     `json:"started_at"`
	UptimeSeconds float64       `json:"uptime_seconds"`
	// Dispatch è lo stato della coda del data plane; assente se è disabilitato.
	Dispatch *dispatch.Snapshot `json:"dispatch,omitempty"`
	// Record sono i contatori della registrazione; assente se è spenta.
	Record *record.Stats `json:"record,omitempty"`
}

// handleServerStatus restituisce l'ultimo campione di carico e i contatori.
func (s *Server) handleServerStatus(w http.ResponseWriter, _ *http.Request) {
	// una sola writeJSON con statusResponse:
	//   - Load: lo snapshot di s.load
	//   - Devices / Models: Len() di Store e Registry
	//   - StartedAt: s.startedAt
	//   - UptimeSeconds: tempo trascorso da s.startedAt, misurato con s.now()
	//     (così nei test è deterministico). Una Duration ha .Seconds().
	resp := statusResponse{
		Load:          s.load.Snapshot(),
		Devices:       s.devices.Len(),
		Models:        s.registry.Len(),
		StartedAt:     s.startedAt,
		UptimeSeconds: s.now().Sub(s.startedAt).Seconds(),
	}
	if s.queue != nil {
		snap := s.queue.Snapshot()
		resp.Dispatch = &snap
	}
	if s.rec != nil {
		stats := s.rec.Stats()
		resp.Record = &stats
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Data plane
// ---------------------------------------------------------------------------

// StreamNotifier è ciò che il control plane deve poter dire agli stream
// aperti. *stream.Handler lo soddisfa.
type StreamNotifier interface {
	NotifyPolicy(deviceID string, p api.Policy)
	EndSession(deviceID string)
	// Network è la rete del device misurata sullo stream; false se il device
	// non ha mai aperto uno stream.
	Network(deviceID string) (netmeasure.State, bool)
}

// network restituisce la rete del device, nil senza data plane o senza stream.
func (s *Server) network(deviceID string) *netmeasure.State {
	if s.notifier == nil {
		return nil
	}
	n, ok := s.notifier.Network(deviceID)
	if !ok {
		return nil
	}
	return &n
}

// SetStream abilita il data plane: registra GET /api/v1/stream (alla prossima
// chiamata di Routes) e collega le notifiche dal control plane.
func (s *Server) SetStream(h http.Handler, n StreamNotifier) {
	s.stream = h
	s.notifier = n
}

// Sessions adatta il device.Store all'interfaccia stream.Sessions.
func Sessions(devices *device.Store) stream.Sessions { return storeSessions{devices} }

type storeSessions struct{ devices *device.Store }

// Lookup implementa stream.Sessions.
func (s storeSessions) Lookup(deviceID, sessionID string) (stream.Session, bool) {
	// Una sessione vecchia non apre lo stream: serve esattamente il session_id corrente.
	d, ok := s.devices.Get(deviceID)
	if !ok || d.SessionID != sessionID {
		return stream.Session{}, false
	}
	return stream.Session{ModelKey: d.Model.ModelKey, Policy: d.Policy, Postprocess: d.Model.Postprocess,
		Probe: d.Capabilities.Probe}, true
}

// QueueSource fornisce lo stato della coda del data plane.
// *dispatch.Dispatcher la soddisfa.
type QueueSource interface {
	Snapshot() dispatch.Snapshot
}

// SetQueue aggiunge lo stato della coda a GET /api/v1/server/status.
func (s *Server) SetQueue(q QueueSource) { s.queue = q }

// SetRecorder attiva la registrazione di sessioni, heartbeat e cambi di
// policy (internal/record).
func (s *Server) SetRecorder(r *record.Recorder) { s.rec = r }

// queueState è lo stato della coda per la registrazione; nil senza data plane.
func (s *Server) queueState() *record.Queue {
	if s.queue == nil || s.rec == nil {
		return nil
	}
	q := s.queue.Snapshot()
	return &record.Queue{
		Slots: q.Slots, Running: q.Running, Queued: q.Queued,
		OfferedPerSec: q.OfferedPerSec, AdmittedPerSec: q.AdmittedPerSec,
		Utilization: q.Utilization, PredictedWaitMS: q.PredictedWaitMS,
	}
}
