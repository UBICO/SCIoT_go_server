package offload

import (
	"math"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
)

func near(a, b Load) bool {
	return math.Abs(a.PerSec-b.PerSec) < 1e-9 && math.Abs(a.Erlangs-b.Erlangs) < 1e-9
}

func TestReservationsFadeAsTheWindowSeesThem(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r := newReservations(10 * time.Second)
	r.reserve(t0, "a", Load{PerSec: 10, Erlangs: 1})

	// La finestra di 10 s del dispatcher ne ha visto metà: resta metà.
	if got := r.pending(t0.Add(5*time.Second), "b"); !near(got, Load{5, 0.5}) {
		t.Errorf("dopo 5 s: %+v, atteso {5 0.5}", got)
	}
	if got := r.pending(t0, "a"); !near(got, Load{}) {
		t.Errorf("il device non vede la propria prenotazione: %+v", got)
	}
	if got := r.pending(t0.Add(10*time.Second), "b"); !near(got, Load{}) {
		t.Errorf("dopo una finestra la misura la contiene tutta: %+v", got)
	}

	// Solo positive.
	r.reserve(t0, "c", Load{PerSec: -10, Erlangs: -1})
	if got := r.pending(t0, "b"); !near(got, Load{10, 1}) {
		t.Errorf("una prenotazione negativa conta: %+v", got)
	}

	// Una seconda prenotazione tiene la parte non vista della prima.
	r.reserve(t0.Add(5*time.Second), "a", Load{PerSec: 10, Erlangs: 1})
	if got := r.pending(t0.Add(5*time.Second), "b"); !near(got, Load{15, 1.5}) {
		t.Errorf("due prenotazioni: %+v, atteso {15 1.5}", got)
	}

	r.forget("a")
	if got := r.pending(t0.Add(5*time.Second), "b"); !near(got, Load{}) {
		t.Errorf("dopo forget: %+v", got)
	}
}

func TestReservationsWithoutWindow(t *testing.T) {
	r := newReservations(0)
	r.reserve(time.Unix(0, 0), "a", Load{10, 1})
	if got := r.pending(time.Unix(0, 0), "b"); !near(got, Load{}) {
		t.Errorf("finestra 0: %+v", got)
	}
}

// Il device a passa a SPLIT; b, che decide subito dopo con le stesse misure,
// vede il carico di a prima che il dispatcher lo misuri.
func TestLatencyReservesWhatItGrants(t *testing.T) {
	l := NewLatency(DefaultLatencyConfig())
	now := time.Unix(1000, 0)
	at := func(id string) State {
		s := state(api.Policy{Mode: api.ModeLocal, PolicyVersion: 1})
		s.DeviceID, s.PolicyAge, s.MaxWindow = id, time.Minute, 3
		s.Inputs.CycleMS = f(1)
		return s
	}
	var d Decision
	// Le prime previsioni servono al rumore (few_samples).
	for range 4 {
		now = now.Add(time.Second)
		d = l.Decide(now, at("a"))
	}
	if d.Mode != api.ModeSplit || d.MaxInflight == nil {
		t.Fatalf("a deve passare a SPLIT con una finestra: %+v", d.Reason)
	}
	if !near(d.Pending, Load{}) {
		t.Errorf("a non ha prenotazioni altrui: %+v", d.Pending)
	}
	chosen := d.Predictions[IndexOf(d.Predictions, *d.SplitPoint, *d.MaxInflight)]
	want := serverLoad(chosen)
	got := l.Decide(now, at("b")).Pending
	if !near(got, want) || got.Erlangs <= 0 {
		t.Errorf("b vede %+v, atteso il carico concesso ad a %+v", got, want)
	}
	l.Forget("a")
	if got := l.Decide(now, at("b")).Pending; !near(got, Load{}) {
		t.Errorf("dopo Forget(a): %+v", got)
	}
}

