// Package dispatch sta tra lo stream e il worker: decide se un frame può
// essere servito entro la sua scadenza (ammissione), lo mette in coda finché
// uno slot del worker è libero e misura il servizio.
//
// La coda è in Go e non nel worker: qui si conoscono esattamente i frame in
// attesa e in esecuzione, quindi l'attesa prevista è un calcolo, non una stima
// di un sistema opaco. Il Dispatcher non manda mai al worker più job dei suoi
// slot: il worker non accoda (la sua queue_wait resta ~0).
//
// Disciplina FIFO, unica per tutti i device: la previsione fatta
// all'ammissione resta valida anche dopo, perché nessun frame arrivato più
// tardi può passare davanti. L'equità tra device la garantisce già
// max_inflight dello stream.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/stream"
)

// Backend è il worker visto dal dispatcher. *worker.Supervisor lo soddisfa.
type Backend interface {
	stream.Executor
	// Slots restituisce i job che il worker esegue in parallelo; 0 se non è
	// pronto.
	Slots() int
}

// Config contiene i parametri del dispatcher.
type Config struct {
	// ServiceEWMAAlpha è il peso del campione nuovo nella media del tempo di
	// servizio S, in (0, 1]. Più alto = segue prima un cambiamento (carico
	// della CPU, modello più lento), ma è più rumoroso.
	ServiceEWMAAlpha float64
	// Window è la finestra su cui si misurano λ e ρ.
	Window time.Duration
}

// DefaultConfig restituisce i valori di partenza.
func DefaultConfig() Config {
	return Config{ServiceEWMAAlpha: 0.2, Window: 10 * time.Second}
}

// key identifica un tipo di job: S dipende dal modello e da quanti layer
// restano da eseguire, cioè dallo split point.
type key struct {
	model string
	split int
}

// segKey identifica un segmento di un modello: il tempo di invoke() di un
// segmento non dipende da quale split point lo ha raggiunto.
type segKey struct {
	model string
	index int
}

// entry è un job ammesso: in coda o in esecuzione.
type entry struct {
	job     stream.Job
	arrived time.Time
	service time.Duration // S previsto all'ammissione
	wait    time.Duration // attesa in coda prevista all'ammissione
	known   bool          // S viene da misure (di questo key o dei suoi segmenti)

	granted chan struct{} // chiuso quando il job ottiene uno slot
	started time.Time     // quando ha ottenuto lo slot; zero = in coda
}

// admission è la previsione fatta quando e è stato ammesso.
func (e *entry) admission() *stream.Admission {
	return &stream.Admission{PredictedWait: e.wait, Service: e.service, Known: e.known}
}

// Dispatcher implementa stream.Executor.
type Dispatcher struct {
	backend Backend
	cfg     Config
	now     func() time.Time

	// mu protegge tutto ciò che segue. Ordine dei lock: mu, poi i lock del
	// backend (Slots); mai il contrario.
	mu      sync.Mutex
	queue   []*entry            // in attesa di uno slot, in ordine di arrivo
	running map[*entry]struct{} // in esecuzione nel worker
	service map[key]time.Duration

	// Stima per segmento: permette di prevedere S anche per uno split point
	// mai usato, se i suoi segmenti sono già stati eseguiti per altri split.
	segments map[segKey]time.Duration // EWMA di layer_ms per segmento
	overhead map[string]time.Duration // EWMA per modello di (S misurata − Σ layer_ms)
	last     map[string]int           // indice dell'ultimo segmento di ogni modello

	window *window
	totals Totals
}

var _ stream.Executor = (*Dispatcher)(nil)

// New crea un Dispatcher davanti a backend.
func New(backend Backend, cfg Config) *Dispatcher {
	return &Dispatcher{
		backend: backend,
		cfg:     cfg,
		now:     time.Now,
		running: map[*entry]struct{}{},
		service: map[key]time.Duration{},

		segments: map[segKey]time.Duration{},
		overhead: map[string]time.Duration{},
		last:     map[string]int{},

		window: newWindow(cfg.Window),
	}
}

