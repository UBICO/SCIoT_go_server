package load

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReader restituisce valori decisi dal test. cpu è una coda: ogni
// chiamata a CPUPercent consuma il primo elemento.
type fakeReader struct {
	mu       sync.Mutex
	cpu      []float64
	cpuErr   error
	count    int
	countErr error
	load1    float64
	loadErr  error
	memTotal uint64
	memAvail uint64
	memErr   error
	calls    int
}

func (f *fakeReader) CPUPercent() (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.cpuErr != nil {
		return 0, f.cpuErr
	}
	if len(f.cpu) == 0 {
		return 0, errNoValue
	}
	v := f.cpu[0]
	f.cpu = f.cpu[1:]
	return v, nil
}
func (f *fakeReader) CPUCount() (int, error)          { return f.count, f.countErr }
func (f *fakeReader) LoadAvg1() (float64, error)      { return f.load1, f.loadErr }
func (f *fakeReader) Memory() (uint64, uint64, error) { return f.memTotal, f.memAvail, f.memErr }
func (f *fakeReader) callCount() int                  { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }
func (f *fakeReader) setCPUErr(err error)             { f.mu.Lock(); defer f.mu.Unlock(); f.cpuErr = err }
func (f *fakeReader) pushCPU(v ...float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cpu = append(f.cpu, v...)
}

var t0 = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

func newTestMonitor(r Reader) *Monitor {
	m := NewMonitor(r, Config{Interval: 10 * time.Millisecond, CPUEWMAAlpha: 0.5})
	m.now = func() time.Time { return t0 }
	m.goroutines = func() int { return 7 }
	return m
}

func fullReader() *fakeReader {
	return &fakeReader{cpu: []float64{40}, count: 10, load1: 2.5, memTotal: 16 << 30, memAvail: 8 << 30}
}

// --- ewma ------------------------------------------------------------------------

func TestEWMA(t *testing.T) {
	if got := ewma(nil, 40, 0.35); got != 40 {
		t.Errorf("primo campione: ewma = %v, want 40", got)
	}
	prev := 40.0
	// 0.5*80 + 0.5*40 = 60
	if got := ewma(&prev, 80, 0.5); got != 60 {
		t.Errorf("ewma = %v, want 60", got)
	}
	if got := ewma(&prev, 80, 1); got != 80 {
		t.Errorf("alpha 1 = solo il campione nuovo: ewma = %v", got)
	}
}

// --- Sample ----------------------------------------------------------------------

func TestSampleReadsEveryMetric(t *testing.T) {
	m := newTestMonitor(fullReader())

	s := m.Sample()

	if s.CPUPercent == nil || *s.CPUPercent != 40 {
		t.Errorf("CPUPercent = %v", s.CPUPercent)
	}
	if s.CPUCount == nil || *s.CPUCount != 10 || s.LoadAvg1Min == nil || *s.LoadAvg1Min != 2.5 {
		t.Errorf("CPUCount/LoadAvg1Min = %v/%v", s.CPUCount, s.LoadAvg1Min)
	}
	if s.MemoryTotalBytes == nil || *s.MemoryTotalBytes != 16<<30 ||
		s.MemoryAvailableBytes == nil || *s.MemoryAvailableBytes != 8<<30 {
		t.Errorf("memoria = %v/%v", s.MemoryTotalBytes, s.MemoryAvailableBytes)
	}
	if s.Goroutines != 7 || !s.SampledAt.Equal(t0) {
		t.Errorf("Goroutines/SampledAt = %d/%v", s.Goroutines, s.SampledAt)
	}
	if got := m.Snapshot(); got.SampledAt != s.SampledAt || *got.CPUPercent != 40 {
		t.Errorf("Snapshot non restituisce l'ultimo campione: %+v", got)
	}
}

func TestSampleFailedReadsAreAbsentNotZero(t *testing.T) {
	boom := errors.New("not supported")
	r := &fakeReader{cpuErr: boom, countErr: boom, loadErr: boom, memErr: boom}
	s := newTestMonitor(r).Sample()

	if s.CPUPercent != nil || s.CPUCount != nil || s.LoadAvg1Min != nil ||
		s.MemoryTotalBytes != nil || s.MemoryAvailableBytes != nil {
		t.Errorf("letture fallite devono essere nil: %+v", s)
	}
	// Il campione esiste comunque: è parziale, non mancante.
	if s.SampledAt.IsZero() || s.Goroutines != 7 {
		t.Errorf("campione parziale perso: %+v", s)
	}
}

