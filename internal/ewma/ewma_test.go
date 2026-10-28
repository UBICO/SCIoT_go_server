package ewma

import (
	"math"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestAlpha(t *testing.T) {
	const tau = 10 * time.Second
	cases := []struct {
		name string
		dt   time.Duration
		tau  time.Duration
		n    uint64
		want float64
	}{
		{"campioni molto distanti", time.Hour, tau, 100, 1 - math.Exp(-360)},
		{"un τ dopo", tau, tau, 100, 1 - math.Exp(-1)},
		{"stesso istante, molti campioni", 0, tau, 100, 1.0 / 101},
		{"secondo campione: media semplice", time.Millisecond, tau, 1, 0.5},
		{"terzo campione", time.Millisecond, tau, 2, 1.0 / 3},
		{"il peso sul tempo vince sulla media", tau, tau, 9, 1 - math.Exp(-1)},
		{"tau zero: nessuna memoria", time.Millisecond, 0, 100, 1},
		{"dt negativo", -time.Second, tau, 100, 1.0 / 101},
	}
	for _, c := range cases {
		if got := Alpha(c.dt, c.tau, c.n); !near(got, c.want) {
			t.Errorf("%s: Alpha = %v, atteso %v", c.name, got, c.want)
		}
	}
}

func TestUpdateIsTheMeanAtFirst(t *testing.T) {
	// Campioni ravvicinati: media e varianza semplici (della popolazione).
	var s Stat
	t0 := time.Unix(0, 0)
	for i, x := range []float64{2, 4, 6} {
		s.Update(t0.Add(time.Duration(i)*time.Millisecond), x, time.Hour)
	}
	if s.N() != 3 || !near(s.Mean(), 4) {
		t.Errorf("N %d, media %v: attesi 3 e 4", s.N(), s.Mean())
	}
	if want := math.Sqrt(8.0 / 3); !near(s.Std(), want) {
		t.Errorf("Std = %v, attesa %v", s.Std(), want)
	}
}

func TestUpdateForgetsOverTime(t *testing.T) {
	var s Stat
	t0 := time.Unix(0, 0)
	const tau = 10 * time.Second
	s.Update(t0, 100, tau)
	s.Update(t0.Add(time.Millisecond), 100, tau) // n = 2, media 100
	// Un'ora dopo: il passato non conta più, la media è il campione nuovo.
	s.Update(t0.Add(time.Hour), 10, tau)
	if math.Abs(s.Mean()-10) > 1e-6 {
		t.Errorf("media dopo un'ora = %v, attesa ~10", s.Mean())
	}
	// Un τ dopo: il nuovo pesa 1 - 1/e.
	s.Update(t0.Add(time.Hour+tau), 20, tau)
	if want := 10 + (1-math.Exp(-1))*10; !near(s.Mean(), want) {
		t.Errorf("media un τ dopo = %v, attesa %v", s.Mean(), want)
	}
}

func TestLastIgnoresOutOfOrderSamples(t *testing.T) {
	var s Stat
	t1 := time.Unix(10, 0)
	s.Update(t1, 1, time.Second)
	s.Update(time.Unix(5, 0), 1, time.Second)
	if !s.Last().Equal(t1) || s.N() != 2 {
		t.Errorf("Last %v, N %d: attesi %v e 2", s.Last(), s.N(), t1)
	}
}

func TestEstimate(t *testing.T) {
	var s Stat
	if _, ok := s.Estimate(time.Now(), 1); ok {
		t.Fatal("stima vuota: ok deve essere false")
	}
	t0 := time.Unix(0, 0)
	s.Update(t0, 0.002, time.Hour)
	s.Update(t0, 0.004, time.Hour)
	e, ok := s.Estimate(t0.Add(1500*time.Millisecond), 1000) // secondi → ms
	if !ok || !near(e.Mean, 3) || !near(e.Std, 1) || e.N != 2 || e.AgeMS != 1500 {
		t.Errorf("Estimate = %+v, %v", e, ok)
	}
}
