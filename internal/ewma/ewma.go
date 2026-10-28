// Package ewma è la stima di una grandezza misurata a intervalli irregolari:
// media e varianza mobili esponenziali, pesate sul TEMPO, più l'età della
// misura. La usano il dispatcher (tempo di servizio S), la rete (banda) e i
// tempi del device per segmento.
//
// Pesare sul tempo vuol dire che la stima dimentica il passato con la stessa
// velocità in secondi qualunque sia la frequenza dei campioni: con campioni
// rari (un probe ogni 30 s) ogni campione pesa molto, con campioni fitti
// (50 frame/s) ognuno pesa poco. τ è la costante di tempo: un campione vecchio
// di τ pesa 1/e (~37%) di uno appena arrivato.
package ewma

import (
	"math"
	"time"
)

// Stat è la stima. Il valore zero è una stima vuota, pronta all'uso. Non è
// sicura da più goroutine: la protegge chi la possiede (con il suo mutex).
type Stat struct {
	mean float64
	vari float64 // varianza mobile
	n    uint64  // campioni visti
	last time.Time
}

// Alpha è il peso del campione nuovo, arrivato dt dopo il precedente, quando
// la stima ha già n campioni (n ≥ 1, senza il nuovo).
func Alpha(dt, tau time.Duration, n uint64) float64 {
	// Peso sul tempo; τ ≤ 0 = nessuna memoria.
	a := 0.0
	switch {
	case tau <= 0:
		a = 1
	case dt > 0:
		a = 1 - math.Exp(-dt.Seconds()/tau.Seconds())
	}
	// All'inizio pesa almeno come una media semplice: senza, una raffica di
	// campioni ravvicinati (il profilo all'avvio) lascerebbe la media sul
	// primo, spesso il più lento (riscaldamento).
	return max(a, 1/float64(n+1))
}

// Update aggiunge il campione x arrivato in at, con costante di tempo tau.
func (s *Stat) Update(at time.Time, x float64, tau time.Duration) {
	if s.n == 0 {
		s.mean, s.vari, s.n, s.last = x, 0, 1, at
		return
	}
	a := Alpha(at.Sub(s.last), tau, s.n)
	// diff prima di aggiornare la media: varianza mobile esponenziale, che con
	// a = 1/(n+1) coincide con la varianza della popolazione.
	diff := x - s.mean
	s.mean += a * diff
	s.vari = (1 - a) * (s.vari + a*diff*diff)
	s.n++
	// Un campione fuori ordine non fa tornare indietro l'età.
	if at.After(s.last) {
		s.last = at
	}
}

// N è il numero di campioni visti; 0 = stima vuota.
func (s Stat) N() uint64 { return s.n }

// Mean è la media; 0 se vuota (controlla N).
func (s Stat) Mean() float64 { return s.mean }

// Std è la deviazione standard dei campioni.
func (s Stat) Std() float64 { return math.Sqrt(s.vari) }

// Last è quando è arrivato l'ultimo campione; zero se vuota.
func (s Stat) Last() time.Time { return s.last }

// Estimate è la stima come la si espone (status, registrazione), in una
// unità scelta da chi la espone: scale moltiplica media e deviazione (per
// esempio da secondi a millisecondi).
type Estimate struct {
	Mean  float64 `json:"mean"`
	Std   float64 `json:"std"`
	N     uint64  `json:"n"`
	AgeMS float64 `json:"age_ms"` // da quanto non arriva un campione
}

// Estimate restituisce la stima vista in now; ok è false se vuota.
func (s Stat) Estimate(now time.Time, scale float64) (e Estimate, ok bool) {
	if s.n == 0 {
		return Estimate{}, false
	}
	return Estimate{
		Mean:  s.mean * scale,
		Std:   s.Std() * scale,
		N:     s.n,
		AgeMS: float64(now.Sub(s.last)) / float64(time.Millisecond),
	}, true
}
