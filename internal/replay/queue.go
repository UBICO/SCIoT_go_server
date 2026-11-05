// Package replay rilegge una registrazione (internal/record, docs/RECORDING.md)
// per verificare offline i modelli del server: qui, la previsione dell'attesa
// in coda di internal/costmodel contro quella misurata (Fase 3.4, verifica 1).
// Nella Fase 3.5 rieseguirà anche gli algoritmi di offloading.
package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
)

// event è la parte di una riga della registrazione che serve qui.
type event struct {
	Kind    string  `json:"kind"`
	TMS     float64 `json:"t_ms"`
	Outcome string  `json:"outcome"`
	Timing  *struct {
		ServerMS    *float64 `json:"server_ms"`
		QueueWaitMS *float64 `json:"queue_wait_ms"`
	} `json:"timing"`
	Queue *struct {
		Slots int `json:"slots"`
	} `json:"queue"`
}

// served è un frame eseguito dal server, con i tempi ricostruiti.
type served struct {
	arrivalMS float64 // quando il server l'ha letto
	waitMS    float64 // in coda
	serviceMS float64 // in esecuzione (S misurata)
	slots     int     // slot del worker in quel momento (ultimo heartbeat)
}

// readServed legge i frame con esito "result" e gli slot dagli heartbeat.
//
// t_ms di un frame è quando l'esito è stato inviato: l'arrivo è
// t_ms − server_ms, il servizio server_ms − queue_wait_ms. Gli slot vengono
// dall'ultimo heartbeat precedente (queue.slots).
func readServed(r io.Reader) ([]served, error) {
	var out []served
	slots := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20) // un heartbeat con molti segmenti è lungo
	for line := 1; sc.Scan(); line++ {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		switch {
		case e.Kind == "heartbeat" && e.Queue != nil && e.Queue.Slots > 0:
			slots = e.Queue.Slots
		case e.Kind == "frame" && e.Outcome == "result" && e.Timing != nil &&
			e.Timing.ServerMS != nil && e.Timing.QueueWaitMS != nil && slots > 0:
			s, w := *e.Timing.ServerMS, *e.Timing.QueueWaitMS
			out = append(out, served{arrivalMS: e.TMS - s, waitMS: w, serviceMS: s - w, slots: slots})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// Ordine di arrivo: gli esiti escono in un ordine diverso.
	slices.SortFunc(out, func(a, b served) int { return cmpFloat(a.arrivalMS, b.arrivalMS) })
	return out, nil
}

// QueueWindow è il confronto in una finestra di tempo.
type QueueWindow struct {
	StartS    float64 // inizio, in secondi dall'avvio del server
	Frames    int
	Slots     int
	Lambda    float64 // frame eseguiti al secondo
	ServiceMS float64 // S medio misurato
	Rho       float64 // λ·S / c
	CA, CS    float64 // variazione di intervalli tra arrivi e servizi
	// MeasuredMS è l'attesa media misurata; MMcMS e GGcMS le previsioni
	// (+Inf se la coda è instabile, ρ ≥ 1).
	MeasuredMS, MMcMS, GGcMS float64
}

// QueueWindows divide i frame eseguiti in finestre di windowS secondi e, per
// ognuna con almeno minFrames frame, confronta l'attesa misurata con quelle
// previste da M/M/c e G/G/c (Allen–Cunneen) con i valori misurati nella
// stessa finestra.
//
// Limite: λ conta i frame eseguiti, non quelli rifiutati o scaduti in coda.
// Con l'ammissione attiva è il carico che la coda ha davvero visto.
func QueueWindows(r io.Reader, windowS float64, minFrames int) ([]QueueWindow, error) {
	frames, err := readServed(r)
	if err != nil {
		return nil, err
	}
	var out []QueueWindow
	for i := 0; i < len(frames); {
		start := math.Floor(frames[i].arrivalMS/1000/windowS) * windowS
		j := i
		for j < len(frames) && frames[j].arrivalMS < (start+windowS)*1000 {
			j++
		}
		if j-i >= minFrames {
			out = append(out, compare(frames[i:j], start, windowS))
		}
		i = j
	}
	return out, nil
}

func compare(fs []served, start, windowS float64) QueueWindow {
	w := QueueWindow{StartS: start, Frames: len(fs), Slots: fs[len(fs)-1].slots}
	var wait, svc, svc2 float64
	for _, f := range fs {
		wait += f.waitMS
		svc += f.serviceMS
		svc2 += f.serviceMS * f.serviceMS
	}
	n := float64(len(fs))
	w.MeasuredMS = wait / n
	w.ServiceMS = svc / n
	w.CS = cv(w.ServiceMS, svc2/n)

	var gap, gap2 float64
	for k := 1; k < len(fs); k++ {
		d := fs[k].arrivalMS - fs[k-1].arrivalMS
		gap += d
		gap2 += d * d
	}
	if m := float64(len(fs) - 1); m > 0 {
		w.CA = cv(gap/m, gap2/m)
	}

	w.Lambda = n / windowS
	s := w.ServiceMS / 1000
	w.Rho = w.Lambda * s / float64(w.Slots)
	mmc, ok := costmodel.WaitMMc(w.Slots, w.Lambda, s)
	ggc, _ := costmodel.WaitGGc(w.Slots, w.Lambda, s, w.CA, w.CS)
	if ok {
		w.MMcMS, w.GGcMS = mmc*1000, ggc*1000
	} else {
		w.MMcMS, w.GGcMS = math.Inf(1), math.Inf(1)
	}
	return w
}

// cv è il coefficiente di variazione da media e media dei quadrati.
func cv(mean, meanSq float64) float64 {
	if mean <= 0 {
		return 0
	}
	return math.Sqrt(max(0, meanSq-mean*mean)) / mean
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Le fasce di utilizzo ρ dei riassunti (Buckets, CheckBuckets): rhoEdges[k]
// è il limite inferiore della fascia k; l'ultima è ρ ≥ 1.
var (
	rhoEdges  = []float64{0, 0.5, 0.8, 0.95, 1}
	rhoLabels = []string{"0–0.5", "0.5–0.8", "0.8–0.95", "0.95–1", "≥ 1 (unstable)"}
)

// rhoBucket è l'indice della fascia di ρ.
func rhoBucket(rho float64) int {
	for k := 1; k < len(rhoEdges); k++ {
		if rho < rhoEdges[k] {
			return k - 1
		}
	}
	return len(rhoEdges) - 1 // instabile
}

// Bucket riassume le finestre di una fascia di utilizzo.
type Bucket struct {
	Label                    string
	Windows                  int
	MeasuredMS, MMcMS, GGcMS float64 // medie delle finestre
}

// Buckets raggruppa le finestre per ρ: [0, 0.5), [0.5, 0.8), [0.8, 0.95),
// [0.95, 1) e instabili (ρ ≥ 1, previsione infinita: solo la misurata).
func Buckets(ws []QueueWindow) []Bucket {
	out := make([]Bucket, len(rhoLabels))
	for i := range out {
		out[i].Label = rhoLabels[i]
	}
	for _, w := range ws {
		b := &out[rhoBucket(w.Rho)]
		b.Windows++
		b.MeasuredMS += w.MeasuredMS
		b.MMcMS += w.MMcMS
		b.GGcMS += w.GGcMS
	}
	for i := range out {
		if n := float64(out[i].Windows); n > 0 {
			out[i].MeasuredMS /= n
			out[i].MMcMS /= n
			out[i].GGcMS /= n
		}
	}
	return out
}
