package dispatch

import (
	"cmp"
	"slices"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/ewma"
)

// Totals sono i contatori dall'avvio. Insieme a λ e ρ dicono alla Fase 3
// quanto il server riesce davvero a servire.
type Totals struct {
	Rejected    uint64 `json:"rejected"`    // server_busy: non ammessi
	Expired     uint64 `json:"expired"`     // deadline_exceeded in coda: previsione sbagliata
	Completed   uint64 `json:"completed"`   // eseguiti con successo
	Failed      uint64 `json:"failed"`      // ammessi, ma l'esecuzione è fallita
	Unavailable uint64 `json:"unavailable"` // arrivati senza un worker pronto
}

// ServiceTime è la S stimata per un modello e uno split point.
type ServiceTime struct {
	ModelKey   string `json:"model_key"`
	SplitPoint int    `json:"split_point"`
	Stats
}

// SegmentTime è il tempo medio di invoke() di un segmento.
type SegmentTime struct {
	ModelKey string `json:"model_key"`
	Index    int    `json:"index"`
	Stats
}

// OverheadTime è il tempo medio, per modello, che S misura oltre ai
// segmenti (gRPC, copia del tensore, attesa nel worker).
type OverheadTime struct {
	ModelKey string `json:"model_key"`
	Stats
}

// Stats è una stima del dispatcher in millisecondi: media, deviazione
// standard dei campioni, quanti campioni e da quanto non ne arriva uno.
type Stats struct {
	MeanMS  float64 `json:"mean_ms"`
	StdMS   float64 `json:"std_ms"`
	Samples uint64  `json:"samples"`
	AgeMS   float64 `json:"age_ms"`
}

func statsOf(s ewma.Stat, now time.Time) Stats {
	e, _ := s.Estimate(now, 1000) // secondi → millisecondi
	return Stats{MeanMS: e.Mean, StdMS: e.Std, Samples: e.N, AgeMS: e.AgeMS}
}

// Snapshot è lo stato della coda in un istante: esposto in
// GET /api/v1/server/status e, nella Fase 3, ingresso dell'algoritmo.
type Snapshot struct {
	Slots   int `json:"slots"` // c
	Running int `json:"running"`
	Queued  int `json:"queued"`

	WindowSeconds  float64 `json:"window_s"`
	OfferedPerSec  float64 `json:"offered_per_s"`  // λ offerta: frame arrivati, ammessi o no
	AdmittedPerSec float64 `json:"admitted_per_s"` // λ ammessa
	// Utilization è ρ misurata: tempo di slot occupato / (c × finestra).
	// Assente senza worker (c = 0).
	Utilization *float64 `json:"utilization,omitzero"`
	// PredictedWaitMS è l'attesa in coda che vedrebbe un frame in arrivo adesso.
	PredictedWaitMS float64 `json:"predicted_wait_ms"`

	Service  []ServiceTime  `json:"service"`  // S misurata per split point
	Segments []SegmentTime  `json:"segments"` // per stimare S degli split mai usati
	Overhead []OverheadTime `json:"overhead"`
	Totals   Totals         `json:"totals"`
}

// Snapshot restituisce lo stato attuale.
func (d *Dispatcher) Snapshot() Snapshot {
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()

	slots := d.backend.Slots()
	sum := d.window.sum(now)
	w := d.window.length().Seconds()
	snap := Snapshot{
		Slots:          slots,
		Running:        len(d.running),
		Queued:         len(d.queue),
		WindowSeconds:  w,
		OfferedPerSec:  float64(sum.offered) / w,
		AdmittedPerSec: float64(sum.admitted) / w,
		Service:        make([]ServiceTime, 0, len(d.service)),
		Segments:       make([]SegmentTime, 0, len(d.segments)),
		Overhead:       make([]OverheadTime, 0, len(d.overhead)),
		Totals:         d.totals,
	}
	if slots > 0 {
		rho := sum.busy.Seconds() / (float64(slots) * w)
		snap.Utilization = &rho
		snap.PredictedWaitMS = ms(predictWait(slots, d.remainingLocked(now), d.queuedLocked()))
	}
	for k, s := range d.service {
		snap.Service = append(snap.Service, ServiceTime{ModelKey: k.model, SplitPoint: k.split, Stats: statsOf(s, now)})
	}
	// Ordine stabile: le mappe in Go non hanno ordine, il JSON sì.
	slices.SortFunc(snap.Service, func(a, b ServiceTime) int {
		return cmp.Or(cmp.Compare(a.ModelKey, b.ModelKey), cmp.Compare(a.SplitPoint, b.SplitPoint))
	})
	for k, s := range d.segments {
		snap.Segments = append(snap.Segments, SegmentTime{ModelKey: k.model, Index: k.index, Stats: statsOf(s, now)})
	}
	slices.SortFunc(snap.Segments, func(a, b SegmentTime) int {
		return cmp.Or(cmp.Compare(a.ModelKey, b.ModelKey), cmp.Compare(a.Index, b.Index))
	})
	for m, s := range d.overhead {
		snap.Overhead = append(snap.Overhead, OverheadTime{ModelKey: m, Stats: statsOf(s, now)})
	}
	slices.SortFunc(snap.Overhead, func(a, b OverheadTime) int { return cmp.Compare(a.ModelKey, b.ModelKey) })
	return snap
}

// ---------------------------------------------------------------------------
// Finestra scorrevole
// ---------------------------------------------------------------------------

// bucket accumula gli eventi di un secondo.
type bucket struct {
	sec      int64 // secondo Unix a cui si riferisce
	offered  int
	admitted int
	busy     time.Duration // tempo di slot occupato dai job terminati in quel secondo
}

// window è un anello di bucket da un secondo: la memoria non cresce con il
// traffico e una lettura costa len(ring) somme, qualunque sia il carico.
//
// Il busy di un job viene contato tutto nel secondo in cui termina: con job
// molto più brevi della finestra (ms contro 10 s) l'errore è trascurabile.
type window struct {
	ring []bucket
}

func newWindow(d time.Duration) *window {
	n := max(1, int(d/time.Second))
	return &window{ring: make([]bucket, n)}
}

func (w *window) length() time.Duration { return time.Duration(len(w.ring)) * time.Second }

// add somma b (sec escluso) al bucket del secondo di now.
func (w *window) add(now time.Time, b bucket) {
	sec := now.Unix()
	n := int64(len(w.ring))
	i := ((sec % n) + n) % n
	if w.ring[i].sec != sec {
		// Il bucket contiene un secondo vecchio, di un giro precedente dell'anello.
		w.ring[i] = bucket{sec: sec}
	}
	w.ring[i].offered += b.offered
	w.ring[i].admitted += b.admitted
	w.ring[i].busy += b.busy
}

// sum restituisce la somma dei bucket degli ultimi len(ring) secondi,
// secondo corrente compreso.
func (w *window) sum(now time.Time) bucket {
	// Solo i bucket con sec in (now-len(ring), now]: gli altri sono vecchi
	// (nessun evento da allora) e non vanno contati.
	cur := now.Unix()
	oldest := cur - int64(len(w.ring))
	var total bucket
	for _, b := range w.ring {
		if b.sec > oldest && b.sec <= cur {
			total.offered += b.offered
			total.admitted += b.admitted
			total.busy += b.busy
		}
	}
	return total
}
