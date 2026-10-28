package costmodel

import (
	"math"
	"strconv"

	"github.com/UBICO/SCIoT_go_server/internal/netmeasure"
)

// Inputs sono le stime con cui si prevede il costo di un frame di un device.
// Tutto in millisecondi, byte e frame al secondo.
type Inputs struct {
	// DeviceMS è la stima del device per segmento 0..last (store dei device,
	// dai segment_ms); nil per un segmento mai eseguito.
	DeviceMS []*float64
	// Network è la rete del device; nil senza stream (rete sconosciuta).
	Network *netmeasure.State
	// InputBytes sono i byte che il device invia a ogni split point
	// (stream.Catalog: gli stessi dell'hello).
	InputBytes map[int]int
	// ServiceMS è S previsto per split point (dispatcher); assente =
	// sconosciuto.
	ServiceMS map[int]float64
	Queue     Queue
}

// Queue è il carico del server SENZA il device di cui si prevede il costo.
type Queue struct {
	Slots int // c
	// OthersLambda sono i frame al secondo degli altri device; OthersErlangs
	// il loro carico Σ λ_i·S_i in erlang (S in secondi).
	OthersLambda  float64
	OthersErlangs float64
	// CA e CS sono i coefficienti di variazione degli intervalli tra arrivi e
	// dei tempi di servizio (WaitGGc).
	CA, CS float64
}

// Prediction è il costo previsto di un frame a uno split point.
type Prediction struct {
	Split     int  // split point; == last per LOCAL
	Local     bool // tutto sul device
	DeviceMS  float64
	UploadMS  float64
	WaitMS    float64
	ServiceMS float64
	TotalMS   float64
	// FramesPerSec è il ritmo del device a questo split (stima prudente:
	// limitato dal calcolo del prefisso o dall'invio); Rho è l'utilizzo del
	// server con il device spostato qui.
	FramesPerSec float64
	Rho          float64
	// Saturated: con il device qui la coda è instabile (Rho ≥ 1), l'attesa
	// non è limitata: il server rifiuterebbe frame.
	Saturated bool
	// Missing elenca gli ingressi sconosciuti ("device segment 3",
	// "bandwidth", "service", ...): con Missing non vuoto TotalMS non è una
	// previsione. Sconosciuto non è "costa tanto".
	Missing []string
}

// Predict prevede il costo di un frame allo split point split (−1..last;
// last = LOCAL) per un modello con segmenti 0..last.
func Predict(in Inputs, split, last int) Prediction {
	p := Prediction{Split: split, Local: split >= last}
	q := in.Queue

	if p.Local {
		p.DeviceMS = deviceMS(in, last, &p)
		p.TotalMS = p.DeviceMS
		p.FramesPerSec = rate(p.DeviceMS)
		// Il server resta con il carico degli altri.
		if q.Slots > 0 {
			p.Rho = q.OthersErlangs / float64(q.Slots)
		}
		return p
	}

	p.DeviceMS = deviceMS(in, split, &p)
	p.UploadMS = uploadMS(in, split, &p)
	svc, ok := in.ServiceMS[split]
	if !ok {
		p.Missing = append(p.Missing, "service")
	}
	p.ServiceMS = svc
	// Il device produce frame al ritmo del più lento tra prefisso e invio.
	p.FramesPerSec = rate(max(p.DeviceMS, p.UploadMS))

	// Prima di ogni divisione per Slots.
	if q.Slots < 1 {
		p.Missing = append(p.Missing, "slots")
		p.TotalMS = p.DeviceMS + p.UploadMS + p.ServiceMS
		return p
	}

	if math.IsInf(p.FramesPerSec, 1) {
		// Inf*0 = NaN in erlangs e in WaitGGc: il device senza limite satura.
		p.Rho = math.Inf(1)
		p.Saturated, p.WaitMS = true, math.Inf(1)
	} else {
		lambda := q.OthersLambda + p.FramesPerSec
		erlangs := q.OthersErlangs + p.FramesPerSec*p.ServiceMS/1000
		p.Rho = erlangs / float64(q.Slots)
		// S medio pesato sui frame/s, in secondi.
		wait, ok := WaitGGc(q.Slots, lambda, erlangs/lambda, q.CA, q.CS)
		if ok {
			p.WaitMS = wait * 1000
		} else {
			p.Saturated, p.WaitMS = true, math.Inf(1)
		}
	}
	p.TotalMS = p.DeviceMS + p.UploadMS + p.WaitMS + p.ServiceMS
	return p
}

// deviceMS è il tempo del device per i segmenti 0..upTo (0 per upTo < 0);
// aggiunge a p.Missing i segmenti mai misurati.
func deviceMS(in Inputs, upTo int, p *Prediction) float64 {
	var total float64
	for i := 0; i <= upTo; i++ {
		if i >= len(in.DeviceMS) || in.DeviceMS[i] == nil {
			p.Missing = append(p.Missing, "device segment "+strconv.Itoa(i))
			continue
		}
		total += *in.DeviceMS[i]
	}
	return total
}

// uploadMS è il tempo di invio del frame dello split point split.
func uploadMS(in Inputs, split int, p *Prediction) float64 {
	bytes, ok := in.InputBytes[split]
	if !ok {
		p.Missing = append(p.Missing, "input bytes")
		return 0
	}
	if in.Network == nil {
		p.Missing = append(p.Missing, "network")
		return 0
	}
	ms, ok := in.Network.UploadMS(bytes)
	if !ok {
		p.Missing = append(p.Missing, "bandwidth or latency")
		return 0
	}
	return ms
}

// rate sono i frame al secondo di un ciclo di ms millisecondi.
func rate(ms float64) float64 {
	if ms <= 0 {
		return math.Inf(1)
	}
	return 1000 / ms
}
