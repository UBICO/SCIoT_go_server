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
//     frame subito, X ms; altrimenti lo invia in background e torna al punto
//     1: il frame occupa il link del device per byte/banda dopo quelli già
//     inviati e arriva al server una latenza dopo.
//
// Un client con la capability wait_for_window (Client.Waits, Fase 3.9,
// docs/CLIENT.md "Window full") con la finestra piena aspetta invece un
// posto quando costa meno di X. W = invio del frame in volo più vecchio + rtt
// stimato − adesso, con la stima del client (media mobile dei rtt dei
// risultati), non l'istante vero della risposta:
//
//   - prima dell'acquisizione (dopo il drain): aspetta W − A − P, se W < X;
//   - al punto 4: aspetta un posto, al massimo X, se W < X; poi invia, o
//     finisce il frame se il posto non si è liberato.
//
// Ogni risposta che libera un posto (risultato o errore) termina l'attesa
// subito; gli errori restano per il drain successivo, come nel client.
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
	// terminare. Sono minimi: su un device lento valgono almeno simCycles
	// cicli del device (simWindows), come 5 s per uno veloce.
	simWarmupMS  = 1000.0
	simMeasureMS = 5000.0
	simTailMS    = 2000.0
	// simCycles: cicli lunghi del device (A + prefisso + suffisso) nella
	// misura. Fase 3.9, Raspberry Pi allo split −1: raffiche di 3 frame ogni
	// 658 ms; in 5 s ne cadono 7 o 8, cioè 4,2 o 4,8 frame/s secondo la fase.
	// Con 30 cicli la risoluzione è ~3%, e si simulano meno frame (~90) di un
	// device veloce in 5 s (~250).
	simCycles = 30
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
	// Fase 4.2: con answer_ms misurato dal client il ritorno è quello
	// (answerDelay); questo resta il minimo. Raspberry Pi in Wi-Fi, finestra
	// 1: answer_ms p50 11,4 ms con latenza 1,7 (~9 ms del client Python).
	simMinAnswerMS = 1.0
	// simSeed rende il risultato riproducibile: la stessa registrazione dà
	// sempre le stesse previsioni.
	simSeed = 0x5c107
)

// simParams sono gli ingressi della simulazione, in millisecondi.
type simParams struct {
	P, X, U, S, A float64 // prefisso, suffisso, invio, servizio, ciclo
	DownMS        float64 // latenza di rete
	AnswerMS      float64 // answer_ms misurato; 0 = sconosciuto (answerDelay)
	Window, Slots int
	Waits         bool    // regola della finestra piena (Client.Waits)
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
	// linkFree è quando il link del client finisce di trasmettere i frame già
	// inviati: un frame nuovo parte solo dopo (FIFO, come il buffer TCP).
	linkFree float64

	// Regola della finestra piena (Client.Waits): i frame in volo (per il più
	// vecchio), la stima del rtt come la tiene il client, l'attesa in corso e
	// il suo gettone: un risveglio con un gettone vecchio è di un'attesa già
	// finita (una risposta è arrivata prima) e si ignora.
	flight  []*job
	rtt     float64
	rttSeen bool
	wait    int // waitNone, waitCapture, waitSend
	waitTok int
}

// Attese del client con la finestra piena.
const (
	waitNone    = iota
	waitCapture // prima dell'acquisizione
	waitSend    // dopo il prefisso, il frame è pronto
)

// clientRTTAlpha è il peso del rtt nuovo nella stima del client
// (split_pipeline.EWMA_ALPHA).
const clientRTTAlpha = 0.2

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
	evWake              // finisce l'attesa di un client (event.tok)
)

