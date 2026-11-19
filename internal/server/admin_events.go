package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// GET /api/v1/admin/events: lo stato del server e dei device come
// Server-Sent Events, per la dashboard (docs/ADMIN_API.md).
//
// Nessun hub e nessun aggancio al percorso degli heartbeat: ogni connessione
// legge lo Store con il suo ticker e manda un device solo quando è cambiato.
// Un browser lento rallenta solo la propria connessione.

// eventsTickDivisor: lo Store si legge ogni HeartbeatInterval / 4, quindi un
// heartbeat arriva alla dashboard al più un quarto di intervallo dopo il
// server. Lo stato del server si manda una volta ogni eventsTickDivisor tick,
// cioè una volta per intervallo.
const eventsTickDivisor = 4

// deviceKey è ciò che, se cambia, fa mandare di nuovo un device: un
// heartbeat (lastSeen), una policy nuova o un pin (policyVersion, pinned),
// una nuova registrazione (sessionID), una decisione (decisionAt).
//
// I tempi sono in nanosecondi (UnixNano) e non time.Time: == su time.Time
// confronta anche la lettura monotona e il fuso, e due istanti uguali
// potrebbero risultare diversi. Con int64 la struct si confronta con ==.
type deviceKey struct {
	sessionID     string
	lastSeen      int64
	policyVersion uint64
	pinned        bool
	decisionAt    int64 // 0 = nessuna decisione
}

// keyOf restituisce la chiave di d.
func keyOf(d adminDevice) deviceKey {
	k := deviceKey{
		sessionID:     d.SessionID,
		lastSeen:      d.LastSeen.UnixNano(),
		policyVersion: d.Policy.PolicyVersion,
		pinned:        d.Pinned,
	}
	if d.LastDecision != nil {
		k.decisionAt = d.LastDecision.At.UnixNano()
	}
	return k
}

// writeEvent scrive un evento SSE e lo manda subito al client:
//
//	event: device
//	data: {"device_id": "pi_a", ...}
//	(riga vuota)
//
// json.Marshal non produce mai un a capo dentro il JSON, quindi una sola
// riga data: basta.
func writeEvent(w io.Writer, rc *http.ResponseController, event string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	// Senza Flush l'evento resta nel buffer e il browser lo vede solo quando
	// il buffer è pieno.
	return rc.Flush()
}

// handleAdminEvents tiene aperta la connessione e manda:
//   - subito: "status" e un "device" per ogni device registrato;
//   - poi, a ogni tick: un "device" per ogni device la cui chiave è cambiata;
//   - ogni eventsTickDivisor tick: "status".
//
// Esce quando il client chiude o il server si ferma (r.Context() cancellato:
// main dà all'adminServer il context del segnale come BaseContext).
func (s *Server) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)

	// Il WriteTimeout dell'adminServer (10 s) chiuderebbe lo stream: lo si
	// toglie solo per questa richiesta. ErrNotSupported = ResponseRecorder dei test.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("admin events: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache") // un proxy non deve tenere in cache lo stream
	w.WriteHeader(http.StatusOK)

	// Istantanea: lo stato completo, così non serve nessun GET prima.
	if err := writeEvent(w, rc, "status", s.status()); err != nil {
		return // il client non c'è più
	}
	sent := make(map[string]deviceKey)
	for _, d := range s.adminDevices() {
		if err := writeEvent(w, rc, "device", d); err != nil {
			return
		}
		sent[d.DeviceID] = keyOf(d)
	}

	// Un ticker non letto in tempo salta i tick, non li accumula: un client
	// lento riceve meno controlli, non una coda.
	ticker := time.NewTicker(s.cfg.HeartbeatInterval / eventsTickDivisor)
	defer ticker.Stop()
	for n := 1; ; n++ {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
		for _, d := range s.adminDevices() {
			// Un device nuovo non è in sent: k != zero value, quindi si manda.
			if k := keyOf(d); k != sent[d.DeviceID] {
				if err := writeEvent(w, rc, "device", d); err != nil {
					return
				}
				sent[d.DeviceID] = k
			}
		}
		if n%eventsTickDivisor == 0 {
			if err := writeEvent(w, rc, "status", s.status()); err != nil {
				return
			}
		}
	}
}