// Execute implementa stream.Executor: ammissione, attesa di uno slot,
// esecuzione nel backend, misura.
func (d *Dispatcher) Execute(ctx context.Context, job stream.Job) (stream.Outcome, error) {
	arrived := d.now()
	e, err := d.admit(job, arrived)
	if err != nil {
		return stream.Outcome{}, err
	}
	if err := d.wait(ctx, e); err != nil {
		return stream.Outcome{}, err
	}

	out, err := d.backend.Execute(ctx, job)
	d.finish(e, out.LayerMS, err)
	if err != nil {
		return stream.Outcome{}, err
	}
	// L'attesa in coda qui si somma a quella (di solito nulla) nel worker.
	out.QueueWaitMS += ms(e.started.Sub(arrived))
	// La registrazione mette la previsione accanto all'attesa misurata.
	out.Admission = e.admission()
	return out, nil
}

// admit decide se il job può terminare entro la scadenza e, se sì, lo mette
// in coda o, con uno slot libero, lo avvia subito.
func (d *Dispatcher) admit(job stream.Job, now time.Time) (*entry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.window.add(now, bucket{offered: 1})
	slots := d.backend.Slots()
	if slots == 0 {
		d.totals.Unavailable++
		return nil, fmt.Errorf("%w: no worker ready", stream.ErrWorkerUnavailable)
	}

	k := key{job.ModelKey, job.SplitPoint}
	service, known := d.estimate(k)
	wait := predictWait(slots, d.remainingLocked(now), d.queuedLocked())

	// Il messaggio va al device così com'è: contiene i numeri della previsione.
	// I rifiuti sono avvolti in un *stream.JobError, così la registrazione
	// conosce la previsione anche dei frame rifiutati; messaggio e
	// errors.Is(err, stream.ErrBusy) non cambiano.
	prediction := stream.Admission{PredictedWait: wait, Service: service, Known: known}
	if finish := now.Add(wait + service); finish.After(job.Deadline) {
		d.totals.Rejected++
		return nil, &stream.JobError{Admission: prediction, Err: fmt.Errorf(
			"%w: predicted wait %.1f ms + service %.1f ms > deadline %.1f ms",
			stream.ErrBusy, ms(wait), ms(service), ms(job.Deadline.Sub(now)))}
	}
	// Senza misure non si può promettere nulla se non c'è uno slot libero. Non
	// basta wait > 0: senza misure anche i job in esecuzione hanno S = 0, e wait
	// risulterebbe 0. La prima esecuzione produce la prima misura.
	if !known && len(d.running)+len(d.queue) >= slots {
		d.totals.Rejected++
		return nil, &stream.JobError{Admission: prediction, Err: fmt.Errorf(
			"%w: no measured service time for model %q split %d and no free slot",
			stream.ErrBusy, job.ModelKey, job.SplitPoint)}
	}

	d.window.add(now, bucket{admitted: 1})
	e := &entry{job: job, arrived: now, service: service, wait: wait, known: known, granted: make(chan struct{})}
	d.queue = append(d.queue, e)
	// Lo avvia subito se c'è uno slot libero.
	d.dispatchLocked(now)
	return e, nil
}