type event struct {
	at   float64
	kind int
	job  *job    // evArrive, evFinish, evAnswer
	cl   *client // evDrain, evPrefixDone, evWake
	tok  int     // evWake: il gettone dell'attesa
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
	sent     float64 // quando il client l'ha inviato (rtt del client)
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

	// Finestre di tempo virtuale (simWindows).
	warmupMS, measureMS, tailMS float64

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
	s.warmupMS, s.measureMS, s.tailMS = simWindows(p)
	s.clients = []*client{{p: Client{P: p.P, X: p.X, U: p.U, A: p.A, S: p.S, DownMS: p.DownMS, AnswerMS: p.AnswerMS, Window: p.Window, Waits: p.Waits}, own: true}}
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
	end := s.warmupMS + s.measureMS + s.tailMS
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
		case evWake:
			s.wake(e.cl, e.tok)
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
	if c.p.Waits && s.waitBeforeCapture(c) {
		return
	}
	s.capture(c)
}

// capture: il client acquisisce un frame (A) e ne calcola il prefisso (P).
func (s *sim) capture(c *client) {
	c.captured = s.now + c.p.A
	s.scheduleClient(s.after(c.p.A+c.p.P), evPrefixDone, c)
}

// prefixDone: il prefisso è pronto; il client invia il frame o lo finisce.
func (s *sim) prefixDone(c *client) {
	if c.inflight >= c.p.Window {
		if c.p.Waits && s.waitAfterPrefix(c) {
			return
		}
		s.finishWindowFull(c)
		return
	}
	s.send(c)
}

// finishWindowFull: il client finisce da sé il frame pronto, X ms, poi
// ricomincia il ciclo.
func (s *sim) finishWindowFull(c *client) {
	if c.own {
		s.done = append(s.done, outcome{captured: c.captured, done: s.now + c.p.X, windowFull: true})
	}
	s.scheduleClient(s.after(c.p.X), evDrain, c)
}

// eta è W, l'attesa che il client prevede per un posto: invio del frame in
// volo più vecchio + rtt stimato − adesso, mai sotto 0 (in ritardo). ok =
// false con la finestra libera o senza un rtt misurato (il client allora non
// aspetta).
func (s *sim) eta(c *client) (float64, bool) {
	if !c.rttSeen || c.inflight < c.p.Window || len(c.flight) == 0 {
		return 0, false
	}
	oldest := c.flight[0].sent
	for _, j := range c.flight[1:] {
		oldest = min(oldest, j.sent)
	}
	return max(0, oldest+c.rtt-s.now), true
}

// waitBeforeCapture applica la regola prima dell'acquisizione; true se il
// client aspetta (il ciclo riprende con wake o con una risposta).
func (s *sim) waitBeforeCapture(c *client) bool {
	w, ok := s.eta(c)
	if !ok || w >= c.p.X {
		return false
	}
	pre := w - c.p.A - c.p.P
	if pre <= 0 {
		return false
	}
	c.wait = waitCapture
	c.waitTok++
	s.scheduleWake(s.now+pre, c)
	return true
}

// waitAfterPrefix applica la regola al frame pronto con la finestra piena;
// true se il client aspetta un posto (al massimo X).
func (s *sim) waitAfterPrefix(c *client) bool {
	w, ok := s.eta(c)
	if !ok || w >= c.p.X {
		return false
	}
	c.wait = waitSend
	c.waitTok++
	s.scheduleWake(s.now+c.p.X, c)
	return true
}

// wake: è passato il tempo di un'attesa senza che una risposta la
// terminasse.
func (s *sim) wake(c *client, tok int) {
	if tok != c.waitTok || c.wait == waitNone {
		return
	}
	was := c.wait
	c.wait = waitNone
	if was == waitCapture {
		s.capture(c)
	} else {
		s.finishWindowFull(c)
	}
}

// wakeOnAnswer: una risposta ha liberato un posto; un'attesa in corso finisce
// subito.
func (s *sim) wakeOnAnswer(c *client) {
	if c.wait == waitNone {
		return
	}
	was := c.wait
	c.waitTok++ // invalida il risveglio già in coda
	c.wait = waitNone
	if was == waitCapture {
		s.capture(c)
	} else {
		s.send(c)
	}
}

