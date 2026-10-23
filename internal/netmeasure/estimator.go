package netmeasure

import (
	"sync"
	"time"
)

// Parametri della stima, fissi: non serve esporli in config.json.
const (
	// BandwidthAlpha è il peso del campione nuovo nella media della banda.
	BandwidthAlpha = 0.2
	// PingWindow: la latenza è il MINIMO degli ultimi PingWindow ping, non una
	// media. Il pong del device viaggia sulla stessa connessione dei frame e
	// aspetta dietro un frame che sta inviando: quei ping misurano l'invio,
	// non la latenza. Il minimo scarta i ping ritardati (come min_rtt in BBR).
	PingWindow = 10
	// Rete "non limitante": tra gli ultimi RecentWindow campioni, meno di
	// NotLimitingShare sono validi. Il server legge più lentamente di quanto i
	// byte arrivino: la rete non è il collo di bottiglia (localhost, LAN).
	RecentWindow     = 20
	NotLimitingShare = 0.1
)

// State è la rete di un device vista dal server.
type State struct {
	// BandwidthBps è la media mobile della banda dei campioni validi, in byte
	// al secondo; nil se non c'è ancora un campione valido.
	BandwidthBps *float64 `json:"bandwidth_bps,omitempty"`
	// LatencyMS è metà del minimo RTT degli ultimi PingWindow ping; nil senza ping.
	LatencyMS *float64 `json:"latency_ms,omitempty"`
	// NotLimiting: quasi tutti i campioni recenti sono stati scartati, la rete
	// è più veloce di quanto il server legga. BandwidthBps non va presa come
	// un limite.
	NotLimiting bool   `json:"not_limiting"`
	Valid       uint64 `json:"valid_samples"`
	Discarded   uint64 `json:"discarded_samples"`
}

// UploadMS stima il tempo di invio di bytes: latenza + byte / banda. ok è
// false se manca la banda o la latenza; con una rete non limitante vale la
// sola latenza.
func (s State) UploadMS(bytes int) (ms float64, ok bool) {
	if s.LatencyMS == nil {
		return 0, false
	}
	if s.NotLimiting {
		return *s.LatencyMS, true
	}
	if s.BandwidthBps == nil || *s.BandwidthBps <= 0 {
		return 0, false
	}
	return *s.LatencyMS + float64(bytes) / *s.BandwidthBps * 1000, true
}

// Estimator tiene lo stato di rete di un device. Sicuro da più goroutine:
// lo stream lo aggiorna (letture e ping), il control plane lo legge.
type Estimator struct {
	mu        sync.Mutex
	bandwidth *float64
	pings     []time.Duration // RTT degli ultimi ping, al massimo PingWindow
	valid     uint64
	discarded uint64
	recent    []bool // validità degli ultimi campioni, al massimo RecentWindow
}

// NewEstimator crea uno stato vuoto.
func NewEstimator() *Estimator { return &Estimator{} }

// Observe aggiunge un campione di ricezione.
func (e *Estimator) Observe(s Sample) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.Valid {
		e.valid++
	} else {
		e.discarded++
	}
	e.recent = append(e.recent, s.Valid)
	if len(e.recent) > RecentWindow {
		e.recent = e.recent[1:]
	}
	if !s.Valid {
		return
	}
	v := s.BandwidthBps()
	if e.bandwidth != nil {
		v = BandwidthAlpha*v + (1-BandwidthAlpha)**e.bandwidth
	}
	// Puntatore nuovo: Snapshot lo restituisce, chi lo legge non deve vederlo cambiare.
	e.bandwidth = &v
}

// ObservePing aggiunge la durata di un ping (andata e ritorno).
func (e *Estimator) ObservePing(rtt time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pings = append(e.pings, rtt)
	if len(e.pings) > PingWindow {
		e.pings = e.pings[1:]
	}
}

// Snapshot restituisce lo stato attuale.
func (e *Estimator) Snapshot() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return State{
		BandwidthBps: e.bandwidth,
		LatencyMS:    latencyMS(e.pings),
		NotLimiting:  notLimiting(e.recent),
		Valid:        e.valid,
		Discarded:    e.discarded,
	}
}

// latencyMS è la latenza di sola andata: metà del minimo RTT, in ms. nil
// senza ping.
func latencyMS(pings []time.Duration) *float64 {
	if len(pings) == 0 {
		return nil
	}
	m := pings[0]
	for _, p := range pings[1:] {
		m = min(m, p)
	}
	v := float64(m) / float64(time.Millisecond) / 2
	return &v
}

// notLimiting dice se la rete non è il collo di bottiglia.
func notLimiting(recent []bool) bool {
	// Con pochi campioni non si può dire.
	if len(recent) < RecentWindow {
		return false
	}
	valid := 0
	for _, v := range recent {
		if v {
			valid++
		}
	}
	return float64(valid)/float64(len(recent)) < NotLimitingShare
}
