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
	// MaxInflight è la finestra del device: frame in volo al massimo
	// (max_inflight dell'hello); 0 = sconosciuta.
	MaxInflight int
	// CycleMS è A: quanto dura un ciclo del device oltre al calcolo dei
	// segmenti e all'invio (acquisizione, gestione delle risposte; con una
	// camera anche l'attesa del frame). nil = sconosciuto.
	CycleMS *float64
}

// Queue è il carico del server SENZA il device di cui si prevede il costo.
type Queue struct {
	Slots int // c
	// OthersLambda sono i frame al secondo degli altri device; OthersErlangs
	// il loro carico Σ λ_i·S_i in erlang (S in secondi).
	OthersLambda  float64
	OthersErlangs float64
	// CA e CS sono i coefficienti di variazione degli intervalli tra arrivi e
	// dei tempi di servizio: la simulazione (sim.go) ne estrae arrivi e
	// servizi.
	CA, CS float64
}

// Perché i frame del device non arrivano tutti al server (Prediction.Bound).
const (
	// BoundDevice: nessun frame finisce sul device, vanno tutti al server.
	BoundDevice = "device"
	// BoundWindow: la causa più frequente dei frame finiti sul device è la
	// finestra piena (max_inflight frame in volo): il client non aspetta.
	BoundWindow = "window"
	// BoundDeadline: la causa più frequente è il server, che rifiuta il frame
	// (attesa più S oltre la sua scadenza, il tempo del device per finirlo da
	// sé) o lo lascia scadere in coda.
	BoundDeadline = "deadline"
)

// Prediction è il costo previsto di un frame a uno split point.
//
// Il client non aspetta mai il server: con la finestra piena o un frame
// rifiutato, finisce il frame da sé. A uno split point una quota dei frame va
// al server (ServerShare), il resto finisce sul device; TotalMS è la latenza
// media di tutti. In SPLIT i valori vengono dalla simulazione (sim.go).
type Prediction struct {
	Split     int  // split point; == last per LOCAL
	Local     bool // tutto sul device
	DeviceMS  float64
	UploadMS  float64
	WaitMS    float64
	ServiceMS float64
	// SuffixMS è il tempo del device per i segmenti split+1..last: quanto
	// costa un frame finito sul device, ed è la sua scadenza sul server.
	SuffixMS float64
	// ServerMS è la latenza media di un frame servito dal server (prefisso,
	// invio, attesa, S, ritorno); LocalMS di uno finito sul device (in LOCAL
	// tutto il modello; in SPLIT, per un rifiutato, anche l'attesa del drain).
	// WaitMS è l'attesa in coda media dei frame serviti.
	ServerMS float64
	LocalMS  float64
	TotalMS  float64
	// FramesPerSec sono i frame al secondo del device, ServerFramesPerSec
	// quelli serviti dal server, ServerShare il loro rapporto. Bound dice
	// perché non vanno tutti al server.
	FramesPerSec       float64
	ServerFramesPerSec float64
	ServerShare        float64
	Bound              string
	// P95MS è il 95° percentile della latenza dei frame del device;
	// RejectedPerSec i frame al secondo rifiutati o scaduti sul server,
	// WindowFullPerSec quelli trovati con la finestra piena (tutti finiti
	// sul device). Solo SPLIT, dalla simulazione (sim.go).
	P95MS            float64
	RejectedPerSec   float64
	WindowFullPerSec float64
	// Rho è l'utilizzo del server con il device a questo split point
	// (in SPLIT: slot occupati nella simulazione).
	Rho float64
	// Saturated: gli altri device saturano già il server (Rho ≥ 1 senza
	// questo device): nessun suo frame sarebbe servito.
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
		p.DeviceMS = deviceMS(in, 0, last, &p)
		p.LocalMS, p.TotalMS = p.DeviceMS, p.DeviceMS
		// Senza A il ritmo è quello del solo calcolo: la latenza non ne
		// dipende, quindi A non è un ingresso mancante.
		a := 0.0
		if in.CycleMS != nil {
			a = *in.CycleMS
		}
		p.FramesPerSec = rate(a + p.DeviceMS)
		p.Bound = BoundDevice
		// Il server resta con il carico degli altri.
		if q.Slots > 0 {
			p.Rho = q.OthersErlangs / float64(q.Slots)
		}
		return p
	}

	p.DeviceMS = deviceMS(in, 0, split, &p)
	p.SuffixMS = deviceMS(in, split+1, last, &p)
	p.UploadMS = uploadMS(in, split, &p)
	svc, ok := in.ServiceMS[split]
	if !ok {
		p.Missing = append(p.Missing, "service")
	}
	p.ServiceMS = svc
	if in.CycleMS == nil {
		p.Missing = append(p.Missing, "cycle")
	}
	if in.MaxInflight < 1 {
		p.Missing = append(p.Missing, "max_inflight")
	}
	// Prima di ogni divisione per Slots.
	if q.Slots < 1 {
		p.Missing = append(p.Missing, "slots")
	}
	p.LocalMS = p.DeviceMS + p.SuffixMS
	if len(p.Missing) > 0 {
		p.TotalMS = p.DeviceMS + p.UploadMS + p.ServiceMS
		return p
	}
	down := 0.0
	if in.Network != nil && in.Network.LatencyMS != nil {
		down = *in.Network.LatencyMS
	}
	sp := simParams{
		P: p.DeviceMS, X: p.SuffixMS, U: p.UploadMS, S: p.ServiceMS, A: *in.CycleMS, DownMS: down,
		Window: in.MaxInflight, Slots: q.Slots, CS: q.CS, CA: q.CA,
	}
	if q.OthersLambda > 0 {
		sp.OthersPerMS = q.OthersLambda / 1000
		sp.OthersS = q.OthersErlangs / q.OthersLambda * 1000
	}
	r := simulate(sp)
	p.FramesPerSec, p.ServerFramesPerSec, p.ServerShare = r.FramesPerSec, r.ServerFramesPerSec, r.ServerShare
	p.RejectedPerSec, p.WindowFullPerSec = r.RejectedPerSec, r.WindowFullPerSec
	p.WaitMS, p.ServerMS, p.TotalMS, p.P95MS = r.WaitMS, r.ServerMS, r.TotalMS, r.P95MS
	if r.ServerFramesPerSec == 0 {
		p.ServerMS = math.NaN() // nessun frame servito: nella registrazione è null
	}
	if r.LocalMS > 0 {
		p.LocalMS = r.LocalMS // altrimenti resta prefisso + suffisso
	}
	p.Rho = r.Rho
	p.Saturated = q.OthersErlangs >= float64(q.Slots)
	switch {
	case r.RejectedPerSec == 0 && r.WindowFullPerSec == 0:
		p.Bound = BoundDevice
	case r.WindowFullPerSec >= r.RejectedPerSec:
		p.Bound = BoundWindow
	default:
		p.Bound = BoundDeadline
	}
	return p
}

// deviceMS è il tempo del device per i segmenti from..to (0 se from > to);
// aggiunge a p.Missing i segmenti mai misurati.
func deviceMS(in Inputs, from, to int, p *Prediction) float64 {
	var total float64
	for i := max(from, 0); i <= to; i++ {
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
