package offload

import (
	"math"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
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
