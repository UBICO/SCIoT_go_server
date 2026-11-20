package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/device"
)

// Registro delle modifiche (docs/ADMIN_API.md#change-log): chi ha cambiato
// cosa con le azioni admin (pin, unpin, salvataggio della config, riavvio).
//
// Ogni azione riuscita va sempre nel log del server, con il nome di chi
// l'ha fatta. Con la sezione "record" in config.json va anche in
// <record.dir>/sciot-audit.jsonl: una riga JSON per azione, un solo file per
// tutti gli avvii (in coda), così la storia sopravvive ai riavvii. Non è una
// metrica e non entra nella registrazione (RECORDING.md): è chi ha fatto
// cosa, non cosa è successo ai frame.

// AuditFile è il nome del file nel record.dir.
const AuditFile = "sciot-audit.jsonl"

// Azioni del registro.
const (
	auditPin     = "pin"
	auditUnpin   = "unpin"
	auditConfig  = "config"
	auditRestart = "restart"
	// Build dei modelli: l'avvio e l'annullamento, con chi (il risultato
	// non ha un "chi": è nel log del server e nello stato del job).
	auditBuildStart  = "model_build"
	auditBuildCancel = "model_build_cancel"
)

// Chi, quando non c'è il nome di un login.
const (
	// actorToken: una richiesta con Authorization: Bearer. Lo script ha il
	// token ma non un nome.
	actorToken = "token"
	// actorAnonymous: porta admin senza token (solo loopback) e senza login.
	actorAnonymous = "anonymous"
	// actorDevicePort: gli endpoint di -admin sulla porta dei device, senza
	// autenticazione. Lì non passa wrap, quindi il context non ha un nome.
	actorDevicePort = "device port"
)

