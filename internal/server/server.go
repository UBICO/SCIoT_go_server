// Package server espone il protocollo v1 via HTTP.
//
// Collega i pezzi: decodifica le richieste (tipi di internal/api), consulta
// il Registry dei modelli e aggiorna lo Store dei device.
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/UBICO/sciot-go-server/internal/api"
	"github.com/UBICO/sciot-go-server/internal/device"
	"github.com/UBICO/sciot-go-server/internal/registry"
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
type Server struct {
	registry *registry.Registry
	devices  *device.Store
	cfg      Config
}

// New crea un Server.
func New(reg *registry.Registry, devices *device.Store, cfg Config) *Server {
	return &Server{registry: reg, devices: devices, cfg: cfg}
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
	mux.HandleFunc("GET /healthz", s.handleHealthz)
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
	// --- 1. Decodifica -------------------------------------------------------

	// limita il corpo e decodificalo.
	//   - r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	//     Oltre il limite, la lettura restituisce un errore invece di
	//     continuare a leggere.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req api.RegisterRequest

	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, "invalid JSON body")
		return
	}
	//   - dec := json.NewDecoder(r.Body) + dec.DisallowUnknownFields()
	//   - var req api.RegisterRequest
	//   - se dec.Decode(&req) fallisce:
	//       writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest,
	//                  "invalid JSON body")
	//     e poi return. ATTENZIONE: writeError NON interrompe l'handler.
	//     Senza return il codice prosegue e scrive una seconda risposta.
	//
	//   Nota: non c'è bisogno di chiudere r.Body, lo fa net/http.

	// --- 2. Validazione ------------------------------------------------------

	//  in quest'ordine, ognuno con writeError + return:
	//   - req.ProtocolVersion != supportedProtocolVersion
	//       → 400, api.ErrCodeUnsupportedProtocol
	//       Nel messaggio indica la versione supportata, usa fmt.Sprintf
	//       (dovrai aggiungere "fmt" agli import).
	//   - req.DeviceID == "" → 400, api.ErrCodeInvalidRequest, "device_id is required"
	//   - req.ModelHash == "" → 400, api.ErrCodeInvalidRequest, "model_hash is required"

	if req.ProtocolVersion != supportedProtocolVersion {
		message := fmt.Sprintf("unsupported protocol version: expected %d, got %d", supportedProtocolVersion, req.ProtocolVersion)
		writeError(w, http.StatusBadRequest, api.ErrCodeUnsupportedProtocol, message)
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

	// --- 3. Modello ----------------------------------------------------------

	//  model, ok := s.registry.Lookup(req.ModelHash)
	//   se !ok → 404, api.ErrCodeModelNotFound, + return
	model, ok := s.registry.Lookup(req.ModelHash)
	if !ok {
		writeError(w, http.StatusNotFound, api.ErrCodeModelNotFound, "model not found")
		return
	}
	// --- 4. Registrazione e risposta -----------------------------------------
	//   - d := s.devices.Register(req.DeviceID, req.DeviceType, model, req.Capabilities)
	//   - log.Printf con device_id, model_key e session_id: è utile vedere
	//     nel log del server ogni registrazione.
	//   - writeJSON(w, http.StatusOK, api.RegisterResponse{...})
	//       * HeartbeatIntervalMS: int(s.cfg.HeartbeatInterval.Milliseconds())
	//         Milliseconds() restituisce int64: in Go le conversioni tra tipi
	//         numerici sono sempre esplicite, quindi serve int(...).
	d := s.devices.Register(req.DeviceID, req.DeviceType, model, req.Capabilities)
	log.Printf("registered device=%s type=%s model=%s session=%s", req.DeviceID, d.Type, d.Model.ModelKey, d.SessionID)
	writeJSON(w, http.StatusOK, api.RegisterResponse{
		SessionID:           d.SessionID,
		Model:               d.Model,
		Policy:              d.Policy,
		HeartbeatIntervalMS: int(s.cfg.HeartbeatInterval.Milliseconds()),
	})
}
