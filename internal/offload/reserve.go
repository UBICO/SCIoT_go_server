package offload

import (
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
)

// Load è un carico sul server: frame al secondo ed erlang (Σ λ·S, S in
// secondi), come Queue.OthersLambda e Queue.OthersErlangs.
type Load struct {
	PerSec, Erlangs float64
}

// serverLoad è il carico che un device porta al server con la previsione p:
// i frame serviti al secondo per S. Zero in LOCAL.
func serverLoad(p costmodel.Prediction) Load {
	if p.Local {
		return Load{}
	}
	return Load{PerSec: p.ServerFramesPerSec, Erlangs: p.ServerFramesPerSec * p.ServiceMS / 1000}
}

// reservations sono i carichi che l'algoritmo ha concesso ai device e che il
// dispatcher non vede ancora tutti (Fase 3.7, docs/OFFLOAD.md).
//
// Il dispatcher misura λ e ρ su una finestra scorrevole di window
// (dispatch.Config.Window, 10 s): un carico che parte all'istante t0 è visto
// per una frazione min(1, (t − t0)/window). Una prenotazione di età a pesa
// quindi load·max(0, 1 − a/window), quello che la misura non contiene
// ancora: il carico non si conta né due volte né mai.
//
// Solo le prenotazioni positive: chi riduce il proprio carico lo libera nella
// misura con lo stesso ritardo, e fino ad allora gli altri sono solo più
// prudenti.
//
// Non è sicura da più goroutine: Latency la usa sotto il proprio lock.
type reservations struct {
	window   time.Duration
	byDevice map[string]reservation
}

type reservation struct {
	load Load
	at   time.Time
}

func newReservations(window time.Duration) reservations {
	return reservations{window: window, byDevice: map[string]reservation{}}
}

// unseen è la parte di r che la misura non vede ancora all'istante now.
func (r *reservations) unseen(res reservation, now time.Time) Load {
	if r.window <= 0 {
		return Load{}
	}
	age := max(0, now.Sub(res.at))
	f := max(0, 1-float64(age)/float64(r.window))
	return Load{PerSec: res.load.PerSec * f, Erlangs: res.load.Erlangs * f}
}

// pending è il carico prenotato da tutti i device tranne except e non ancora
// visibile all'istante now: la somma di unseen.
func (r *reservations) pending(now time.Time, except string) Load {
	var sum Load
	for id, res := range r.byDevice {
		if id == except {
			continue
		}
		u := r.unseen(res, now)
		sum.PerSec += u.PerSec
		sum.Erlangs += u.Erlangs
	}
	return sum
}

// reserve prenota delta per il device all'istante now, se delta.Erlangs > 0.
// Una prenotazione precedente dello stesso device non ancora vista non va
// persa: la nuova è la sua parte non vista più delta, da now.
func (r *reservations) reserve(now time.Time, deviceID string, delta Load) {
	if delta.Erlangs <= 0 {
		return
	}
	if old, ok := r.byDevice[deviceID]; ok {
		u := r.unseen(old, now)
		delta.PerSec += u.PerSec
		delta.Erlangs += u.Erlangs
	}
	r.byDevice[deviceID] = reservation{load: delta, at: now}
}

// forget dimentica le prenotazioni del device (nuova sessione).
func (r *reservations) forget(deviceID string) {
	delete(r.byDevice, deviceID)
}
