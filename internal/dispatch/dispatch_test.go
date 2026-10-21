package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/stream"
)

// --- backend finto ------------------------------------------------------------------

// fakeBackend registra l'ordine dei job (Tensor[0] = id) e il massimo di job
// in esecuzione insieme. Con hold non nil ogni job aspetta che il test lo
// liberi (un valore sul canale = un job terminato).
type fakeBackend struct {
	slots  atomic.Int32
	hold   chan struct{}
	err    error
	layers []float64 // layer_ms restituiti; nil = []float64{1}

	mu         sync.Mutex
	order      []byte
	running    int
	maxRunning int
}

func newBackend(slots int, hold bool) *fakeBackend {
	b := &fakeBackend{}
	b.slots.Store(int32(slots))
	if hold {
		b.hold = make(chan struct{})
	}
	return b
}

func (b *fakeBackend) Slots() int { return int(b.slots.Load()) }

func (b *fakeBackend) Execute(ctx context.Context, job stream.Job) (stream.Outcome, error) {
	b.mu.Lock()
	b.order = append(b.order, job.Tensor[0])
	b.running++
	b.maxRunning = max(b.maxRunning, b.running)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.running--
		b.mu.Unlock()
	}()

	if b.hold != nil {
		select {
		case <-b.hold:
		case <-ctx.Done():
			return stream.Outcome{}, ctx.Err()
		}
	}
	if b.err != nil {
		return stream.Outcome{}, b.err
	}
	layers := b.layers
	if layers == nil {
		layers = []float64{1}
	}
	return stream.Outcome{LayerMS: layers, QueueWaitMS: 0.25}, nil
}

func (b *fakeBackend) seen() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.order...)
}

// --- helper -------------------------------------------------------------------------

func job(id byte, deadline time.Duration) stream.Job {
	return stream.Job{ModelKey: "yolo", SplitPoint: 4, Tensor: []byte{id}, Deadline: time.Now().Add(deadline)}
}

// seed imposta S come se fosse già stata misurata.
func seed(d *Dispatcher, s time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.service[key{"yolo", 4}] = s
}

type result struct {
	out stream.Outcome
	err error
}

// submit esegue il job in una goroutine; il risultato arriva sul canale.
func submit(d *Dispatcher, j stream.Job) <-chan result {
	ch := make(chan result, 1)
	go func() {
		out, err := d.Execute(context.Background(), j)
		ch <- result{out, err}
	}()
	return ch
}

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func queued(d *Dispatcher) int  { return d.Snapshot().Queued }
func running(d *Dispatcher) int { return d.Snapshot().Running }

// --- funzioni pure --------------------------------------------------------------------

func TestPredictWait(t *testing.T) {
	ms := func(v ...int) []time.Duration {
		out := make([]time.Duration, len(v))
		for i, x := range v {
			out[i] = time.Duration(x) * time.Millisecond
		}
		return out
	}
	cases := []struct {
		name            string
		slots           int
		running, queued []time.Duration
		want            int // ms
	}{
		{"tutto libero", 2, nil, nil, 0},
		{"uno slot libero", 2, ms(5), nil, 0},
		{"un solo slot occupato", 1, ms(5), nil, 5},
		{"esempio del commento", 2, ms(3, 8), ms(5), 8},
		// Slot liberi a 3, 8 → il primo in coda: 8, 8 → il secondo: 13, 8.
		{"coda più lunga degli slot", 2, ms(3, 8), ms(5, 5), 8},
		// Stessa S per tutti: coincide con la formula della specifica,
		// (queued + running − c + 1) / c × S = (3+2−2+1)/2 × 10 = 20.
		{"caso omogeneo", 2, ms(10, 10), ms(10, 10, 10), 20},
		{"slot diminuiti dopo un riavvio", 1, ms(4, 9), nil, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := predictWait(tc.slots, tc.running, tc.queued)
			if want := time.Duration(tc.want) * time.Millisecond; got != want {
				t.Errorf("predictWait = %v, want %v", got, want)
			}
		})
	}
}

func TestServiceEWMA(t *testing.T) {
	d := New(newBackend(1, false), Config{ServiceEWMAAlpha: 0.5, Window: time.Second})
	k := key{"yolo", 4}
	d.observeLocked(k, 10*time.Millisecond)
	if d.service[k] != 10*time.Millisecond {
		t.Errorf("primo campione: S = %v, attesa 10ms", d.service[k])
	}
	d.observeLocked(k, 20*time.Millisecond)
	if d.service[k] != 15*time.Millisecond {
		t.Errorf("secondo campione: S = %v, attesa 15ms", d.service[k])
	}
}

