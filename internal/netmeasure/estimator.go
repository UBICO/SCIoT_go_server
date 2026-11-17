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
	// LatencyWindow: la latenza è il MINIMO dei ping puliti degli ultimi
	// LatencyWindow, non una media. Il pong del device viaggia sullo stesso
	// link dei frame: con il link saturo aspetta dietro i byte già nel buffer
	// TCP di invio del device (bufferbloat: ~170 ms a 50 Mbit/s, misurato con
	// client/scripts/bench_network.py) e misura la coda, non la latenza. Un
	// ping è pulito se nessun messaggio è finito mentre era in viaggio
	// (ping.clean). Senza ping puliti nella finestra vale il ping pulito più
	// recente, anche più vecchio: la latenza di base cambia di rado, e un
	// device saturo per più di LatencyWindow (Fase 3.9: 1968 ms a 5 Mbit/s;
	// Fase 4.2, R2_w3: 65 ms contro 1,6) non la gonfia più. Senza nessun ping
	// pulito (saturo da quando è connesso) il minimo di tutti: include la coda.
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
	// BandwidthBps è la banda dei campioni validi, in byte al secondo: l'inverso
	// della media mobile del loro tempo per byte, pesata sul tempo; nil se non
	// c'è ancora un campione valido. Non la media delle bande: con tempi
	// dispersi (frame piccoli) quella è più alta di byte / tempo medio, e
	// UploadMS esce troppo corto (Fase 4.2, R5: 16 contro 11 MB/s).
	// BandwidthStdBps è la stessa dispersione relativa (CV) del tempo per byte,
	// riportata sulla banda; BandwidthAgeMS da quanto non arriva un campione
	// valido.
	BandwidthBps    *float64 `json:"bandwidth_bps,omitempty"`
	BandwidthStdBps *float64 `json:"bandwidth_std_bps,omitempty"`
	BandwidthAgeMS  *float64 `json:"bandwidth_age_ms,omitempty"`
	// LatencyMS è metà del minimo RTT dei ping puliti degli ultimi LatencyWindow
	// (vedi latencyMS per i ripieghi); nil senza ping.
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
	perByte   ewma.Stat     // dei campioni validi, in secondi per byte
	pings     []ping        // dell'ultimo LatencyWindow più il pulito più recente, dal più vecchio
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
	// clean: nessun messaggio è finito dopo l'invio del ping (at − rtt),
	// quindi il pong non ha fatto coda dietro un frame. Basta lastSample: il
	// pong non entra in un messaggio WebSocket già iniziato, e lo stream
	// chiama Observe prima di leggere il messaggio dopo (internal/stream,
	// readLoop), quindi un frame in arrivo durante il ping è già osservato
	// quando arriva il pong. Shortcut: vale per messaggi non frammentati (il
	// client Python); un client che frammenta può far passare un pong tra due
	// frammenti, che resterebbe pulito. Conservativo: un messaggio finito
	// durante il ping lo sporca anche se il pong non gli era dietro.
	clean bool
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
		// Media mobile del tempo per byte, non della banda. Un campione valido
		// ha sempre Bytes > 0 (sampleOf).
		e.perByte.Update(at, s.Receive.Seconds()/float64(s.Bytes), e.tau)
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
// tornato in at, e dimentica i ping più vecchi di LatencyWindow, tranne il
// più recente e il pulito più recente (la latenza di riserva, LatencyWindow).
//
// Senza nuovi ping (stream chiuso) i vecchi restano: la latenza sopravvive
// alle riconnessioni come la banda.
func (e *Estimator) ObservePing(at time.Time, rtt time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Sporco = un messaggio finito dopo l'invio del ping (at − rtt). Anche dopo
	// at: lastSample tiene solo l'ultimo, e un frame finito tra il pong e questa
	// chiamata nasconderebbe quello finito durante il ping. Con lastSample zero
	// (nessun messaggio) o == at − rtt il ping è pulito.
	sent := at.Add(-rtt)
	dirty := e.lastSample.After(sent)
	e.pings = append(e.pings, ping{at: at, rtt: rtt, clean: !dirty})
	cutoff := at.Add(-LatencyWindow)
	lastClean := -1
	for i, p := range e.pings {
		if p.clean {
			lastClean = i
		}
	}
	// Filtro sul posto: resta chi è nella finestra e il pulito più recente. Il
	// ping appena aggiunto è nella finestra, quindi e.pings non resta vuoto.
	kept := e.pings[:0]
	for i, p := range e.pings {
		if i == lastClean || !p.at.Before(cutoff) {
			kept = append(kept, p)
		}
	}
	e.pings = kept
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
	// b.Mean è in secondi per byte; con b.Mean == 0 (tempi nulli) niente banda.
	if b, ok := e.perByte.Estimate(now, 1); ok && b.Mean > 0 {
		bw := 1 / b.Mean
		sd := b.Std / b.Mean * bw // CV del tempo per byte, riportato sulla banda
		st.BandwidthBps, st.BandwidthStdBps, st.BandwidthAgeMS = &bw, &sd, &b.AgeMS
	}
	return st
}

// latencyMS è la latenza di sola andata, in ms: metà del minimo RTT dei ping
// puliti nella LatencyWindow che finisce col ping più recente; senza, del
// ping pulito più recente (più vecchio della finestra); senza nessun ping
// pulito, del minimo di tutti. nil senza ping. pings in ordine di at.
func latencyMS(pings []ping) *float64 {
	if len(pings) == 0 {
		return nil
	}
	cutoff := pings[len(pings)-1].at.Add(-LatencyWindow)
	all := pings[0].rtt
	var inWindow, lastClean time.Duration
	var hasWindow, hasClean bool
	for _, p := range pings {
		all = min(all, p.rtt)
		if !p.clean {
			continue
		}
		lastClean, hasClean = p.rtt, true // in ordine di at: resta il più recente
		if !p.at.Before(cutoff) && (!hasWindow || p.rtt < inWindow) {
			inWindow, hasWindow = p.rtt, true
		}
	}
	m := all
	switch {
	case hasWindow:
		m = inWindow
	case hasClean:
		m = lastClean
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
