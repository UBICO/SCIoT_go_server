package dispatch

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/stream"
)

func TestEstimateSources(t *testing.T) {
	d := New(newBackend(1, false), DefaultConfig())
	d.mu.Lock()
	d.service[key{"yolo", 4}] = stat(30 * time.Millisecond)
	d.segments[segKey{"yolo", 6}] = stat(5 * time.Millisecond)
	d.segments[segKey{"yolo", 7}] = stat(20 * time.Millisecond)
	d.overhead["yolo"] = stat(2 * time.Millisecond)
	d.last["yolo"] = 7
	d.mu.Unlock()

	if st, src, ok := d.Estimate("yolo", 4); !ok || src != SourceMeasured || math.Abs(st.MeanMS-30) > 1e-6 || st.Samples != 1 {
		t.Errorf("split 4: %+v %q %v; attesa la misura diretta", st, src, ok)
	}
	// Split 5: segmenti 6 e 7 più l'overhead = 27 ms.
	if st, src, ok := d.Estimate("yolo", 5); !ok || src != SourceSegments || math.Abs(st.MeanMS-27) > 1e-6 || st.Samples != 0 {
		t.Errorf("split 5: %+v %q %v; attesi 27 ms dai segmenti", st, src, ok)
	}
	// Split 3: manca il segmento 4. Niente ripiego sulla S più lunga.
	if _, _, ok := d.Estimate("yolo", 3); ok {
		t.Error("split 3: segmento 4 mai misurato, attesa nessuna stima")
	}
	if _, _, ok := d.Estimate("altro", 0); ok {
		t.Error("modello sconosciuto: attesa nessuna stima")
	}
}

func TestProfileModelSeedsEverySplit(t *testing.T) {
	b := newBackend(2, false)
	b.layers = []float64{1, 2, 3} // modello di 3 segmenti, eseguito tutto
	d := New(b, DefaultConfig())

	res, err := d.ProfileModel(context.Background(), "m", 16, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(b.seen()); got != ProfileWarmup+4 {
		t.Errorf("esecuzioni = %d, attese %d (riscaldamento + misurate)", got, ProfileWarmup+4)
	}
	if res.Runs != 4 || len(res.SegmentMS) != 3 || math.Abs(res.SegmentMS[2]-3) > 1e-9 || res.OverheadMS < 0 {
		t.Errorf("risultato = %+v", res)
	}
	// Ogni split −1..1 ora ha S dai segmenti; nessuno è "misurato".
	for k := -1; k <= 1; k++ {
		st, src, ok := d.Estimate("m", k)
		if !ok || src != SourceSegments {
			t.Errorf("split %d: %+v %q %v", k, st, src, ok)
		}
	}
	// Il profilo non è traffico: niente λ, ρ, interarrivi, totali dei frame.
	snap := d.Snapshot()
	if snap.OfferedPerSec != 0 || snap.AdmittedPerSec != 0 || *snap.Utilization != 0 || snap.Interarrival != nil ||
		snap.Totals.Completed != 0 || snap.Totals.Profiled != uint64(ProfileWarmup+4) || len(snap.Service) != 0 {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestProfileModelStopsOnError(t *testing.T) {
	b := newBackend(1, false)
	b.err = errors.New("worker down")
	d := New(b, DefaultConfig())
	res, err := d.ProfileModel(context.Background(), "m", 16, 3)
	if err == nil || res.Runs != 0 {
		t.Errorf("res = %+v, err = %v; atteso un errore alla prima esecuzione", res, err)
	}
	if got := len(b.seen()); got != 1 {
		t.Errorf("esecuzioni = %d, attesa 1 (si ferma al primo errore)", got)
	}
}

func TestProfileWaitsForASlot(t *testing.T) {
	b := newBackend(1, true)
	d := New(b, DefaultConfig())
	seed(d, time.Millisecond)
	frame := submit(d, job(1, 5*time.Second))
	eventually(t, "frame in esecuzione", func() bool { return len(b.seen()) == 1 })

	done := make(chan error, 1)
	go func() {
		_, err := d.ProfileModel(context.Background(), "m", 16, 1)
		done <- err
	}()
	// Il profilo aspetta in coda: lo slot è del frame.
	eventually(t, "profilo in coda", func() bool { return d.Snapshot().Queued == 1 })
	b.mu.Lock()
	if b.maxRunning != 1 {
		t.Errorf("in esecuzione insieme = %d, atteso 1", b.maxRunning)
	}
	b.mu.Unlock()
	for range 1 + ProfileWarmup + 1 { // il frame, poi le esecuzioni del profilo
		b.hold <- struct{}{}
	}
	if r := <-frame; r.err != nil {
		t.Errorf("frame: %v", r.err)
	}
	if err := <-done; err != nil {
		t.Errorf("profilo: %v", err)
	}
}

func TestProfileWithoutWorker(t *testing.T) {
	d := New(newBackend(0, false), DefaultConfig())
	if _, err := d.ProfileModel(context.Background(), "m", 16, 1); !errors.Is(err, stream.ErrWorkerUnavailable) {
		t.Errorf("err = %v, atteso ErrWorkerUnavailable", err)
	}
}
