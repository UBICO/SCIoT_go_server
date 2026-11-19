package server

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/netmeasure"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

// Porta admin: l'API della dashboard (docs/ADMIN_API.md).
//
// È un router separato da Routes, servito da un secondo http.Server su
// -admin-addr: la porta dei device resta esposta sulla LAN, quella admin si
// può tenere su localhost, e nessuna route admin è raggiungibile dalla porta
// dei device. Il vecchio -admin (route senza autenticazione sulla porta dei
// device) resta com'è.
//
// Tutto qui legge lo stato che il server calcola già: niente metriche
// nuove, nessun effetto su protocollo e algoritmo.

// AdminRoutes restituisce il router della porta admin. token vuoto = nessuna
// autenticazione (main lo permette solo su loopback, CheckAdminAddr).
// Va chiamata una volta sola, dopo SetStream/SetAlgorithm come Routes.
func (s *Server) AdminRoutes(token string) http.Handler {
	s.admin = newAdminAuth(token, s.now)

	// protected: le route che richiedono il login. I pattern sono interi
	// ("GET /api/v1/admin/status"): il mux esterno passa la richiesta così
	// com'è, senza togliere prefissi.
	protected := http.NewServeMux()
	protected.HandleFunc("POST /api/v1/admin/logout", s.handleAdminLogout)
	protected.HandleFunc("GET /api/v1/admin/status", s.handleServerStatus)
	protected.HandleFunc("GET /api/v1/admin/devices", s.handleAdminDevices)
	protected.HandleFunc("POST /api/v1/admin/devices/{id}/policy", s.handleSetPolicy)
	protected.HandleFunc("DELETE /api/v1/admin/devices/{id}/policy", s.handleUnpin)

	mux := http.NewServeMux()
	// Il login è l'unica route admin senza credenziali. Il mux sceglie il
	// pattern più specifico: questo vince su "/api/v1/admin/".
	mux.HandleFunc("POST /api/v1/admin/login", s.handleAdminLogin)
	mux.Handle("/api/v1/admin/", s.admin.wrap(protected))

	// CrossOriginProtection (net/http, Go 1.25) rifiuta le richieste che
	// modificano (POST, PUT, DELETE) mandate dal browser da un'altra origine,
	// leggendo Sec-Fetch-Site / Origin: è la difesa CSRF per il cookie.
	// curl e gli script non mandano quegli header e passano.
	return http.NewCrossOriginProtection().Handler(mux)
}

// ---------------------------------------------------------------------------
// Ultima decisione per device
// ---------------------------------------------------------------------------

// decisionLog tiene l'ultima decisione dell'algoritmo per ogni device, per
// mostrarne il motivo nella dashboard. La scrive la goroutine del decider
// (decideNow), la leggono gli handler admin.
type decisionLog struct {
	mu   sync.Mutex
	last map[string]adminDecision
}

// adminDecision è una decisione con l'ora in cui è stata presa. L'ora non
// si prende da Decision.TS: la imposta il Recorder, che può essere spento.
type adminDecision struct {
	At       time.Time        `json:"at"`
	Decision *record.Decision `json:"decision"`
}

// set salva d come ultima decisione di deviceID. d non va più modificato
// da chi chiama: gli handler lo leggono (e lo codificano) da altre goroutine.
func (l *decisionLog) set(deviceID string, at time.Time, d *record.Decision) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Lo zero value di decisionLog è utilizzabile: la map nasce alla prima scrittura.
	if l.last == nil {
		l.last = make(map[string]adminDecision)
	}
	l.last[deviceID] = adminDecision{At: at, Decision: d}
}

// get restituisce l'ultima decisione di deviceID, nil se non ce n'è.
func (l *decisionLog) get(deviceID string) *adminDecision {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Leggere da una map nil è lecito (dà lo zero value): niente controllo.
	d, ok := l.last[deviceID]
	if !ok {
		return nil
	}
	return &d
}

// forget toglie la decisione di deviceID: alla ri-registrazione quella
// della sessione precedente non vale più (come fleet.Forget).
func (l *decisionLog) forget(deviceID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.last, deviceID) // su una map nil non fa nulla
}

