package netmeasure

import (
	"sync"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/ewma"
)

// Parametri della stima, fissi: non serve esporli in config.json.
const (
	// DefaultBandwidthTau è la costante di tempo della stima della banda
	// (internal/ewma): con un probe ogni 30 s segue un cambio in un minuto.
	DefaultBandwidthTau = 60 * time.Second
	// LatencyWindow: la latenza è il MINIMO dei ping degli ultimi
	// LatencyWindow, non una media. Il pong del device viaggia sullo stesso
	// link dei frame: con il link saturo aspetta dietro i byte già nel buffer
	// TCP di invio del device (bufferbloat: ~170 ms a 50 Mbit/s, misurato con
	// client/scripts/bench_network.py) e misura la coda, non la latenza.
	// La finestra è lunga perché la latenza di base cambia di rado: il valore
	// misurato a link libero (LOCAL, avvio, pause) sopravvive a un periodo di
	// saturazione. Oltre LatencyWindow di saturazione continua la latenza
	// include la coda (come min_rtt in BBR, che però svuota il link per
	// rimisurarla).
	LatencyWindow = 60 * time.Second
	// PingTimeout: quanto aspettare un pong. Più dell'intervallo tra i ping:
	// con il link saturo un pong in ritardo è meglio di nessuna misura (a
	// 10 Mbit/s la coda supera 1 s).
	PingTimeout = 5 * time.Second
	// Rete "non limitante": tra i campioni dell'ultimo NotLimitingWindow
	// (almeno NotLimitingMinSamples), almeno NotLimitingBufferedShare vengono
	// dal buffer (Sample.Buffered). Il server legge più lentamente di quanto i
	// byte arrivino: la rete non è il collo di bottiglia (localhost, LAN). Una
	// finestra di tempo, non di campioni: con un probe ogni 30 s, 20 campioni
	// sarebbero 10 minuti.
	//
	// Contano i campioni dal buffer, non i "non validi": un campione che ha
	// aspettato l'85% dei byte non è valido ma dice che la rete limita (Fase
	// 3.9, ~8 Mbit/s: tre probe così davano "non limitante" e un invio di
	// 1,2 MB previsto in 3 ms).
	NotLimitingWindow        = 60 * time.Second
	NotLimitingMinSamples    = 3
	NotLimitingBufferedShare = 0.9
)

// State è la rete di un device vista dal server.
type State struct {
	// BandwidthBps è la media mobile della banda dei campioni validi, in byte
	// al secondo, pesata sul tempo; nil se non c'è ancora un campione valido.
	// BandwidthStdBps è la deviazione standard dei campioni, BandwidthAgeMS da
	// quanto non arriva un campione valido.
	BandwidthBps    *float64 `json:"bandwidth_bps,omitempty"`
	BandwidthStdBps *float64 `json:"bandwidth_std_bps,omitempty"`
	BandwidthAgeMS  *float64 `json:"bandwidth_age_ms,omitempty"`
	// LatencyMS è metà del minimo RTT dei ping degli ultimi LatencyWindow;
	// nil senza ping.
	LatencyMS *float64 `json:"latency_ms,omitempty"`
	// NotLimiting: quasi tutti i campioni recenti vengono dal buffer, la rete
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
	tau       time.Duration // della banda
	bandwidth ewma.Stat     // dei campioni validi, in byte al secondo
	pings     []ping        // dell'ultimo LatencyWindow, dal più vecchio
	valid     uint64
	discarded uint64
	recent    []mark // campioni dell'ultimo NotLimitingWindow, dal più vecchio
	// lastSample è quando è arrivato l'ultimo campione, valido o no: dice
	// quanto è vecchia la misura (la regola del probe, internal/stream).
	lastSample time.Time
}

// ping è un ping riuscito: quando è tornato il pong e la durata.
type ping struct {
	at  time.Time
	rtt time.Duration
}

// mark è un campione per la regola "non limitante": quando e se dal buffer.
type mark struct {
	at       time.Time
	buffered bool
}

// NewEstimator crea uno stato vuoto; bandwidthTau è la costante di tempo
// della banda (DefaultBandwidthTau, o stream.bandwidth_tau_ms).
func NewEstimator(bandwidthTau time.Duration) *Estimator {
	return &Estimator{tau: bandwidthTau}
}

// Observe aggiunge un campione di ricezione, arrivato in at.
func (e *Estimator) Observe(at time.Time, s Sample) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastSample = at
	if s.Valid {
		e.valid++
	} else {
		e.discarded++
	}
	e.recent = append(e.recent, mark{at: at, buffered: s.Buffered()})
	cutoff := at.Add(-NotLimitingWindow)
	for len(e.recent) > 0 && e.recent[0].at.Before(cutoff) {
		e.recent = e.recent[1:]
	}
	if s.Valid {
		e.bandwidth.Update(at, s.BandwidthBps(), e.tau)
	}
}

// LastSample è quando è arrivato l'ultimo campione; zero se nessuno.
//
// Un campione scartato conta quanto uno valido: dice che la rete non limita,
// ed è anche quella una misura aggiornata.
func (e *Estimator) LastSample() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastSample
}

// ObservePing aggiunge la durata di un ping (andata e ritorno) il cui pong è
// tornato in at, e dimentica i ping più vecchi di LatencyWindow.
//
// Senza nuovi ping (stream chiuso) i vecchi restano: la latenza sopravvive
// alle riconnessioni come la banda.
func (e *Estimator) ObservePing(at time.Time, rtt time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pings = append(e.pings, ping{at: at, rtt: rtt})
	cutoff := at.Add(-LatencyWindow)
	for len(e.pings) > 1 && e.pings[0].at.Before(cutoff) {
		e.pings = e.pings[1:]
	}
}

// Snapshot restituisce lo stato visto in now.
func (e *Estimator) Snapshot(now time.Time) State {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := State{
		LatencyMS:   latencyMS(e.pings),
		NotLimiting: notLimiting(e.recent, now),
		Valid:       e.valid,
		Discarded:   e.discarded,
	}
	// Valori nuovi a ogni Snapshot: chi tiene lo State non li vede cambiare.
	if b, ok := e.bandwidth.Estimate(now, 1); ok {
		st.BandwidthBps, st.BandwidthStdBps, st.BandwidthAgeMS = &b.Mean, &b.Std, &b.AgeMS
	}
	return st
}

// latencyMS è la latenza di sola andata: metà del minimo RTT, in ms. nil
// senza ping.
func latencyMS(pings []ping) *float64 {
	if len(pings) == 0 {
		return nil
	}
	m := pings[0].rtt
	for _, p := range pings[1:] {
		m = min(m, p.rtt)
	}
	v := float64(m) / float64(time.Millisecond) / 2
	return &v
}

// notLimiting dice se, visto da now, la rete non è il collo di bottiglia.
// recent sono i campioni in ordine di arrivo; Observe toglie quelli più
// vecchi di NotLimitingWindow rispetto all'ULTIMO campione, non a now.
func notLimiting(recent []mark, now time.Time) bool {
	// Solo i campioni entro NotLimitingWindow da now: se non arriva niente da
	// un minuto, la vecchia conclusione non vale più.
	cutoff := now.Add(-NotLimitingWindow)
	total, buffered := 0, 0
	for _, m := range recent {
		if m.at.Before(cutoff) {
			continue
		}
		total++
		if m.buffered {
			buffered++
		}
	}
	if total < NotLimitingMinSamples {
		return false
	}
	return float64(buffered)/float64(total) >= NotLimitingBufferedShare
}
