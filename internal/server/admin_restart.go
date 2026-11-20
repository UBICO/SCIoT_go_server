package server

import (
	"log"
	"net/http"
	"os"
)

// POST /api/v1/admin/restart: riavvio del server dalla dashboard
// (docs/ADMIN_API.md). Serve dopo un salvataggio di config.json: la config
// nuova vale solo al prossimo avvio.
//
// Il server non si riavvia da solo: chiama la funzione data da main
// (SetRestart), che cancella il context di run come un Ctrl+C. run spegne
// tutto in ordine (stream, porte, decisioni, registrazione, worker) e
// restituisce true; main allora rilancia il programma con syscall.Exec,
// stessi argomenti e ambiente (cmd/server/main.go).
//
// Prima di riavviare si controlla config.json con gli stessi controlli
// dell'avvio: un file che non partirebbe (modificato a mano) spegnerebbe il
// server senza farlo ripartire, e con lui la dashboard.

// SetRestart abilita il riavvio dalla dashboard: restart avvia lo
// spegnimento. Va chiamata prima di AdminRoutes, come SetConfigFile.
func (s *Server) SetRestart(restart func()) { s.restart = restart }

// handleAdminRestart: POST /api/v1/admin/restart, senza corpo.
//
//	202 {"restarting": true} → lo spegnimento parte dopo la risposta
//	400 invalid_config       → config.json non partirebbe: niente riavvio
//	404 not_found            → riavvio o config dalla dashboard spenti
//	500                      → config.json illeggibile: niente riavvio
func (s *Server) handleAdminRestart(w http.ResponseWriter, r *http.Request) {
	// Senza config non c'è nessun controllo possibile: meglio non riavviare.
	if s.restart == nil || s.config == nil || s.config.check == nil {
		writeError(w, http.StatusNotFound, errCodeNotFound, "restart is not enabled")
		return
	}
	data, err := os.ReadFile(s.config.path)
	if err != nil {
		log.Printf("admin restart: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot read the config file: not restarting")
		return
	}
	// current = next: il file com'è, la regola di worker.command non scatta.
	if err := s.config.check(data, data); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidConfig, "config.json would not start the server: "+err.Error())
		return
	}
	s.auditRestartRequest(r, auditConfigEntry{Path: s.config.path, Hash: hashOf(data)})
	// Prima la risposta, poi lo spegnimento: lo Shutdown della porta admin
	// aspetta la fine di questa richiesta, quindi il 202 arriva al browser.
	writeJSON(w, http.StatusAccepted, map[string]bool{"restarting": true})
	s.restart()
}