func (s *sim) scheduleWake(at float64, c *client) {
	s.seq++
	heap.Push(&s.q, &event{at: at, kind: evWake, cl: c, tok: c.waitTok, seq: s.seq})
}

// send: il client invia il frame pronto, in background.
func (s *sim) send(c *client) {
	c.inflight++
	// Invio in background (Fase 3.9): ws.send ritorna quando i byte sono nel
	// buffer del kernel, il client riprende subito; il link trasmette i frame
	// uno alla volta, in ordine. U = latenza + byte/banda: solo byte/banda
	// occupa il link, la latenza si aggiunge all'arrivo.
	//
	// shortcut: buffer del kernel infinito, ws.send non blocca mai; su un link
	// lento con finestre grandi il buffer si riempie e il client si ferma:
	// modellarlo se la previsione sbaglia lì (frame/s troppo alti).
	// Con rete "non limitante" U è la sola latenza: transfer 0, il link non si
	// occupa.
	transfer := max(0, c.p.U-c.p.DownMS)
	start := max(s.now, c.linkFree)
	c.linkFree = start + transfer
	arrival := c.linkFree + c.p.DownMS
	// La scadenza conta da quando il server legge il frame.
	j := &job{cl: c, captured: c.captured, deadline: arrival + c.p.X, sEst: c.p.S, sActual: s.sample(c.p.S, s.p.CS), sent: s.now}
	c.flight = append(c.flight, j)
	s.schedule(arrival, evArrive, j)
	s.scheduleClient(s.after(0), evDrain, c)
}

// answerDelay è quanto impiega una risposta a tornare al client, dalla fine
// del lavoro del server al posto libero nella finestra. Mai meno di
// simMinAnswerMS.
//
// Con answer_ms misurato (c.p.AnswerMS > 0): answer_ms meno la latenza
// DownMS, perché answer_ms comprende anche la latenza del primo byte
// all'andata, che la simulazione ha già messo nell'arrivo del frame (send:
// arrival = linkFree + DownMS). Senza: la latenza di rete, come prima della
// Fase 4.2.
func (s *sim) answerDelay(c *client) float64 {
	if c.p.AnswerMS > 0 {
		return max(c.p.AnswerMS-c.p.DownMS, simMinAnswerMS)
	}
	return max(c.p.DownMS, simMinAnswerMS)
}

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
	s.busyMS += overlap(j.started, s.now, s.warmupMS, s.warmupMS+s.measureMS)
	if j.cl != nil {
		s.schedule(s.now+s.answerDelay(j.cl), evAnswer, j)
	}
	s.dispatch()
}

// answer: la risposta di un frame arriva al client e libera la finestra.
func (s *sim) answer(j *job) {
	c := j.cl
	c.inflight--
	c.flight = slices.DeleteFunc(c.flight, func(f *job) bool { return f == j })
	// Il client stima il rtt dai risultati, non dagli errori.
	if !j.failed {
		rtt := s.now - j.sent
		if c.rttSeen {
			rtt = clientRTTAlpha*rtt + (1-clientRTTAlpha)*c.rtt
		}
		c.rtt, c.rttSeen = rtt, true
	}
	if c.p.Waits {
		s.wakeOnAnswer(c)
	}
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
		if o.captured < s.warmupMS || o.captured >= s.warmupMS+s.measureMS {
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
	sec := s.measureMS / 1000
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
	r.Rho = s.busyMS / (float64(s.p.Slots) * s.measureMS)
	return r
}

// simWindows sono riscaldamento, misura e coda in tempo virtuale: i minimi
// fissi, o più su un device lento. Il ciclo lungo (A + prefisso + suffisso:
// un frame finito sul device) è il passo più lento del device; gli altri
// device non contano, si misurano solo i frame del device.
func simWindows(p simParams) (warmup, measure, tail float64) {
	cycle := p.A + p.P + p.X
	return max(simWarmupMS, 2*cycle), max(simMeasureMS, simCycles*cycle), max(simTailMS, 2*cycle)
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
