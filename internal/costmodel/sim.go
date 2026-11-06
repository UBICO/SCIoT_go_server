package costmodel

import (
	"container/heap"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/dispatch"
)

// Simulazione a eventi di un device a uno split point e del dispatcher.
//
// Le formule a valori medi non vedono due comportamenti del sistema vero
// (docs/COSTMODEL.md): i frame di uno stesso device si accodano tra loro
// quando la finestra supera gli slot, e il server li rifiuta in modo
// sistematico; mentre il device finisce un frame da sé non invia nulla, e gli
// slot restano vuoti. La simulazione esegue le stesse regole del codice vero
// per qualche secondo virtuale e conta.
//
// Regole del client (client/sciot_client, http_go_client.py e
// split_pipeline.py), in ordine in ogni ciclo:
//
//  1. drain: ogni errore ricevuto (rifiuto, scadenza) viene finito sul
//     device, X ms ciascuno, anche quelli che arrivano durante il drain;
//  2. acquisizione e contorno: A ms; il frame nasce alla fine (la latenza
//     non comprende A);
//  3. prefisso: P ms;
//  4. finestra piena (max_inflight frame in volo): il device finisce il
//     frame subito, X ms; altrimenti lo invia, U ms, e il frame arriva al
//     server alla fine dell'invio.
//
// La risposta (risultato o errore) torna dopo la latenza di rete e libera la
// finestra quando arriva, non al drain. Scadenza di un frame = X.
//
// Regole del server (internal/dispatch): ammissione con dispatch.PredictWait
// e la S stimata, coda FIFO, slot; un frame che ottiene lo slot dopo la sua
// scadenza torna come errore.
//
// Gli altri device in SPLIT sono client con le stesse regole e i loro tempi
// (simParams.Others, Fase 3.8): inviano appena la finestra si libera, vengono
// rifiutati, finiscono i rifiutati al drain. Si contano solo i frame del
// device (client 0). Senza l'elenco (ripiego), gli altri sono un flusso di
// frame sempre ammessi (il loro ritmo misurato conta già solo gli ammessi),
// con la loro variabilità.

const (
	// simWarmupMS è il tempo virtuale scartato all'inizio (coda vuota,
	// finestra vuota: non è il regime); simMeasureMS quello in cui si contano
	// i frame nati; simTailMS quello lasciato ai frame nati alla fine per
	// terminare.
	simWarmupMS  = 1000.0
	simMeasureMS = 5000.0
	simTailMS    = 2000.0
	// simMaxEvents ferma una simulazione degenere (gli altri device da soli
	// saturano il server e la coda cresce senza limite).
	simMaxEvents = 1_000_000
	// simMinCycleMS è la durata minima di un ciclo del device: con A, P e U
	// nulli il ciclo non farebbe avanzare il tempo.
	simMinCycleMS = 0.001
	// simMinAnswerMS è il tempo minimo perché una risposta torni al device
	// (server, thread che riceve nel client), anche con latenza di rete
	// nulla. Senza, un rifiuto arriverebbe nello stesso istante in cui il
	// device inizia il ciclo successivo, e l'ordine tra i due eventi
	// deciderebbe il risultato (frame/s al server raddoppiati); nel sistema
	// vero la risposta arriva sempre dopo.
	//
	// Misurato (Fase 3.8): in localhost, split −1 (1.2 MB), invio → risposta
	// meno il tempo del server è 1.06 ms di mediana (0.85–1.25 tra 10° e 90°
	// percentile); la sola lettura del frame sul server è 0.54 ms. Con 0.1 ms
	// la simulazione dava 14.8 rifiuti/s e nessuna finestra piena contro 7.9
	// e 7.0 misurati; con 1 ms 7.6 e 7.4.
	//
	// shortcut: costante, mentre il tempo vero cresce con i byte inviati (con
	// la rete "non limitante" l'invio simulato è solo la latenza); usare la
	// lettura misurata dal server (receive_ms) per split point se su rete vera
	// (Fase 4) le previsioni dipendono da lui.
	simMinAnswerMS = 1.0
	// simSeed rende il risultato riproducibile: la stessa registrazione dà
	// sempre le stesse previsioni.
	simSeed = 0x5c107
)

