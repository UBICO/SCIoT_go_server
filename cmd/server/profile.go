package main

import (
	"context"
	"log"
	"slices"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/dispatch"
	"github.com/UBICO/SCIoT_go_server/internal/record"
	"github.com/UBICO/SCIoT_go_server/internal/worker"
)

// profileLoop profila ogni modello del catalogo a ogni segnale su ready (il
// worker è diventato pronto), finché ctx non viene cancellato. rec nil =
// registrazione spenta.
//
// I segnali arrivano dal supervisore, che non deve bloccarsi: ready ha
// buffer 1 e chi segnala non aspetta (vedi signalReady). Due riavvii mentre
// un profilo è in corso diventano un solo profilo dopo.
func profileLoop(ctx context.Context, d *dispatch.Dispatcher, catalog *worker.Catalog, ready <-chan struct{}, runs int, rec *record.Recorder) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ready:
		}
		for _, model := range catalog.Models() {
			sizes, _ := catalog.Segments(model)
			i := slices.IndexFunc(sizes, func(s api.SegmentSize) bool { return s.SplitPoint == -1 })
			if i < 0 {
				continue // il catalogo è cambiato nel frattempo (worker di nuovo giù)
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
	}
}

// signalReady segnala ready senza bloccare: se un segnale è già in attesa,
// basta quello.
func signalReady(ready chan<- struct{}) {
	select {
	case ready <- struct{}{}:
	default:
	}
}
