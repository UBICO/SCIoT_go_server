package device

import (
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/ewma"
)

// Cycle è la stima di A, il tempo di un ciclo del device oltre al calcolo dei
// segmenti: acquisizione, consegna dei frame al socket (l'invio è in
// background), gestione delle risposte; con una camera anche l'attesa del
// frame. Non è nella telemetria: si ricava dalle finestre degli heartbeat,
//
//	A = Σ (durata − Σ segment_ms·segment_count − wait_ms) / Σ inference_count
//
// pesando le finestre sul tempo (internal/ewma, costante di tempo tau).
//
// Il rapporto delle somme, non la media delle A di ogni finestra (Fase 3.9):
// un frame finito sul device conta per intero nella finestra in cui finisce,
// anche se è iniziato in quella prima. Su un device lento (Raspberry Pi,
// ciclo di 658 ms con un frame locale da 636 ms, finestre di 1 s) la A di una
// finestra va da −43 a +143 ms, con la mediana sotto zero; sulle somme vale
// 15,8 ms e dà i frame/s misurati. Gli errori si compensano solo sommando.
//
// Il calcolo viene dai tempi per segmento e non da inference_time_ms, che
// non contiene i suffissi dei frame finiti sul device.
type Cycle struct {
	rest   ewma.Stat // ms per finestra: durata − calcolo (anche negativa)
	frames ewma.Stat // frame per finestra
}

// Update aggiunge la finestra di un heartbeat arrivato in at: telemetria t,
// durata gapMS (since_last_seen). Restituisce la stima nuova; una finestra
// che non dice nulla (nessun frame, durata nulla, segment_ms assenti o di
// lunghezza diversa da segment_count, un segmento eseguito senza tempo) la
// lascia com'è.
func (c Cycle) Update(t api.Telemetry, gapMS float64, at time.Time, tau time.Duration) Cycle {
	if t.InferenceCount <= 0 || gapMS <= 0 || len(t.SegmentMS) == 0 || len(t.SegmentMS) != len(t.SegmentCount) {
		return c
	}
	compute := 0.0
	for i, n := range t.SegmentCount {
		if n <= 0 {
			continue
		}
		if t.SegmentMS[i] == nil {
			return c
		}
		compute += *t.SegmentMS[i] * float64(n)
	}
	// Le attese del client per un posto nella finestra non sono A: la
	// simulazione le aggiunge da sé (sim.go). Assenti = 0.
	wait := 0.0
	if t.WaitMS != nil {
		wait = *t.WaitMS
	}
	// Nessun max(0, ·): gli scarti negativi compensano quelli positivi.
	// c è una copia (receiver per valore): il chiamante riceve la nuova dal return.
	c.rest.Update(at, gapMS-compute-wait, tau)
	c.frames.Update(at, float64(t.InferenceCount), tau)
	return c
}

// MS è A in millisecondi, mai sotto zero (finestra e tempi vengono da
// orologi diversi); ok = false senza finestre utili.
func (c Cycle) MS() (float64, bool) {
	if c.frames.N() == 0 || c.frames.Mean() <= 0 {
		return 0, false
	}
	return max(0, c.rest.Mean()/c.frames.Mean()), true
}
