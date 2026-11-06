package server

import (
	"sync"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/device"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

// decider decide le policy dei device in una goroutine sola, un device alla
// volta, fuori dal gestore dell'heartbeat (Fase 3.8): con N device una
// decisione costa ~0.5 ms per ogni altro device in SPLIT, e il device non
// deve aspettarla. Uno alla volta mantiene la garanzia della 3.7: due device
// non decidono sullo stesso server libero.
//
// La coda non ha doppioni: un device già in attesa che manda un heartbeat
// nuovo non entra due volte, si sostituisce il suo heartbeat con quello più
// recente. La coda non supera mai il numero di device, e si decide sempre
// sull'ultimo stato. L'ordine è quello del primo heartbeat in attesa (FIFO):
// nessun device resta indietro.
type decider struct {
	run func(decisionJob) // la decisione di un device (Server.decideNow)

	mu      sync.Mutex
	pending map[string]decisionJob // device → l'heartbeat più recente non ancora deciso
	order   []string               // i device di pending, in ordine di arrivo

	wake chan struct{} // capacità 1: "c'è qualcosa in coda"
	stop chan struct{} // chiuso da close
	done chan struct{} // chiuso dalla goroutine quando esce

	// busy conta i device in coda o in decisione: wait aspetta che torni a
	// zero (i test, e la chiusura ordinata).
	busy sync.WaitGroup
}

// decisionJob è un heartbeat da decidere.
type decisionJob struct {
	deviceID, sessionID string
	hb                  *record.Heartbeat
	res                 device.HeartbeatResult
	at                  time.Time // arrivo dell'heartbeat: il ritardo della decisione (lag_ms)
}

// newDecider crea il decider e avvia la sua goroutine.
func newDecider(run func(decisionJob)) *decider {
	d := &decider{
		run:     run,
		pending: map[string]decisionJob{},
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go d.loop()
	return d
}

// submit mette in coda l'heartbeat j. Non blocca mai: il gestore
// dell'heartbeat risponde subito.
func (d *decider) submit(j decisionJob) {
	d.mu.Lock()
	if _, queued := d.pending[j.deviceID]; !queued {
		d.order = append(d.order, j.deviceID)
		d.busy.Add(1)
	}
	d.pending[j.deviceID] = j
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default: // c'è già un segnale in attesa
	}
}

// next toglie dalla coda il primo device e restituisce il suo heartbeat più
// recente; ok = false con la coda vuota.
func (d *decider) next() (j decisionJob, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.order) == 0 {
		return decisionJob{}, false
	}
	id := d.order[0]
	d.order = d.order[1:]
	j = d.pending[id]
	delete(d.pending, id)
	return j, true
}

// loop è la goroutine: a ogni sveglia decide i device in coda, uno alla
// volta, finché la coda è vuota; esce quando stop viene chiuso.
func (d *decider) loop() {
	defer close(d.done)
	for {
		select {
		case <-d.stop:
			return
		case <-d.wake:
			for j, ok := d.next(); ok; j, ok = d.next() {
				d.run(j) // fuori dal lock: submit può mettere in coda mentre si decide
				d.busy.Done()
			}
		}
	}
}

// wait aspetta che i device in coda siano stati decisi.
func (d *decider) wait() { d.busy.Wait() }

// close ferma la goroutine e aspetta che esca: dopo close nessuna decisione
// scrive più (la registrazione si può chiudere).
func (d *decider) close() {
	close(d.stop)
	<-d.done
}