// wait attende che e ottenga uno slot, oppure la scadenza o la chiusura
// dello stream.
func (d *Dispatcher) wait(ctx context.Context, e *entry) error {
	timer := time.NewTimer(e.job.Deadline.Sub(d.now()))
	defer timer.Stop()

	var err error
	select {
	case <-e.granted:
		// Lo slot può arrivare quando la scadenza è appena passata (il timer
		// non è ancora scattato): si controlla, per non eseguire un job il
		// cui risultato non serve più.
		if !d.now().After(e.job.Deadline) {
			return nil
		}
		err = stream.ErrDeadlineExceeded
	case <-timer.C:
		err = stream.ErrDeadlineExceeded
	case <-ctx.Done():
		err = ctx.Err() // stream chiuso: nessuno riceverà l'esito
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if e.started.IsZero() {
		// Ancora in coda: esce senza aver mai occupato uno slot.
		d.queue = slices.DeleteFunc(d.queue, func(q *entry) bool { return q == e })
	} else {
		// Lo slot è arrivato insieme alla scadenza: va restituito.
		delete(d.running, e)
		d.dispatchLocked(d.now())
	}
	if errors.Is(err, stream.ErrDeadlineExceeded) {
		// Succede solo se la previsione era sbagliata (S cresciuta dopo
		// l'ammissione): è un segnale utile, per questo ha un contatore.
		d.totals.Expired++
		// Con la previsione di e: è il caso più utile per la Fase 3, la
		// previsione era sbagliata e la registrazione deve dire di quanto.
		return &stream.JobError{Admission: *e.admission(), Err: fmt.Errorf(
			"%w: expired after %.1f ms in queue", stream.ErrDeadlineExceeded, ms(d.now().Sub(e.arrived)))}
	}
	return err
}

// finish libera lo slot di e, aggiorna S (del key e dei segmenti) e le
// metriche. layerMS è il tempo di ogni segmento eseguito dal worker.
func (d *Dispatcher) finish(e *entry, layerMS []float64, execErr error) {
	now := d.now()
	elapsed := now.Sub(e.started)

	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.running, e)
	d.window.add(now, bucket{busy: elapsed})
	if execErr != nil {
		// Un'esecuzione fallita (worker giù, scadenza durante il calcolo) non
		// misura il servizio: non entra in S.
		d.totals.Failed++
	} else {
		d.totals.Completed++
		d.observeLocked(key{e.job.ModelKey, e.job.SplitPoint}, elapsed)
		d.observeSegmentsLocked(e.job.ModelKey, e.job.SplitPoint, layerMS, elapsed)
	}
	d.dispatchLocked(now)
}

// dispatchLocked avvia i job in testa alla coda finché ci sono slot liberi.
// Va chiamata con d.mu acquisito ogni volta che si libera uno slot o entra
// un job.
func (d *Dispatcher) dispatchLocked(now time.Time) {
	slots := d.backend.Slots()
	for len(d.queue) > 0 && len(d.running) < slots {
		e := d.queue[0]
		d.queue = d.queue[1:]
		e.started = now
		d.running[e] = struct{}{}
		close(e.granted)
	}
}

// estimate restituisce S per k e se viene da misure. In ordine:
//
//  1. la media misurata per k: la stima migliore, perché include l'overhead
//     esatto per la dimensione del tensore di quello split point;
//  2. la somma dei segmenti da eseguire più l'overhead del modello, se tutti
//     quei segmenti sono già stati misurati (anche per altri split point);
//  3. la S più lunga tra quelle note (stima prudente: un job sconosciuto in
//     coda non deve far sembrare la coda più corta di quanto è); senza
//     nessuna misura, 0. Solo in questo caso known è false.
func (d *Dispatcher) estimate(k key) (time.Duration, bool) {
	if s, ok := d.service[k]; ok {
		return s, true
	}
	if s, ok := d.fromSegments(k); ok {
		return s, true
	}
	var longest time.Duration
	for _, s := range d.service {
		longest = max(longest, s)
	}
	return longest, false
}

// observeLocked aggiorna la media di S per k con un nuovo campione.
func (d *Dispatcher) observeLocked(k key, sample time.Duration) {
	updateEWMA(d.service, k, sample, d.cfg.ServiceEWMAAlpha)
}

// updateEWMA aggiorna la media mobile m[k] con un nuovo campione. È generica
// ([K comparable]): la stessa funzione serve per le mappe con chiave key,
// segKey e string. comparable = tipi utilizzabili come chiave di una mappa.
func updateEWMA[K comparable](m map[K]time.Duration, k K, sample time.Duration, alpha float64) {
	prev, ok := m[k]
	if !ok {
		// Primo campione: partire da 0 sottostimerebbe S per molti campioni e
		// farebbe ammettere troppo.
		m[k] = sample
		return
	}
	m[k] = time.Duration(alpha*float64(sample) + (1-alpha)*float64(prev))
}