// simParams sono gli ingressi della simulazione, in millisecondi.
type simParams struct {
	P, X, U, S, A float64 // prefisso, suffisso, invio, servizio, ciclo
	DownMS        float64 // ritorno della risposta: latenza di rete
	Window, Slots int
	CS            float64 // variabilità di S (anche per gli altri)

	// Altri device come client (Fase 3.8); vuoto = il flusso qui sotto.
	Others []Client
	// Altri device come flusso (ripiego): frame al millisecondo, S medio,
	// variabilità degli arrivi.
	OthersPerMS, OthersS, CA float64
}

// client è un device nella simulazione: i suoi tempi e il suo stato. own è
// il device di cui si prevede il costo: solo i suoi frame si contano.
type client struct {
	p   Client
	own bool

	inflight int    // frame in volo (finestra)
	errors   []*job // errori ricevuti, da finire al prossimo drain
	captured float64
}

// simResult è ciò che la simulazione ha contato sui frame del device nati
// nella finestra di misura.
type simResult struct {
	FramesPerSec, ServerFramesPerSec, ServerShare float64
	RejectedPerSec, WindowFullPerSec              float64
	WaitMS, ServerMS, LocalMS, TotalMS, P95MS     float64
	Rho                                           float64 // slot occupati / (c × durata)
}

// Tipi di evento.
const (
	evDrain      = iota // il device inizia un ciclo: drain
	evPrefixDone        // il device ha calcolato il prefisso di un frame
	evArrive            // un frame del device arriva al server
	evOthers            // arriva un frame di un altro device
	evFinish            // un job termina il servizio
	evAnswer            // una risposta arriva al device
)

type event struct {
	at   float64
	kind int
	job  *job    // evArrive, evFinish, evAnswer
	cl   *client // evDrain, evPrefixDone
	seq  int     // ordine di inserimento: a pari istante, prima chi è entrato prima
}

type eventQueue []*event

func (q eventQueue) Len() int { return len(q) }
func (q eventQueue) Less(i, j int) bool {
	if q[i].at != q[j].at {
		return q[i].at < q[j].at
	}
	return q[i].seq < q[j].seq
}
func (q eventQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *eventQueue) Push(x any)   { *q = append(*q, x.(*event)) }
func (q *eventQueue) Pop() any {
	old := *q
	e := old[len(old)-1]
	*q = old[:len(old)-1]
	return e
}

// job è un frame sul server, inviato da un client; cl nil = frame del flusso
// di ripiego (scadenza infinita, nessuna risposta).
type job struct {
	cl       *client
	captured float64 // quando è nato
	deadline float64 // istante oltre cui il risultato non serve più
	sEst     float64 // S stimata dal dispatcher
	sActual  float64 // S vera di questa esecuzione
	arrived  float64
	started  float64
	failed   bool // la risposta è un errore: il device lo finirà al drain
}

// outcome è un frame del device terminato.
type outcome struct {
	captured, done float64
	served         bool
	rejected       bool // rifiutato o scaduto sul server, finito al drain
	windowFull     bool
	waitMS         float64 // in coda sul server; solo served
}

// sim è lo stato della simulazione.
type sim struct {
	p   simParams
	rng *rand.Rand
	q   eventQueue
	seq int
	now float64

	// Server.
	running []*job
	queue   []*job
	busyMS  float64 // tempo di slot occupato nella finestra di misura

	clients []*client // clients[0] è il device
	done    []outcome
}

func (s *sim) schedule(at float64, kind int, j *job) {
	s.seq++
	heap.Push(&s.q, &event{at: at, kind: kind, job: j, seq: s.seq})
}

func (s *sim) scheduleClient(at float64, kind int, c *client) {
	s.seq++
	heap.Push(&s.q, &event{at: at, kind: kind, cl: c, seq: s.seq})
}