func TestSampleSmoothsCPU(t *testing.T) {
	r := fullReader()
	r.cpu = []float64{40, 80, 80}
	m := newTestMonitor(r) // alpha 0.5

	want := []float64{40, 60, 70}
	for i, w := range want {
		if got := *m.Sample().CPUPercent; got != w {
			t.Errorf("campione %d: CPU = %v, want %v", i, got, w)
		}
	}
}

func TestSampleCPUErrorKeepsAverage(t *testing.T) {
	r := fullReader() // primo campione CPU = 40
	m := newTestMonitor(r)
	m.Sample()

	r.setCPUErr(errors.New("transient"))
	if s := m.Sample(); s.CPUPercent != nil {
		t.Errorf("CPU fallita deve essere nil, got %v", *s.CPUPercent)
	}

	r.setCPUErr(nil)
	r.pushCPU(80)
	// La media riparte da 40, non da zero: 0.5*80 + 0.5*40 = 60.
	if got := *m.Sample().CPUPercent; got != 60 {
		t.Errorf("CPU dopo l'errore = %v, want 60 (l'errore non deve azzerare la media)", got)
	}
}

func TestSnapshotBeforeFirstSample(t *testing.T) {
	s := newTestMonitor(fullReader()).Snapshot()
	if !s.SampledAt.IsZero() || s.CPUPercent != nil {
		t.Errorf("prima del primo Sample lo snapshot deve essere vuoto: %+v", s)
	}
}

// --- JSON ------------------------------------------------------------------------

func TestSnapshotJSONOmitsAbsent(t *testing.T) {
	got, err := json.Marshal(Snapshot{CPUPercent: ptr(12.5), Goroutines: 3})
	if err != nil {
		t.Fatal(err)
	}
	// Nessun campo nil, nessun sampled_at zero: solo i valori misurati.
	if want := `{"cpu_percent":12.5,"goroutines":3}`; string(got) != want {
		t.Errorf("JSON\n got: %s\nwant: %s", got, want)
	}
	got, _ = json.Marshal(Snapshot{SampledAt: t0})
	if !strings.Contains(string(got), `"sampled_at":"2026-10-14T10:00:00Z"`) {
		t.Errorf("sampled_at mancante: %s", got)
	}
}

// --- Run -------------------------------------------------------------------------

func TestRunSamplesImmediatelyAndStops(t *testing.T) {
	r := fullReader()
	r.cpu = make([]float64, 1000) // abbastanza campioni per tutto il test
	m := newTestMonitor(r)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done) // chiudere un canale "sveglia" chi ci sta aspettando
	}()

	// Attende almeno 3 campioni: il primo immediato, poi quelli del ticker.
	deadline := time.Now().Add(2 * time.Second)
	for r.callCount() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("Run non campiona: %d campioni in 2 s", r.callCount())
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run non termina dopo cancel()")
	}

	// Dopo l'uscita non si campiona più.
	n := r.callCount()
	time.Sleep(50 * time.Millisecond)
	if r.callCount() != n {
		t.Error("Run continua a campionare dopo essere uscito: ticker non fermato?")
	}
}

func TestSnapshotConcurrentWithSample(t *testing.T) {
	// Con go test -race: Sample e Snapshot in parallelo non devono fare data race.
	r := fullReader()
	r.cpu = make([]float64, 200)
	m := newTestMonitor(r)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			m.Sample()
		}
	})
	for range 4 {
		wg.Go(func() {
			for range 200 {
				m.Snapshot()
			}
		})
	}
	wg.Wait()
}

// --- HostReader (macchina reale) ---------------------------------------------------

func TestHostReaderOnThisMachine(t *testing.T) {
	// Non controlla valori esatti (dipendono dalla macchina), solo che le
	// letture funzionino e siano plausibili sulla piattaforma di sviluppo/CI.
	var r HostReader
	if n, err := r.CPUCount(); err != nil || n < 1 {
		t.Errorf("CPUCount = %d, %v", n, err)
	}
	total, avail, err := r.Memory()
	if err != nil || total == 0 || avail > total {
		t.Errorf("Memory = %d/%d, %v", avail, total, err)
	}
	if p, err := r.CPUPercent(); err != nil || p < 0 || p > 100 {
		t.Errorf("CPUPercent = %v, %v", p, err)
	}
	if l, err := r.LoadAvg1(); err != nil || l < 0 {
		t.Errorf("LoadAvg1 = %v, %v", l, err)
	}
}
