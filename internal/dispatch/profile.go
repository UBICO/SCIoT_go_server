package dispatch

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/stream"
)

// Profilo all'avvio: il dispatcher impara i tempi dei segmenti solo dai frame
// eseguiti, quindi un segmento che nessun device manda al server resta
// sconosciuto e S degli split che lo eseguono non è stimabile. Il profilo
// esegue il modello intero (split −1) qualche volta su un input di zeri: una
// sola esecuzione misura ogni segmento 0..last e l'overhead, e da lì
// fromSegments stima S per ogni split.
//
// I job del profilo aspettano uno slot come gli altri (non rubano il worker ai
// frame veri), ma non entrano in λ, ρ, interarrivi e S per split: non sono
// traffico dei device, e senza decoder la loro S non è quella di un frame.

// ProfileWarmup sono le esecuzioni scartate prima di quelle misurate: la prima
// invoke() alloca i tensori e scalda le cache. Con la media pesata di
// internal/ewma i primi campioni pesano tutti uguale, quindi un primo
// campione lento resterebbe a lungo nella stima.
const ProfileWarmup = 1

// profileTimeout è la scadenza di ogni esecuzione del profilo: ampia, perché
// nessun device aspetta il risultato.
const profileTimeout = 30 * time.Second

// Source dice da dove viene una stima di S.
type Source string

const (
	// SourceMeasured: media misurata sui frame di quello split point.
	SourceMeasured Source = "measured"
	// SourceSegments: somma dei segmenti più l'overhead del modello, misurati
	// su altri split point o dal profilo.
	SourceSegments Source = "segments"
)

// Estimate restituisce S per model e split e la sua origine, come l'ammissione
// la userebbe (vedi estimate), ma senza il ripiego sulla S più lunga: ok è
// false se S non è né misurata né ricavabile dai segmenti. Per una stima dai
// segmenti Stats ha solo MeanMS: deviazione e campioni sono delle misure
// dirette.
func (d *Dispatcher) Estimate(model string, split int) (st Stats, src Source, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := key{model, split}
	if s, ok := d.service[k]; ok {
		return statsOf(s, d.now()), SourceMeasured, true
	}
	if s, ok := d.fromSegments(k); ok {
		return Stats{MeanMS: ms(s)}, SourceSegments, true
	}
	return Stats{}, "", false
}

// ProfileResult è il risultato del profilo di un modello: media delle
// esecuzioni misurate (riscaldamento escluso).
type ProfileResult struct {
	ModelKey   string
	Runs       int       // esecuzioni misurate
	SegmentMS  []float64 // media per segmento 0..last
	OverheadMS float64   // media di S − Σ segmenti, mai sotto zero
	Duration   time.Duration
}

// ProfileModel esegue il modello intero ProfileWarmup + runs volte su un
// tensore di zeri di inputBytes byte (l'input grezzo, split −1) e semina con le
// esecuzioni misurate le stime per segmento e l'overhead del modello.
func (d *Dispatcher) ProfileModel(ctx context.Context, model string, inputBytes, runs int) (ProfileResult, error) {
	started := d.now()
	job := stream.Job{ModelKey: model, SplitPoint: -1, Tensor: make([]byte, inputBytes)}
	res := ProfileResult{ModelKey: model}

	for i := range ProfileWarmup + runs {
		layerMS, elapsed, err := d.runProfile(ctx, job)
		if err != nil {
			return res, fmt.Errorf("profile %s run %d: %w", model, i, err)
		}
		if i < ProfileWarmup {
			continue
		}
		d.mu.Lock()
		d.observeSegmentsLocked(d.now(), model, -1, layerMS, elapsed)
		d.mu.Unlock()

		if res.SegmentMS == nil {
			res.SegmentMS = make([]float64, len(layerMS))
		}
		var sum float64
		for j, v := range layerMS {
			res.SegmentMS[j] += v
			sum += v
		}
		res.OverheadMS += max(0, ms(elapsed)-sum)
		res.Runs++
	}
	if res.Runs > 0 {
		n := float64(res.Runs)
		for j := range res.SegmentMS {
			res.SegmentMS[j] /= n
		}
		res.OverheadMS /= n
	}

	res.Duration = d.now().Sub(started)
	return res, nil
}

// runProfile esegue job aspettando uno slot come un frame, senza ammissione
// (nessuna scadenza da rispettare) e senza entrare nelle metriche del
// traffico. Restituisce i tempi per segmento e la durata dell'esecuzione.
func (d *Dispatcher) runProfile(ctx context.Context, job stream.Job) ([]float64, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, profileTimeout)
	defer cancel()
	job.Deadline = d.now().Add(profileTimeout)

	d.mu.Lock()
	if d.backend.Slots() == 0 {
		d.mu.Unlock()
		return nil, 0, fmt.Errorf("%w: no worker ready", stream.ErrWorkerUnavailable)
	}
	// S prevista: serve a predictWait, perché i frame che arrivano dietro al
	// profilo vedano l'attesa giusta.
	service, known := d.estimate(key{job.ModelKey, job.SplitPoint})
	e := &entry{job: job, arrived: d.now(), service: service, known: known, granted: make(chan struct{})}
	d.queue = append(d.queue, e)
	d.dispatchLocked(d.now())
	d.mu.Unlock()

	select {
	case <-e.granted:
	case <-ctx.Done():
		d.mu.Lock()
		if e.started.IsZero() {
			d.queue = slices.DeleteFunc(d.queue, func(q *entry) bool { return q == e })
		} else {
			delete(d.running, e)
			d.dispatchLocked(d.now())
		}
		d.mu.Unlock()
		return nil, 0, ctx.Err()
	}

	out, err := d.backend.Execute(ctx, job)
	elapsed := d.now().Sub(e.started)

	d.mu.Lock()
	delete(d.running, e)
	d.totals.Profiled++
	d.dispatchLocked(d.now())
	d.mu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	if len(out.LayerMS) == 0 {
		return nil, 0, fmt.Errorf("worker returned no layer times")
	}
	return out.LayerMS, elapsed, nil
}
