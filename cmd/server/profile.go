package main

import (
	"context"
	"log"
	"slices"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/dispatch"
	"github.com/UBICO/SCIoT_go_server/internal/record"
	"github.com/UBICO/SCIoT_go_server/internal/worker"
)

// profileLoop profila ogni modello del catalogo a ogni segnale su ready (il
// worker è diventato pronto) e, con reprofileAge > 0, di nuovo i modelli con
// un segmento la cui stima è più vecchia di reprofileAge
// (Dispatcher.StaleModels), finché ctx non viene cancellato. rec nil =
// registrazione spenta.
//
// I segnali arrivano dal supervisore, che non deve bloccarsi: ready ha
// buffer 1 e chi segnala non aspetta (vedi signalReady). Due riavvii mentre
// un profilo è in corso diventano un solo profilo dopo.
func profileLoop(ctx context.Context, d *dispatch.Dispatcher, catalog *worker.Catalog, ready <-chan struct{}, runs int, reprofileAge time.Duration, rec *record.Recorder) {
	// Senza nuovo profilo il ticker non scatta mai (nil channel).
	var tick <-chan time.Time
	if reprofileAge > 0 {
		t := time.NewTicker(reprofileAge / 2) // un segmento invecchia al massimo di 1.5 × reprofileAge
		defer t.Stop()
		tick = t.C
	}
	for {
		var models []string
		select {
		case <-ctx.Done():
			return
		case <-ready:
			models = catalog.Models()
		case <-tick:
			models = d.StaleModels(reprofileAge)
		}
		for _, model := range models {
			profileModel(ctx, d, catalog, model, runs, rec)
		}
	}
}

// profileModel profila un modello del catalogo e registra il risultato.
func profileModel(ctx context.Context, d *dispatch.Dispatcher, catalog *worker.Catalog, model string, runs int, rec *record.Recorder) {
	sizes, _ := catalog.Segments(model)
	i := slices.IndexFunc(sizes, func(s api.SegmentSize) bool { return s.SplitPoint == -1 })
	if i < 0 {
		return // il catalogo è cambiato nel frattempo (worker di nuovo giù)
	}
	res, err := d.ProfileModel(ctx, model, sizes[i].InputBytes, runs)
	ev := &record.Profile{
		ModelKey: model, Runs: res.Runs, SegmentMS: res.SegmentMS,
		OverheadMS: res.OverheadMS, DurationMS: float64(res.Duration.Microseconds()) / 1000,
	}
	if err != nil {
		ev.Error = err.Error()
		log.Printf("profile %s: %v", model, err)
	} else {
		log.Printf("profile %s: %d runs in %.0f ms, overhead %.1f ms", model, res.Runs, ev.DurationMS, res.OverheadMS)
	}
	rec.Record(ev)
}

// signalReady segnala ready senza bloccare: se un segnale è già in attesa,
// basta quello.
func signalReady(ready chan<- struct{}) {
	select {
	case ready <- struct{}{}:
	default:
	}
}