// simulate esegue la simulazione e riassume i frame del device.
func simulate(p simParams) simResult {
	s := &sim{p: p, rng: rand.New(rand.NewPCG(simSeed, uint64(p.Slots)))}
	s.clients = []*client{{p: Client{P: p.P, X: p.X, U: p.U, A: p.A, S: p.S, DownMS: p.DownMS, Window: p.Window}, own: true}}
	for _, o := range p.Others {
		s.clients = append(s.clients, &client{p: o})
	}
	for i, c := range s.clients {
		// Sfasa le partenze su un ciclo intero (A+P), non su A soltanto: con
		// tempi costanti, partenze ravvicinate fanno sempre vincere lo stesso.
		s.scheduleClient(float64(i)*(c.p.A+c.p.P)/float64(len(s.clients)), evDrain, c)
	}
	if len(p.Others) == 0 && p.OthersPerMS > 0 {
		s.schedule(s.sample(1/p.OthersPerMS, p.CA), evOthers, nil)
	}
	end := simWarmupMS + simMeasureMS + simTailMS
	for n := 0; s.q.Len() > 0 && n < simMaxEvents; n++ {
		e := heap.Pop(&s.q).(*event)
		if e.at > end {
			break
		}
		s.now = e.at
		switch e.kind {
		case evDrain:
			s.drain(e.cl)
		case evPrefixDone:
			s.prefixDone(e.cl)
		case evArrive:
			s.arrive(e.job)
		case evOthers:
			sEst := p.OthersS
			s.arrive(&job{sEst: sEst, sActual: s.sample(sEst, p.CS), deadline: math.Inf(1)})
			s.schedule(s.now+s.sample(1/p.OthersPerMS, p.CA), evOthers, nil)
		case evFinish:
			s.finish(e.job)
		case evAnswer:
			s.answer(e.job)
		}
	}
	return s.summary()
}

// drain inizia un ciclo del client: finisce un errore ricevuto, se ce n'è,
// altrimenti acquisisce e calcola il prefisso di un frame nuovo.
func (s *sim) drain(c *client) {
	if len(c.errors) > 0 {
		j := c.errors[0]
		c.errors = c.errors[1:]
		if c.own {
			s.done = append(s.done, outcome{captured: j.captured, done: s.now + c.p.X, rejected: true})
		}
		// Rischedulare il drain gestisce anche gli errori arrivati nel frattempo.
		s.scheduleClient(s.after(c.p.X), evDrain, c)
		return
	}
	c.captured = s.now + c.p.A
	s.scheduleClient(s.after(c.p.A+c.p.P), evPrefixDone, c)
}

// prefixDone: il prefisso è pronto; il client invia il frame o lo finisce.
func (s *sim) prefixDone(c *client) {
	if c.inflight >= c.p.Window {
		if c.own {
			s.done = append(s.done, outcome{captured: c.captured, done: s.now + c.p.X, windowFull: true})
		}
		s.scheduleClient(s.after(c.p.X), evDrain, c)
		return
	}
	c.inflight++
	// La scadenza conta da quando il server legge il frame.
	j := &job{cl: c, captured: c.captured, deadline: s.now + c.p.U + c.p.X, sEst: c.p.S, sActual: s.sample(c.p.S, s.p.CS)}
	s.schedule(s.now+c.p.U, evArrive, j)
	s.scheduleClient(s.after(c.p.U), evDrain, c)
}

// answerDelay è quanto impiega una risposta a tornare al client: la latenza
// di rete, ma mai meno di simMinAnswerMS.
func (s *sim) answerDelay(c *client) float64 { return max(c.p.DownMS, simMinAnswerMS) }

// after è l'istante del prossimo evento del device dopo d ms di lavoro.
func (s *sim) after(d float64) float64 { return s.now + max(d, simMinCycleMS) }

// arrive: un frame arriva al server.
func (s *sim) arrive(j *job) {
	j.arrived = s.now
	// I frame del flusso di ripiego hanno scadenza infinita: sempre ammessi.
	if j.cl != nil && s.now+s.predictedWait()+j.sEst > j.deadline {
		j.failed = true
		s.schedule(s.now+s.answerDelay(j.cl), evAnswer, j)
		return
	}
	s.queue = append(s.queue, j)
	s.dispatch()
}

// predictedWait è l'attesa che il dispatcher prevederebbe adesso: residui
// stimati dei job in esecuzione e S stimate di quelli in coda.
func (s *sim) predictedWait() float64 {
	running := make([]time.Duration, 0, len(s.running))
	for _, j := range s.running {
		running = append(running, dur(max(0, j.sEst-(s.now-j.started))))
	}
	queued := make([]time.Duration, len(s.queue))
	for i, j := range s.queue {
		queued[i] = dur(j.sEst)
	}
	return float64(dispatch.PredictWait(s.p.Slots, running, queued)) / float64(time.Millisecond)
}