// auditEntry è una riga di sciot-audit.jsonl. Pin e unpin hanno i campi del
// device, il salvataggio della config e il riavvio hanno config:
//
//	{"ts":"2026-11-19T15:04:05.123Z","who":"francesco","action":"pin",
//	 "device_id":"pi_a","session_id":"FK2F…",
//	 "policy":{"mode":"SPLIT","split_point":3,"policy_version":4},"pinned":true}
//	{"ts":"…","who":"francesco","action":"config",
//	 "config":{"path":"/srv/sciot/config.json","backup":"config.json.20261119-150405.bak","hash":"…"}}
//	{"ts":"…","who":"francesco","action":"restart",
//	 "config":{"path":"/srv/sciot/config.json","hash":"…"}}
//
// Puntatori con omitempty: un campo che non c'entra con l'azione manca dalla
// riga; pinned resta anche quando è false (dopo un unpin).
type auditEntry struct {
	TS        time.Time         `json:"ts"`
	Who       string            `json:"who"`
	Action    string            `json:"action"`
	DeviceID  string            `json:"device_id,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	Policy    *api.Policy       `json:"policy,omitempty"` // la policy dopo l'azione
	Pinned    *bool             `json:"pinned,omitempty"` // dopo l'azione
	Config    *auditConfigEntry `json:"config,omitempty"`
	Build     *auditBuildEntry  `json:"build,omitempty"`
}

// auditBuildEntry descrive un build di un modello.
type auditBuildEntry struct {
	ID       string `json:"id"`
	ModelKey string `json:"model_key"`
	Output   string `json:"output"`
}

// auditConfigEntry descrive config.json in un salvataggio o in un riavvio.
type auditConfigEntry struct {
	Path   string `json:"path"`             // il file
	Backup string `json:"backup,omitempty"` // salvataggio: il backup del contenuto precedente, nella stessa cartella
	Hash   string `json:"hash"`             // sha256 del contenuto scritto, o di quello con cui si riparte
}

// AuditLog è il file del registro. Lo apre main (OpenAudit) e lo passa con
// Server.SetAudit; nil = solo il log del server.
type AuditLog struct {
	mu sync.Mutex // una riga alla volta: due pin insieme non si mescolano
	f  *os.File
}

// OpenAudit apre (o crea) dir/AuditFile in coda.
func OpenAudit(dir string) (*AuditLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// 0o600: dentro ci sono i nomi di chi usa la dashboard, lo legge solo
	// l'utente del servizio. O_APPEND: ogni Write va in fondo, anche dopo un
	// riavvio.
	f, err := os.OpenFile(filepath.Join(dir, AuditFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &AuditLog{f: f}, nil
}

// Close chiude il file.
func (a *AuditLog) Close() error {
	return a.f.Close()
}

// write aggiunge e come una riga JSON.
func (a *AuditLog) write(e auditEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	// Una Write sola: con O_APPEND la riga arriva intera.
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err = a.f.Write(append(data, '\n'))
	return err
}

// SetAudit dà al server il file del registro. Va chiamata prima di Routes e
// AdminRoutes, come SetRecorder.
func (s *Server) SetAudit(a *AuditLog) { s.auditLog = a }

// audit registra un'azione riuscita su d (lo stato dopo l'azione).
func (s *Server) audit(r *http.Request, action string, d device.Device) {
	policy, pinned := d.Policy, d.Pinned
	e := auditEntry{
		TS:        s.now().UTC(),
		Who:       actorOf(r),
		Action:    action,
		DeviceID:  d.ID,
		SessionID: d.SessionID,
		Policy:    &policy,
		Pinned:    &pinned,
	}
	log.Printf("admin %s: device=%s session=%s policy=%s v%d by=%q",
		action, d.ID, d.SessionID, d.Policy.Mode, d.Policy.PolicyVersion, e.Who)
	s.writeAudit(e)
}

// auditConfigSave registra il salvataggio di config.json.
func (s *Server) auditConfigSave(r *http.Request, c auditConfigEntry) {
	e := auditEntry{TS: s.now().UTC(), Who: actorOf(r), Action: auditConfig, Config: &c}
	log.Printf("admin config: path=%s backup=%s by=%q", c.Path, c.Backup, e.Who)
	s.writeAudit(e)
}

// auditRestartRequest registra un riavvio chiesto dalla dashboard, con la
// config con cui il server ripartirà.
func (s *Server) auditRestartRequest(r *http.Request, c auditConfigEntry) {
	e := auditEntry{TS: s.now().UTC(), Who: actorOf(r), Action: auditRestart, Config: &c}
	log.Printf("admin restart: config=%s hash=%s by=%q", c.Path, c.Hash, e.Who)
	s.writeAudit(e)
}

// auditBuild registra l'avvio o l'annullamento di un build.
func (s *Server) auditBuild(r *http.Request, action string, job *buildJob) {
	e := auditEntry{TS: s.now().UTC(), Who: actorOf(r), Action: action,
		Build: &auditBuildEntry{ID: job.ID, ModelKey: job.ModelKey, Output: job.Output}}
	log.Printf("admin %s: id=%s output=%s by=%q", action, job.ID, job.Output, e.Who)
	s.writeAudit(e)
}

// writeAudit scrive e nel file, se c'è.
func (s *Server) writeAudit(e auditEntry) {
	if s.auditLog == nil {
		return
	}
	if err := s.auditLog.write(e); err != nil {
		// L'azione è già fatta: un registro rotto non la annulla, si dice nel log.
		log.Printf("audit: %v", err)
	}
}

// actorKey è la chiave del nome di chi fa la richiesta nel context.
type actorKey struct{}

// withActor restituisce r con who nel context. La chiama wrap.
func withActor(r *http.Request, who string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), actorKey{}, who))
}

// actorOf restituisce chi fa la richiesta: il nome messo da wrap, oppure
// actorDevicePort se r non è passata da wrap (porta dei device, -admin).
func actorOf(r *http.Request) string {
	who, ok := r.Context().Value(actorKey{}).(string)
	if !ok {
		return actorDevicePort
	}
	return who
}
