// Package costmodel prevede quanto costerebbe un frame a ogni split point:
// tempo del device, invio, attesa in coda sul server, servizio del server.
// Non ha stato: riceve le stime (internal/ewma, netmeasure, dispatch) e
// restituisce previsioni. Lo usano l'algoritmo di offloading (Fase 3.6) e
// sciot-replay, che lo verifica contro le misure registrate.
//
// L'attesa in coda è prevista IN REGIME con la teoria delle code: è diversa
// dalla previsione del dispatcher per l'ammissione, che simula la coda di
// questo istante slot per slot. Qui la domanda è "se il carico diventa
// questo, quanto aspetta in media un frame?".
package costmodel

import "math"

// ErlangC è la probabilità che un frame in arrivo debba aspettare, in una
// coda M/M/c con c serventi e carico offerto a = λ·S (in erlang). Vale solo
// per a < c (coda stabile); altrimenti restituisce 1.
//
// Si calcola dalla formula di Erlang B con la ricorrenza
//
//	B(0) = 1,   B(k) = a·B(k−1) / (k + a·B(k−1))
//
// e poi
//
//	C = c·B(c) / (c − a·(1 − B(c)))
//
// La ricorrenza evita i fattoriali e le potenze di a, che con c grande
// andrebbero fuori scala.
func ErlangC(c int, a float64) float64 {
	switch {
	case c < 1 || a < 0:
		return math.NaN()
	case a >= float64(c):
		return 1
	case a == 0:
		return 0
	}
	b := 1.0
	for k := 1; k <= c; k++ {
		b = a * b / (float64(k) + a*b)
	}
	return float64(c) * b / (float64(c) - a*(1-b))
}

// WaitMMc è l'attesa media in coda (senza il servizio) di una M/M/c:
//
//	Wq = C(c, a) · S / (c − a),   a = λ·S
//
// lambda in frame al secondo, s in secondi; il risultato in secondi. ok è
// false se la coda è instabile (λ·S ≥ c): l'attesa cresce senza limite, e il
// sistema vero la limita solo rifiutando frame (ammissione).
func WaitMMc(c int, lambda, s float64) (wait float64, ok bool) {
	a := lambda * s
	if c < 1 || lambda < 0 || s < 0 || a >= float64(c) {
		return math.Inf(1), false
	}
	return ErlangC(c, a) * s / (float64(c) - a), true
}

// WaitGGc corregge WaitMMc per arrivi e servizi non esponenziali con
// l'approssimazione di Allen–Cunneen:
//
//	Wq(G/G/c) ≈ Wq(M/M/c) · (ca² + cs²) / 2
//
// ca e cs sono i coefficienti di variazione (deviazione standard / media)
// degli intervalli tra arrivi e dei tempi di servizio. Con ca = cs = 1
// (esponenziali) è M/M/c; con servizi costanti (cs = 0) e arrivi di Poisson
// è circa la metà. La M/M/c resta il limite prudente quando ca e cs ≤ 1.
func WaitGGc(c int, lambda, s, ca, cs float64) (wait float64, ok bool) {
	if ca < 0 || cs < 0 {
		return math.NaN(), false
	}
	wait, ok = WaitMMc(c, lambda, s)
	if !ok {
		return wait, false
	}
	return wait * (ca*ca + cs*cs) / 2, true
}