// dispatch avvia i job in testa alla coda finché ci sono slot liberi; un
// frame del device che ottiene lo slot oltre la sua scadenza torna come
// errore (come dispatch.wait).
//
// shortcut: il dispatcher vero lo toglie dalla coda alla scadenza, qui solo
// quando tocca a lui; con l'ammissione per scadenza succede di rado.
func (s *sim) dispatch() {
	for len(s.queue) > 0 && len(s.running) < s.p.Slots {
		j := s.queue[0]
		s.queue = s.queue[1:]
		if j.cl != nil && s.now > j.deadline {
			j.failed = true
			s.schedule(s.now+s.answerDelay(j.cl), evAnswer, j)
			continue
		}
		j.started = s.now
		s.running = append(s.running, j)
		s.schedule(s.now+j.sActual, evFinish, j)
	}
}

// finish: un job ha terminato il servizio.
func (s *sim) finish(j *job) {
	s.running = slices.DeleteFunc(s.running, func(r *job) bool { return r == j })
	s.busyMS += overlap(j.started, s.now, simWarmupMS, simWarmupMS+simMeasureMS)
	if j.cl != nil {
		s.schedule(s.now+s.answerDelay(j.cl), evAnswer, j)
	}
	s.dispatch()
}

// answer: la risposta di un frame arriva al client e libera la finestra.
func (s *sim) answer(j *job) {
	c := j.cl
	c.inflight--
	if j.failed {
		c.errors = append(c.errors, j)
		return
	}
	if !c.own {
		return
	}
	s.done = append(s.done, outcome{captured: j.captured, done: s.now, served: true, waitMS: j.started - j.arrived})
}

// summary riassume i frame del device nati nella finestra di misura.
func (s *sim) summary() simResult {
	var r simResult
	var served, local int
	var waitSum, serverSum, localSum float64
	var lat []float64
	for _, o := range s.done {
		if o.captured < simWarmupMS || o.captured >= simWarmupMS+simMeasureMS {
			continue
		}
		l := o.done - o.captured
		lat = append(lat, l)
		switch {
		case o.served:
			served++
			waitSum += o.waitMS
			serverSum += l
		default:
			local++
			localSum += l
			if o.rejected {
				r.RejectedPerSec++
			} else if o.windowFull {
				r.WindowFullPerSec++
			}
		}
	}
	sec := simMeasureMS / 1000
	n := served + local
	r.FramesPerSec = float64(n) / sec
	r.ServerFramesPerSec = float64(served) / sec
	r.RejectedPerSec /= sec
	r.WindowFullPerSec /= sec
	if n > 0 {
		r.ServerShare = float64(served) / float64(n)
		r.TotalMS = (serverSum + localSum) / float64(n)
		r.P95MS = Percentile(lat, 0.95)
	}
	if served > 0 {
		r.WaitMS, r.ServerMS = waitSum/float64(served), serverSum/float64(served)
	}
	if local > 0 {
		r.LocalMS = localSum / float64(local)
	}
	r.Rho = s.busyMS / (float64(s.p.Slots) * simMeasureMS)
	return r
}

// Percentile restituisce il quantile q (0..1) dei valori, con il metodo del
// rango più vicino: il valore più piccolo che ha almeno q·n valori ≤ di sé.
// 0 senza valori. Non modifica values.
func Percentile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	rank := max(1, int(math.Ceil(q*float64(len(sorted)))))
	return sorted[min(rank, len(sorted))-1]
}

// sample estrae un valore con media mean e coefficiente di variazione cv:
// costante con cv = 0, altrimenti log-normale (positiva, media e cv esatti:
// σ² = ln(1+cv²), μ = ln(mean) − σ²/2).
func (s *sim) sample(mean, cv float64) float64 {
	if cv <= 0 || mean <= 0 {
		return mean
	}
	sigma2 := math.Log1p(cv * cv)
	return math.Exp(math.Log(mean) - sigma2/2 + math.Sqrt(sigma2)*s.rng.NormFloat64())
}

// overlap è la parte dell'intervallo [a, b] dentro [lo, hi].
func overlap(a, b, lo, hi float64) float64 { return max(0, min(b, hi)-max(a, lo)) }

func dur(ms float64) time.Duration { return time.Duration(ms * float64(time.Millisecond)) }