func TestEstimateUsesTheLongestKnownForUnknownKeys(t *testing.T) {
	d := New(newBackend(1, false), DefaultConfig())
	if s, known := d.estimate(key{"yolo", 4}); s != 0 || known {
		t.Errorf("senza misure: %v, %v", s, known)
	}
	d.service[key{"yolo", 2}] = 30 * time.Millisecond
	d.service[key{"fomo", 0}] = 5 * time.Millisecond
	if s, known := d.estimate(key{"yolo", 4}); s != 30*time.Millisecond || known {
		t.Errorf("key sconosciuto: %v, %v", s, known)
	}
}

func TestWindow(t *testing.T) {
	w := newWindow(3 * time.Second)
	t0 := time.Unix(1000, 0)
	w.add(t0, bucket{offered: 1, admitted: 1, busy: time.Millisecond})
	w.add(t0.Add(500*time.Millisecond), bucket{offered: 1})
	w.add(t0.Add(2*time.Second), bucket{offered: 2, busy: 2 * time.Millisecond})

	if s := w.sum(t0.Add(2 * time.Second)); s.offered != 4 || s.admitted != 1 || s.busy != 3*time.Millisecond {
		t.Errorf("somma = %+v", s)
	}
	// Al secondo 1003 il secondo 1000 è uscito dalla finestra (1001..1003).
	if s := w.sum(t0.Add(3 * time.Second)); s.offered != 2 {
		t.Errorf("dopo 3 s: offered = %d, attesi 2", s.offered)
	}
	// Il secondo 1003 riusa il bucket del 1000: va azzerato, non sommato.
	w.add(t0.Add(3*time.Second), bucket{offered: 1})
	if s := w.sum(t0.Add(3 * time.Second)); s.offered != 3 {
		t.Errorf("bucket riusato: offered = %d, attesi 3", s.offered)
	}
	if s := w.sum(t0.Add(time.Hour)); s.offered != 0 {
		t.Errorf("molto dopo: %+v", s)
	}
}

// --- ammissione e coda -------------------------------------------------------------------

