package main

import (
	"context"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/device"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

// runtimeLoop registra un evento runtime subito (il livello di partenza) e poi
// ogni every, finché ctx non viene cancellato. Parte solo con la
// registrazione accesa (rec non nil).
func runtimeLoop(ctx context.Context, rec *record.Recorder, devices *device.Store, every time.Duration) {
	sample := func() {
		r := record.SampleRuntime()
		r.Devices = devices.Len()
		rec.Record(&r)
	}
	// Un campione subito: il primo punto è "prima del carico", e un run lungo
	// si legge come pendenza da lì.
	sample()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sample()
		}
	}
}