// observeSegmentsLocked aggiorna i tempi dei segmenti eseguiti e l'overhead
// del modello.
//
// layerMS[j] è il segmento split+1+j (il worker esegue split+1 .. last, in
// ordine). L'overhead è ciò che S misura oltre ai segmenti: gRPC, copia del
// tensore, attesa nel worker.
func (d *Dispatcher) observeSegmentsLocked(model string, split int, layerMS []float64, elapsed time.Duration) {
	if len(layerMS) == 0 {
		return // backend che non misura i segmenti: niente da imparare
	}
	alpha := d.cfg.ServiceEWMAAlpha
	var sum time.Duration
	for j, v := range layerMS {
		// time.Duration(v) * time.Millisecond troncherebbe v a intero prima di
		// moltiplicare: si converte dopo.
		dur := time.Duration(v * float64(time.Millisecond))
		updateEWMA(d.segments, segKey{model, split + 1 + j}, dur, alpha)
		sum += dur
	}
	// Il worker esegue split+1 .. last: l'ultimo segmento eseguito è l'ultimo
	// del modello.
	d.last[model] = split + len(layerMS)
	// Mai sotto zero: le due misure vengono da orologi diversi (Go e Python).
	updateEWMA(d.overhead, model, max(0, elapsed-sum), alpha)
}

// fromSegments stima S per k come somma dei segmenti split+1 .. last più
// l'overhead del modello. ok = false se manca anche una sola misura.
//
// L'overhead del modello è una media su tutti gli split point, ma in parte
// dipende dalla dimensione del tensore (~1 ms/MB sul gRPC): per questo una
// misura diretta di k, quando c'è, ha la precedenza (vedi estimate).
func (d *Dispatcher) fromSegments(k key) (time.Duration, bool) {
	// Senza last il modello non è mai stato eseguito e non si sa quanti segmenti ha.
	last, ok := d.last[k.model]
	if !ok {
		return 0, false
	}
	// Con split >= last non c'è nessun segmento da eseguire sul server: non è
	// un job valido.
	if k.split >= last {
		return 0, false
	}
	var total time.Duration
	for i := k.split + 1; i <= last; i++ {
		s, ok := d.segments[segKey{k.model, i}]
		if !ok {
			return 0, false
		}
		total += s
	}
	overhead, ok := d.overhead[k.model]
	if !ok {
		return 0, false
	}
	return total + overhead, true
}

// remainingLocked restituisce il tempo di servizio residuo previsto di ogni
// job in esecuzione: S meno il tempo già trascorso, mai sotto zero (un job
// più lento della sua S finirà "a momenti").
func (d *Dispatcher) remainingLocked(now time.Time) []time.Duration {
	out := make([]time.Duration, 0, len(d.running))
	for e := range d.running {
		out = append(out, max(0, e.service-now.Sub(e.started)))
	}
	return out
}

// queuedLocked restituisce S dei job in coda, in ordine.
func (d *Dispatcher) queuedLocked() []time.Duration {
	out := make([]time.Duration, len(d.queue))
	for i, e := range d.queue {
		out[i] = e.service
	}
	return out
}

// predictWait restituisce quanto aspetterebbe in coda un job che arriva
// adesso, con disciplina FIFO e slots esecutori.
//
// È una simulazione deterministica: ogni slot ha un istante in cui si libera
// (il residuo di un job in esecuzione, 0 se è libero); ogni job in coda, in
// ordine, prende lo slot che si libera per primo e lo occupa per la sua S. Il
// nuovo job partirà quando si libera il primo slot dopo tutti questi.
//
//	slots 2, running [3ms 8ms], queued [5ms]
//	slot liberi a: 3, 8 → il job in coda prende lo slot a 3 → 8, 8 → attesa 8ms
func predictWait(slots int, running, queued []time.Duration) time.Duration {
	// free[i] è l'istante (relativo ad adesso) in cui lo slot i si libera. Se i
	// job in esecuzione sono più degli slot (diminuiti dopo un riavvio del
	// worker) si tengono i primi slots.
	free := make([]time.Duration, slots)
	copy(free, running)

	// argmin restituisce l'indice dello slot che si libera per primo.
	argmin := func() int {
		best := 0
		for i, f := range free {
			if f < free[best] {
				best = i
			}
		}
		return best
	}

	for _, s := range queued {
		free[argmin()] += s
	}
	return free[argmin()]
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