func TestFirstJobRunsWithoutMeasures(t *testing.T) {
	b := newBackend(1, false)
	d := New(b, DefaultConfig())
	out, err := d.Execute(context.Background(), job(1, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if out.QueueWaitMS < 0.25 {
		t.Errorf("QueueWaitMS = %v: deve includere l'attesa del worker", out.QueueWaitMS)
	}
	snap := d.Snapshot()
	if snap.Totals.Completed != 1 || len(snap.Service) != 1 || snap.Service[0].SplitPoint != 4 {
		t.Errorf("snapshot = %+v", snap)
	}
}

// runQueued accoda n job in ordine (ognuno entra prima del successivo), poi
// li libera tutti e controlla che terminino senza errori.
func runQueued(t *testing.T, b *fakeBackend, d *Dispatcher, n int) {
	t.Helper()
	var results []<-chan result
	for i := range byte(n) {
		results = append(results, submit(d, job(i, 10*time.Second)))
		eventually(t, "job ammesso", func() bool { return running(d)+queued(d) == int(i)+1 })
	}
	for range n {
		b.hold <- struct{}{}
	}
	for _, r := range results {
		if res := <-r; res.err != nil {
			t.Errorf("err = %v", res.err)
		}
	}
}

func TestNeverMoreJobsThanSlots(t *testing.T) {
	b := newBackend(2, true)
	d := New(b, DefaultConfig())
	seed(d, time.Millisecond)
	runQueued(t, b, d, 6)
	if b.maxRunning != 2 {
		t.Errorf("max in esecuzione = %d, slot 2", b.maxRunning)
	}
}

func TestFIFO(t *testing.T) {
	// Con un solo slot un job entra nel backend solo dopo che il precedente
	// è terminato: l'ordine osservato è quello delle assegnazioni. Con più
	// slot due job sbloccati insieme possono entrare in ordine invertito per
	// lo scheduler, non per il dispatcher.
	b := newBackend(1, true)
	d := New(b, DefaultConfig())
	seed(d, time.Millisecond)
	runQueued(t, b, d, 6)
	if got := b.seen(); string(got) != string([]byte{0, 1, 2, 3, 4, 5}) {
		t.Errorf("ordine = %v, atteso FIFO", got)
	}
}

func TestRejectWhenPredictedLate(t *testing.T) {
	b := newBackend(1, true)
	d := New(b, DefaultConfig())
	seed(d, 100*time.Millisecond)

	first := submit(d, job(1, 10*time.Second))
	eventually(t, "primo in esecuzione", func() bool { return running(d) == 1 })

	_, err := d.Execute(context.Background(), job(2, 50*time.Millisecond))
	if !errors.Is(err, stream.ErrBusy) {
		t.Fatalf("err = %v, atteso ErrBusy", err)
	}
	if !strings.Contains(err.Error(), "predicted") || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("il messaggio deve spiegare il rifiuto con i numeri: %q", err)
	}
	if n := d.Snapshot().Totals.Rejected; n != 1 {
		t.Errorf("Rejected = %d", n)
	}
	b.hold <- struct{}{}
	<-first
}

func TestUnknownServiceNeedsAFreeSlot(t *testing.T) {
	b := newBackend(1, true)
	d := New(b, DefaultConfig())

	first := submit(d, job(1, 10*time.Second))
	eventually(t, "primo in esecuzione", func() bool { return running(d) == 1 })
	if _, err := d.Execute(context.Background(), job(2, 10*time.Second)); !errors.Is(err, stream.ErrBusy) {
		t.Errorf("senza misure e senza slot libero: err = %v, atteso ErrBusy", err)
	}
	b.hold <- struct{}{}
	<-first
}

func TestExpiresInQueueWithoutRunning(t *testing.T) {
	b := newBackend(1, true)
	d := New(b, DefaultConfig())
	seed(d, time.Millisecond) // sottostima: il primo job durerà molto di più

	first := submit(d, job(1, 10*time.Second))
	eventually(t, "primo in esecuzione", func() bool { return running(d) == 1 })

	start := time.Now()
	_, err := d.Execute(context.Background(), job(2, 50*time.Millisecond))
	if !errors.Is(err, stream.ErrDeadlineExceeded) {
		t.Fatalf("err = %v, atteso ErrDeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("scaduto dopo %v: doveva uscire alla scadenza", elapsed)
	}
	snap := d.Snapshot()
	if snap.Queued != 0 || snap.Totals.Expired != 1 {
		t.Errorf("snapshot = %+v", snap)
	}
	b.hold <- struct{}{}
	<-first
	if got := b.seen(); len(got) != 1 {
		t.Errorf("il backend ha ricevuto %v: un job scaduto non va eseguito", got)
	}
}

func TestCanceledInQueue(t *testing.T) {
	b := newBackend(1, true)
	d := New(b, DefaultConfig())
	seed(d, time.Millisecond)
	first := submit(d, job(1, 10*time.Second))
	eventually(t, "primo in esecuzione", func() bool { return running(d) == 1 })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := d.Execute(ctx, job(2, 10*time.Second))
		done <- err
	}()
	eventually(t, "secondo in coda", func() bool { return queued(d) == 1 })
	cancel() // stream chiuso
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if n := queued(d); n != 0 {
		t.Errorf("Queued = %d: un job annullato deve uscire dalla coda", n)
	}
	b.hold <- struct{}{}
	<-first
}

func TestWorkerUnavailable(t *testing.T) {
	d := New(newBackend(0, false), DefaultConfig())
	if _, err := d.Execute(context.Background(), job(1, time.Second)); !errors.Is(err, stream.ErrWorkerUnavailable) {
		t.Errorf("err = %v", err)
	}
	snap := d.Snapshot()
	if snap.Totals.Unavailable != 1 || snap.Utilization != nil {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestFailedExecutionFreesTheSlotAndIsNotMeasured(t *testing.T) {
	b := newBackend(1, false)
	b.err = errors.New("boom")
	d := New(b, DefaultConfig())
	if _, err := d.Execute(context.Background(), job(1, time.Second)); err == nil {
		t.Fatal("atteso un errore")
	}
	snap := d.Snapshot()
	if snap.Totals.Failed != 1 || len(snap.Service) != 0 || snap.Running != 0 {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestSnapshotRates(t *testing.T) {
	d := New(newBackend(2, false), Config{ServiceEWMAAlpha: 0.2, Window: 2 * time.Second})
	for i := range byte(4) {
		if _, err := d.Execute(context.Background(), job(i, time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	snap := d.Snapshot()
	if snap.Slots != 2 || snap.WindowSeconds != 2 || snap.OfferedPerSec != 2 || snap.AdmittedPerSec != 2 {
		t.Errorf("snapshot = %+v", snap)
	}
	if snap.Utilization == nil || *snap.Utilization < 0 || *snap.Utilization > 1 {
		t.Errorf("Utilization = %v", snap.Utilization)
	}
}

func TestConcurrentLoad(t *testing.T) {
	// Per -race: molti device insieme, alcuni rifiutati o scaduti. Alla fine
	// coda e slot devono essere vuoti e i conti devono tornare.
	b := newBackend(3, false)
	d := New(b, DefaultConfig())
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() {
			d.Execute(context.Background(), job(byte(i), time.Duration(i%20)*time.Millisecond))
		})
	}
	wg.Wait()
	snap := d.Snapshot()
	tot := snap.Totals
	if snap.Running != 0 || snap.Queued != 0 {
		t.Errorf("snapshot = %+v", snap)
	}
	if sum := tot.Rejected + tot.Expired + tot.Completed + tot.Failed; sum != 200 {
		t.Errorf("totali = %+v: la somma deve essere 200", tot)
	}
	if b.maxRunning > 3 {
		t.Errorf("max in esecuzione = %d, slot 3", b.maxRunning)
	}
}

// --- stima per segmento ------------------------------------------------------------------

const msD = time.Millisecond

func TestObserveSegments(t *testing.T) {
	d := New(newBackend(1, false), Config{ServiceEWMAAlpha: 0.5, Window: time.Second})
	// split 4, il worker esegue i segmenti 5, 6, 7.
	d.observeSegmentsLocked("yolo", 4, []float64{1, 2, 3.5}, 10*msD)

	want := map[segKey]time.Duration{{"yolo", 5}: msD, {"yolo", 6}: 2 * msD, {"yolo", 7}: 3500 * time.Microsecond}
	for k, w := range want {
		if got := d.segments[k]; got != w {
			t.Errorf("segmento %d = %v, want %v (i decimali di layer_ms non vanno persi)", k.index, got, w)
		}
	}
	if d.last["yolo"] != 7 {
		t.Errorf("ultimo segmento = %d, atteso 7", d.last["yolo"])
	}
	if got := d.overhead["yolo"]; got != 3500*time.Microsecond {
		t.Errorf("overhead = %v, atteso 10 − 6.5 = 3.5ms", got)
	}

	// Secondo campione: EWMA anche sui segmenti (alpha 0.5).
	d.observeSegmentsLocked("yolo", 6, []float64{5.5}, 6*msD)
	if got := d.segments[segKey{"yolo", 7}]; got != 4500*time.Microsecond {
		t.Errorf("segmento 7 dopo due campioni = %v, atteso 4.5ms", got)
	}
}

func TestObserveSegmentsOverheadNeverNegative(t *testing.T) {
	d := New(newBackend(1, false), DefaultConfig())
	// Orologi diversi: Go misura meno della somma di Python.
	d.observeSegmentsLocked("yolo", 4, []float64{3, 3}, 5*msD)
	if got := d.overhead["yolo"]; got != 0 {
		t.Errorf("overhead = %v, atteso 0", got)
	}
}

func TestEstimateFromSegments(t *testing.T) {
	d := New(newBackend(1, false), DefaultConfig())
	d.observeLocked(key{"yolo", 4}, 10*msD)
	d.observeSegmentsLocked("yolo", 4, []float64{1, 2, 3}, 10*msD) // overhead 4

	// Split 5 mai usato: segmenti 6 e 7 noti → 2 + 3 + 4.
	if s, known := d.estimate(key{"yolo", 5}); s != 9*msD || !known {
		t.Errorf("split 5 = %v, %v; atteso 9ms ricavato dai segmenti", s, known)
	}
	// Split 3: il segmento 4 non è mai stato eseguito → ripiego prudente.
	if s, known := d.estimate(key{"yolo", 3}); s != 10*msD || known {
		t.Errorf("split 3 = %v, %v; atteso il ripiego (10ms, non noto)", s, known)
	}
	// Split 7 = ultimo segmento: nulla da eseguire sul server.
	if _, ok := d.fromSegments(key{"yolo", 7}); ok {
		t.Error("split = ultimo segmento non è un job valido")
	}
	// Modello mai eseguito.
	if _, ok := d.fromSegments(key{"fomo", 0}); ok {
		t.Error("modello sconosciuto: niente stima dai segmenti")
	}
}

func TestDirectMeasureBeatsSegments(t *testing.T) {
	d := New(newBackend(1, false), DefaultConfig())
	d.observeSegmentsLocked("yolo", 4, []float64{1, 2, 3}, 10*msD)
	d.observeLocked(key{"yolo", 5}, 20*msD) // misura diretta, diversa dalla somma (9ms)
	if s, _ := d.estimate(key{"yolo", 5}); s != 20*msD {
		t.Errorf("S = %v: la misura diretta ha la precedenza", s)
	}
}

func TestSegmentsLearnedFromExecute(t *testing.T) {
	b := newBackend(1, false)
	b.layers = []float64{1, 2, 3}
	d := New(b, DefaultConfig())
	if _, err := d.Execute(context.Background(), job(1, time.Second)); err != nil {
		t.Fatal(err)
	}
	snap := d.Snapshot()
	if len(snap.Segments) != 3 || snap.Segments[0].Index != 5 || snap.Segments[2].Index != 7 {
		t.Errorf("segments = %+v", snap.Segments)
	}
	if len(snap.Overhead) != 1 || snap.Overhead[0].ModelKey != "yolo" {
		t.Errorf("overhead = %+v", snap.Overhead)
	}
	// Uno split mai usato ora è ammesso anche con la coda piena.
	if _, known := d.estimate(key{"yolo", 6}); !known {
		t.Error("split 6 deve essere stimabile dai segmenti")
	}
}

// --- previsione per la registrazione --------------------------------------------

func TestOutcomeCarriesTheAdmission(t *testing.T) {
	d := New(newBackend(1, false), DefaultConfig())
	seed(d, 8*time.Millisecond)
	out, err := d.Execute(context.Background(), job(1, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	a := out.Admission
	if a == nil || a.PredictedWait != 0 || a.Service != 8*time.Millisecond || !a.Known {
		t.Errorf("Admission = %+v", a)
	}
}

func TestRejectionCarriesTheAdmission(t *testing.T) {
	b := newBackend(1, true)
	d := New(b, DefaultConfig())
	seed(d, 100*time.Millisecond)
	first := submit(d, job(1, 10*time.Second))
	eventually(t, "primo in esecuzione", func() bool { return running(d) == 1 })

	_, err := d.Execute(context.Background(), job(2, 50*time.Millisecond))
	var je *stream.JobError
	if !errors.As(err, &je) || !errors.Is(err, stream.ErrBusy) {
		t.Fatalf("err = %v: atteso un *stream.JobError che resta un ErrBusy", err)
	}
	if je.Admission.Service != 100*time.Millisecond || je.Admission.PredictedWait <= 0 || !je.Admission.Known {
		t.Errorf("Admission = %+v", je.Admission)
	}
	if !strings.Contains(err.Error(), "predicted wait") {
		t.Errorf("il messaggio per il device non deve cambiare: %q", err)
	}
	b.hold <- struct{}{}
	<-first
}

func TestUnknownServiceRejectionCarriesTheAdmission(t *testing.T) {
	b := newBackend(1, true)
	d := New(b, DefaultConfig())
	first := submit(d, job(1, 10*time.Second))
	eventually(t, "primo in esecuzione", func() bool { return running(d) == 1 })

	_, err := d.Execute(context.Background(), job(2, 10*time.Second))
	var je *stream.JobError
	if !errors.As(err, &je) || je.Admission.Known {
		t.Errorf("err = %v, admission %+v: senza misure Known è false", err, je)
	}
	b.hold <- struct{}{}
	<-first
}

func TestExpiryCarriesTheAdmission(t *testing.T) {
	b := newBackend(1, true)
	d := New(b, DefaultConfig())
	seed(d, time.Millisecond) // sottostima: il frame scadrà in coda
	first := submit(d, job(1, 10*time.Second))
	eventually(t, "primo in esecuzione", func() bool { return running(d) == 1 })

	_, err := d.Execute(context.Background(), job(2, 50*time.Millisecond))
	var je *stream.JobError
	if !errors.As(err, &je) || !errors.Is(err, stream.ErrDeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	// La previsione era 1 ms di attesa: l'attesa vera ha superato i 50 ms.
	if je.Admission.Service != time.Millisecond || je.Admission.PredictedWait > 10*time.Millisecond {
		t.Errorf("Admission = %+v", je.Admission)
	}
	b.hold <- struct{}{}
	<-first
}

func TestExecutionErrorCarriesTheAdmission(t *testing.T) {
	// Il frame è stato ammesso, poi la scadenza è passata nel worker.
	b := newBackend(1, false)
	b.err = fmt.Errorf("%w: rpc deadline", stream.ErrDeadlineExceeded)
	d := New(b, DefaultConfig())
	seed(d, 8*time.Millisecond)

	_, err := d.Execute(context.Background(), job(1, time.Second))
	var je *stream.JobError
	if !errors.As(err, &je) || !errors.Is(err, stream.ErrDeadlineExceeded) {
		t.Fatalf("err = %v: atteso un *stream.JobError che resta un ErrDeadlineExceeded", err)
	}
	if je.Admission.Service != 8*time.Millisecond || !je.Admission.Known {
		t.Errorf("Admission = %+v", je.Admission)
	}
	if !strings.Contains(err.Error(), "rpc deadline") {
		t.Errorf("il messaggio originale va conservato: %q", err)
	}
}