// ---------------------------------------------------------------------------
// GET /api/v1/admin/devices
// ---------------------------------------------------------------------------

// adminDevice è un device come lo vede la dashboard: lo stato dello Store
// più rete e ultima decisione. Come statusResponse resta qui, non in
// internal/api: non è un messaggio del protocollo dei device.
type adminDevice struct {
	DeviceID             string     `json:"device_id"`
	DeviceType           string     `json:"device_type"`
	SessionID            string     `json:"session_id"`
	ModelKey             string     `json:"model_key"`
	Policy               api.Policy `json:"policy"`
	Pinned               bool       `json:"pinned"`
	PolicySince          time.Time  `json:"policy_since"`
	AppliedPolicyVersion uint64     `json:"applied_policy_version"`
	RegisteredAt         time.Time  `json:"registered_at"`
	LastSeen             time.Time  `json:"last_seen"`
	// Telemetry è l'ultimo heartbeat; null prima del primo (assente ≠ 0).
	Telemetry *api.Telemetry `json:"telemetry"`
	// Network è la rete misurata sullo stream; null senza data plane o
	// prima che il device apra lo stream.
	Network *netmeasure.State `json:"network"`
	// LastDecision è null con l'algoritmo "static" o prima della prima
	// decisione.
	LastDecision *adminDecision `json:"last_decision"`
}

// adminDevicesResponse è il corpo di GET /api/v1/admin/devices.
type adminDevicesResponse struct {
	Devices []adminDevice `json:"devices"`
}

// handleAdminDevices elenca i device registrati, ordinati per ID.
//
//	200 → adminDevicesResponse ({"devices": []} senza device, mai null)
func (s *Server) handleAdminDevices(w http.ResponseWriter, _ *http.Request) {
	list := s.devices.List()
	// Non "var out []adminDevice": senza device il JSON sarebbe null invece di [].
	out := make([]adminDevice, 0, len(list))
	for _, d := range list {
		ad := adminDevice{
			DeviceID:             d.ID,
			DeviceType:           d.Type,
			SessionID:            d.SessionID,
			ModelKey:             d.Model.ModelKey,
			Policy:               d.Policy,
			Pinned:               d.Pinned,
			PolicySince:          d.PolicySince,
			AppliedPolicyVersion: d.AppliedPolicyVersion,
			RegisteredAt:         d.RegisteredAt,
			LastSeen:             d.LastSeen,
			Network:              s.network(d.ID),
			LastDecision:         s.decisions.get(d.ID),
		}
		// 0 = nessun heartbeat ancora: la telemetria è assente, non zero.
		// d è nuova a ogni iterazione (Go ≥ 1.22): &d.LastTelemetry è sicuro.
		if d.AppliedPolicyVersion > 0 {
			ad.Telemetry = &d.LastTelemetry
		}
		out = append(out, ad)
	}
	writeJSON(w, http.StatusOK, adminDevicesResponse{Devices: out})
}

// ---------------------------------------------------------------------------
// Controllo all'avvio
// ---------------------------------------------------------------------------

// CheckAdminAddr dice se la porta admin può partire su addr con token.
//
//   - token non vuoto: almeno MinAdminTokenLen caratteri, qualunque addr;
//   - token vuoto: solo su loopback (127.0.0.0/8, ::1, "localhost"), dove
//     arriva solo chi è già sulla macchina. ":8081" o "0.0.0.0:8081"
//     ascoltano su tutte le interfacce: senza token vanno rifiutati.
//
// Restituisce nil o un errore che dice cosa fare.
func CheckAdminAddr(addr, token string) error {
	if token != "" {
		if len(token) < MinAdminTokenLen {
			return fmt.Errorf("SCIOT_ADMIN_TOKEN is too short: at least %d characters", MinAdminTokenLen)
		}
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid admin address %q: %w", addr, err)
	}
	// host == "" (":8081") ascolta su tutte le interfacce: ParseIP("") è nil.
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("admin address %q is not loopback: set SCIOT_ADMIN_TOKEN (at least %d characters) or use 127.0.0.1:PORT",
		addr, MinAdminTokenLen)
}