// TestLatencyWithoutReservations: con Reserve spento un cambio non prenota e
// nessuno vede prenotazioni (ablazione, Fase 3.8).
func TestLatencyWithoutReservations(t *testing.T) {
	cfg := DefaultLatencyConfig()
	cfg.Reserve = false
	l := NewLatency(cfg)
	now := time.Unix(1000, 0)
	at := func(id string) State {
		s := state(api.Policy{Mode: api.ModeLocal, PolicyVersion: 1})
		s.DeviceID, s.PolicyAge, s.MaxWindow = id, time.Minute, 3
		s.Inputs.CycleMS = f(1)
		return s
	}
	var d Decision
	for range 4 {
		now = now.Add(time.Second)
		d = l.Decide(now, at("a"))
	}
	if d.Mode != api.ModeSplit {
		t.Fatalf("a deve passare a SPLIT anche senza prenotazioni: %+v", d.Reason)
	}
	if got := l.Decide(now, at("b")).Pending; !near(got, Load{}) {
		t.Errorf("senza prenotazioni b vede %+v", got)
	}
}

// TestReservationsOnlyInTheFallback: con gli altri device simulati come
// client le prenotazioni non entrano (né nelle previsioni SPLIT né nel ρ di
// LOCAL) e la decisione è quella senza prenotazioni; un cambio però prenota
// lo stesso, e chi decide nel ripiego (senza client) lo vede. Per questo
// l'ablazione delle prenotazioni non ha bisogno di run dal vivo.
func TestReservationsOnlyInTheFallback(t *testing.T) {
	off := DefaultLatencyConfig()
	off.Reserve = false
	on, without := NewLatency(DefaultLatencyConfig()), NewLatency(off)
	now := time.Unix(1000, 0)
	at := func(id string) State {
		s := state(api.Policy{Mode: api.ModeLocal, PolicyVersion: 1})
		s.DeviceID, s.PolicyAge, s.MaxWindow = id, time.Minute, 3
		s.Inputs.CycleMS = f(1)
		return s
	}
	var a Decision
	for range 4 {
		now = now.Add(time.Second)
		a = on.Decide(now, at("a"))
		without.Decide(now, at("a"))
	}
	if a.Mode != api.ModeSplit {
		t.Fatalf("a deve passare a SPLIT: %+v", a.Reason)
	}
	// b vede a come client alla sua policy nuova (Fleet.SetPolicy).
	other := at("a")
	other.Policy = api.Policy{Mode: api.ModeSplit, SplitPoint: a.SplitPoint, MaxInflight: a.MaxInflight}
	c, ok := clientOf(other)
	if !ok {
		t.Fatal("client di a")
	}
	b := at("b")
	b.Inputs.Queue.Clients = []costmodel.Client{c}
	dOn, dOff := on.Decide(now, b), without.Decide(now, b)
	if !near(dOn.Pending, Load{}) {
		t.Errorf("con i client b non deve sommare prenotazioni: %+v", dOn.Pending)
	}
	if dOn.Mode != dOff.Mode || dOn.Reason != dOff.Reason {
		t.Errorf("decisioni diverse: %s %s / %s %s", dOn.Mode, dOn.Reason, dOff.Mode, dOff.Reason)
	}
	for i, p := range dOn.Predictions {
		if q := dOff.Predictions[i]; p.P95MS != q.P95MS || p.TotalMS != q.TotalMS || p.Rho != q.Rho {
			t.Errorf("split %d/%d (local %v): p95 %v / %v, ρ %v / %v", p.Split, p.Window, p.Local, p.P95MS, q.P95MS, p.Rho, q.Rho)
		}
	}
	// Nel ripiego (b senza client) la prenotazione di a c'è ancora.
	if got := on.Decide(now, at("b")).Pending; got.Erlangs <= 0 {
		t.Errorf("nel ripiego b deve vedere il carico concesso ad a: %+v", got)
	}
}
