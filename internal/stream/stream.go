// Package stream implementa il data plane: uno stream WebSocket per device
// (GET /api/v1/stream) che trasporta frame e risultati.
//
// La libreria WebSocket (github.com/coder/websocket) è usata solo in questo
// package: se un giorno andasse sostituita, si riscrive solo qui.
//
// L'esecuzione dei frame non sta qui: il Handler la delega a un Executor.
// Nel 2.4 l'Executor vero (dispatcher + worker gRPC); nei test FakeExecutor.
package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/netmeasure"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

// ---------------------------------------------------------------------------
// Dipendenze (interfacce piccole, definite da chi le usa)
// ---------------------------------------------------------------------------

// Session è ciò che lo stream deve sapere del device.
type Session struct {
	ModelKey string
	Policy   api.Policy
	// Postprocess è come decodificare l'output: gli stessi parametri che il
	// device ha ricevuto alla registrazione.
	Postprocess api.Postprocess
	// Probe: il device ha dichiarato la capability "probe" (risponde a
	// probe_request). Senza, il server non gliela chiede mai.
	Probe bool
}

// Sessions verifica una sessione e ne restituisce lo stato attuale.
type Sessions interface {
	Lookup(deviceID, sessionID string) (Session, bool)
}

// Catalog conosce, per ogni modello, i byte attesi a ogni split point
// (nel 2.4 vengono dal Describe del worker).
type Catalog interface {
	Segments(modelKey string) ([]api.SegmentSize, bool)
}

// Job è un frame da eseguire.
type Job struct {
	DeviceID   string
	ModelKey   string
	SplitPoint int
	Tensor     []byte
	Deadline   time.Time // oltre questo istante il risultato non serve più
	WantRaw    bool
	// Postprocess decodifica l'output in detection; Decoder vuoto = nessuna.
	Postprocess api.Postprocess
}

// Outcome è il risultato dell'esecuzione di un Job.
type Outcome struct {
	Output      []byte // tensore grezzo; può essere nil se !WantRaw
	Detections  []api.Detection
	LayerMS     []float64
	QueueWaitMS float64
	// Admission è la previsione con cui il job è stato ammesso; nil se
	// l'Executor non ne fa (FakeExecutor, Client senza dispatcher).
	Admission *Admission
}

// Admission è ciò che il dispatcher ha previsto per un job: serve a valutare
// offline quanto sbaglia la previsione (la registrazione, internal/record).
type Admission struct {
	PredictedWait time.Duration // attesa in coda prevista
	Service       time.Duration // S usata nella previsione
	Known         bool          // S da misure; false = stima prudente
}

// JobError accompagna un errore dell'Executor con la previsione del
// dispatcher: per un frame rifiutato (ErrBusy) o scaduto in coda
// (ErrDeadlineExceeded). Il messaggio e errors.Is restano quelli di Err.
type JobError struct {
	Admission Admission
	Err       error
}

func (e *JobError) Error() string { return e.Err.Error() }
func (e *JobError) Unwrap() error { return e.Err }

// Errori che un Executor può restituire; il Handler li traduce nei codici
// del protocollo (vedi errorCode).
var (
	ErrBusy              = errors.New("server busy")
	ErrDeadlineExceeded  = errors.New("deadline exceeded")
	ErrWorkerUnavailable = errors.New("worker unavailable")
)

// Executor esegue un frame dal suo split point alla fine del modello.
type Executor interface {
	Execute(ctx context.Context, job Job) (Outcome, error)
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// Config contiene i limiti dello stream.
type Config struct {
	MaxInflight     int           // frame in volo per device
	MaxMessageBytes int           // dimensione massima di un messaggio
	DefaultDeadline time.Duration // per i frame senza deadline_ms
	WriteTimeout    time.Duration // per scrivere un messaggio al device
	PingInterval    time.Duration // ping per la latenza; 0 = mai
	// ProbeInterval: si chiede un probe a un device (con la capability) che
	// non invia byte misurabili da più di così; 0 = mai.
	ProbeInterval time.Duration
	// ProbeUnknownInterval: lo stesso, finché la banda del device non ha una
	// stima (vedi probeInterval); 0 = sempre ProbeInterval.
	ProbeUnknownInterval time.Duration
	ProbeBytes           int // dimensione del probe
	// BandwidthTau è la costante di tempo della stima della banda.
	BandwidthTau time.Duration
}

// DefaultConfig restituisce i valori di partenza della specifica.
func DefaultConfig() Config {
	return Config{
		MaxInflight:     3,
		MaxMessageBytes: 8 << 20, // 8 MiB: il frame più grande di yolo-small è 3,2 MB
		DefaultDeadline: time.Second,
		WriteTimeout:    5 * time.Second,
		PingInterval:    time.Second,
		ProbeInterval:   30 * time.Second,
		// 3 probe in 15 s: i campioni che "non limitante" chiede nella sua
		// finestra (netmeasure.NotLimitingMinSamples in NotLimitingWindow).
		ProbeUnknownInterval: 5 * time.Second,
		// 8 letture da 64 KiB (netmeasure.ReadChunk): abbastanza per misurare,
		// ~0,4 s di link a 10 Mbit/s ogni ProbeInterval.
		ProbeBytes:   8 * netmeasure.ReadChunk,
		BandwidthTau: netmeasure.DefaultBandwidthTau,
	}
}

// Handler è l'http.Handler di GET /api/v1/stream.
type Handler struct {
	sessions Sessions
	catalog  Catalog
	exec     Executor
	cfg      Config
	now      func() time.Time

	// afterSend, se non nil, viene chiamata dopo l'invio di un risultato o di
	// un errore. Solo per i test: simula una goroutine lenta.
	afterSend func()

	// rec registra un evento per ogni frame; nil = registrazione spenta.
	rec *record.Recorder

	// mu protegge conns: lo stream aperto di ogni device (al massimo uno).
	// Lo scrivono ServeHTTP (apertura e chiusura), lo leggono NotifyPolicy ed
	// EndSession, chiamati dagli handler del control plane.
	mu    sync.Mutex
	conns map[string]*conn
	// nets è la rete di ogni device che ha aperto almeno uno stream (stesso
	// mutex). Sopravvive alla chiusura dello stream e alle nuove sessioni: la
	// rete è del device, non della connessione.
	nets map[string]*netmeasure.Estimator
}

// NewHandler crea un Handler.
func NewHandler(sessions Sessions, catalog Catalog, exec Executor, cfg Config) *Handler {
	return &Handler{
		sessions: sessions, catalog: catalog, exec: exec, cfg: cfg, now: time.Now,
		conns: map[string]*conn{}, nets: map[string]*netmeasure.Estimator{},
	}
}

// SetRecorder attiva la registrazione degli esiti dei frame (internal/record).
// Va chiamata prima di servire richieste.
func (h *Handler) SetRecorder(r *record.Recorder) { h.rec = r }

// conn è lo stato di uno stream aperto.
type conn struct {
	ws        *websocket.Conn
	deviceID  string
	sessionID string
	modelKey  string
	// postprocess è fissato all'apertura dello stream: dipende dal modello
	// della sessione, che non cambia finché la sessione vive.
	postprocess api.Postprocess
	sizes       map[int]int // split point → byte attesi
	net         *netmeasure.Estimator

	probe bool // il device risponde ai probe (capability)

	mu       sync.Mutex
	inflight map[uint64]bool // sequence_id dei frame in esecuzione
	// Probe in attesa di risposta (stesso mutex): probeID 0 = nessuno.
	probeID     uint64
	probeSentAt time.Time
	lastProbeID uint64 // per numerare i probe di questo stream

	// ready viene chiuso dopo l'invio dell'hello: NotifyPolicy lo aspetta,
	// perché l'hello deve essere il primo messaggio dello stream.
	ready chan struct{}

	wg sync.WaitGroup // frame in esecuzione, da attendere prima di chiudere
}

// ServeHTTP verifica la sessione, fa l'upgrade a WebSocket, invia "hello" e
// legge i frame finché il device non chiude.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	deviceID, sessionID := r.Header.Get(api.HeaderDevice), r.Header.Get(api.HeaderSession)
	if deviceID == "" || sessionID == "" {
		writeHTTPError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest,
			fmt.Sprintf("%s and %s are required", api.HeaderDevice, api.HeaderSession))
		return
	}
	sess, ok := h.sessions.Lookup(deviceID, sessionID)
	if !ok {
		writeHTTPError(w, http.StatusConflict, api.ErrCodeSessionExpired, "session expired")
		return
	}
	segments, ok := h.catalog.Segments(sess.ModelKey)
	if !ok {
		writeHTTPError(w, http.StatusServiceUnavailable, api.ErrCodeWorkerUnavailable,
			fmt.Sprintf("no data plane for model %q", sess.ModelKey))
		return
	}

	// Da qui in poi la connessione HTTP diventa un WebSocket: niente più
	// risposte HTTP, solo messaggi e codici di chiusura.
	// Compressione disattivata (default di AcceptOptions) e da lasciare così:
	// la misura della banda conta i byte del messaggio (internal/netmeasure).
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("stream upgrade: device=%s: %v", deviceID, err)
		return
	}
	defer ws.CloseNow() // rete di sicurezza: Close ordinato più sotto
	// Il limite predefinito della libreria è 32 KB: un frame è di MB.
	ws.SetReadLimit(int64(h.cfg.MaxMessageBytes))

	c := &conn{
		ws: ws, deviceID: deviceID, sessionID: sessionID, modelKey: sess.ModelKey,
		postprocess: sess.Postprocess,
		sizes:       make(map[int]int, len(segments)), inflight: make(map[uint64]bool),
		ready: make(chan struct{}), net: h.estimator(deviceID), probe: sess.Probe,
	}
	for _, s := range segments {
		c.sizes[s.SplitPoint] = s.InputBytes
	}

	// Lo stream si registra PRIMA dell'hello: quando il device lo riceve,
	// EndSession, NotifyPolicy e CloseAll lo trovano già.
	if old := h.register(c); old != nil {
		// Un device ha al massimo uno stream: il precedente viene chiuso. In
		// una goroutine: il closing handshake può attendere secondi un device
		// che non risponde, e intanto questo stream deve già leggere.
		go old.ws.Close(websocket.StatusCode(api.CloseReplaced), "replaced by a newer stream")
	}
	defer h.unregister(c)

	// Una nuova registrazione arrivata tra il primo Lookup e register non ha
	// trovato questo stream da chiudere (EndSession): si ricontrolla. Da qui
	// in poi ogni nuova registrazione lo trova nel registro.
	sess, ok = h.sessions.Lookup(deviceID, sessionID)
	if !ok {
		ws.Close(websocket.StatusCode(api.CloseSessionExpired), "session expired")
		return
	}

	ctx := r.Context()
	if err := h.writeJSON(ctx, c, api.Hello{
		Type: api.MsgHello, MaxInflight: h.cfg.MaxInflight, MaxMessageBytes: h.cfg.MaxMessageBytes,
		Policy: sess.Policy, Segments: segments,
	}); err != nil {
		return
	}
	close(c.ready)

	// Ping e probe vivono quanto readLoop: Ping aspetta il pong che legge
	// readLoop, e la risposta a un probe arriva da readLoop.
	bgCtx, stopBg := context.WithCancel(ctx)
	var bgDone sync.WaitGroup
	if h.cfg.PingInterval > 0 {
		bgDone.Go(func() { h.pingLoop(bgCtx, c) })
	}
	if c.probe && h.cfg.ProbeInterval > 0 {
		bgDone.Go(func() { h.probeLoop(bgCtx, c) })
	}

	h.readLoop(ctx, c)
	stopBg()
	bgDone.Wait()
	c.wg.Wait() // non chiudere mentre dei frame stanno ancora scrivendo
	ws.Close(websocket.StatusNormalClosure, "")
}

// readLoop legge i messaggi del device. Ogni frame valido viene eseguito in
// una goroutine propria: il device non aspetta il risultato prima di inviare
// il frame successivo (client asincrono).
//
// La lettura è in due tempi per misurare la ricezione: Reader ritorna quando
// arriva l'inizio del messaggio (l'attesa tra un frame e l'altro non conta),
// ReadMessage legge il resto e lo cronometra.
func (h *Handler) readLoop(ctx context.Context, c *conn) {
	for {
		typ, r, err := c.ws.Reader(ctx)
		if err != nil {
			return // chiusura del device o errore di rete
		}
		msg, sample, err := netmeasure.ReadMessage(r, h.now(), h.now)
		if err != nil {
			return // errore di rete o messaggio troppo grande (SetReadLimit)
		}
		received := h.now()

		if typ != websocket.MessageBinary {
			h.sendError(ctx, c, 0, api.ErrCodeInvalidFrame, "frames must be binary messages")
			continue
		}
		// Ogni messaggio binario (frame o probe) misura la rete, anche se poi
		// viene rifiutato.
		c.net.Observe(received, sample)
		rawHeader, payload, err := api.DecodeBinary(msg)
		if err != nil {
			h.sendError(ctx, c, 0, api.ErrCodeInvalidFrame, err.Error())
			continue
		}
		var hdr api.FrameHeader
		if err := json.Unmarshal(rawHeader, &hdr); err != nil {
			h.sendError(ctx, c, 0, api.ErrCodeInvalidFrame, "invalid frame header: "+err.Error())
			continue
		}
		if hdr.Type == api.MsgProbe {
			h.handleProbe(ctx, c, rawHeader, len(payload), sample)
			continue
		}
		expected := func(sp int) (int, bool) { n, ok := c.sizes[sp]; return n, ok }
		if err := hdr.Validate(len(payload), expected); err != nil {
			h.sendError(ctx, c, hdr.SequenceID, api.ErrCodeInvalidFrame, err.Error())
			continue
		}
		if code, reason := c.acquire(hdr.SequenceID, h.cfg.MaxInflight); code != "" {
			h.sendError(ctx, c, hdr.SequenceID, code, reason)
			continue
		}

		// Il frame resta "in volo" fino alla fine dell'esecuzione: lo libera
		// handleFrame, PRIMA di inviare l'esito (vedi lì).
		c.wg.Go(func() {
			h.handleFrame(ctx, c, hdr, payload, received, sample)
		})
	}
}

// pingLoop misura la latenza con un ping ogni PingInterval, finché ctx vive.
func (h *Handler) pingLoop(ctx context.Context, c *conn) {
	t := time.NewTicker(h.cfg.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// Il pong si aspetta anche oltre l'intervallo (netmeasure.PingTimeout):
		// intanto i tick si saltano, un ping alla volta.
		pctx, cancel := context.WithTimeout(ctx, max(netmeasure.PingTimeout, h.cfg.PingInterval))
		start := h.now()
		err := c.ws.Ping(pctx)
		end := h.now()
		cancel()
		if err == nil {
			c.net.ObservePing(end, end.Sub(start))
		}
	}
}

// acquire registra un frame in volo. Restituisce un codice di errore se il
// device ha superato la finestra o se il sequence_id è già in volo.
func (c *conn) acquire(seq uint64, maxInflight int) (code, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[seq] {
		return api.ErrCodeInvalidFrame, fmt.Sprintf("sequence_id %d already in flight", seq)
	}
	if len(c.inflight) >= maxInflight {
		return api.ErrCodeInflightLimit, fmt.Sprintf("%d frames already in flight", len(c.inflight))
	}
	c.inflight[seq] = true
	return "", ""
}

func (c *conn) release(seq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, seq)
}

// handleFrame esegue un frame e invia al device il risultato o l'errore.
func (h *Handler) handleFrame(ctx context.Context, c *conn, hdr api.FrameHeader, payload []byte,
	received time.Time, sample netmeasure.Sample) {
	deadline := received.Add(h.cfg.DefaultDeadline)
	if hdr.DeadlineMS != nil {
		deadline = received.Add(time.Duration(*hdr.DeadlineMS * float64(time.Millisecond)))
	}

	out, err := h.exec.Execute(ctx, Job{
		DeviceID:    c.deviceID,
		ModelKey:    c.modelKey,
		Postprocess: c.postprocess,
		SplitPoint:  hdr.SplitPoint,
		Tensor:      payload,
		Deadline:    deadline,
		WantRaw:     hdr.WantRaw,
	})
	// Il frame esce dalla finestra PRIMA che il device riceva l'esito: un
	// device che, appena ricevuto il risultato, invia il frame successivo
	// deve trovare il posto libero. Liberarlo dopo l'invio (per esempio con
	// un defer) lascia una finestra in cui il nuovo frame viene rifiutato con
	// inflight_limit anche se il device rispetta max_inflight.
	c.release(hdr.SequenceID)
	if h.afterSend != nil {
		defer h.afterSend()
	}

	if err != nil {
		code, message := errorCode(err), err.Error()
		// server_busy e deadline_exceeded sono esiti previsti dell'ammissione
		// sotto carico: niente log (si vedranno nelle metriche) e il messaggio,
		// scritto da noi, va al device così com'è. Un errore inatteso invece
		// va nel log con i dettagli, e al device arriva un messaggio generico.
		if code == api.ErrCodeWorkerUnavailable {
			log.Printf("stream: device=%s seq=%d: execute: %v", c.deviceID, hdr.SequenceID, err)
			message = "worker unavailable"
		}
		h.rec.Record(frameEvent(c, hdr, len(payload), deadline.Sub(received), sample, code, nil, nil, err))
		h.sendError(ctx, c, hdr.SequenceID, code, message)
		return
	}

	// La policy può essere cambiata mentre il frame era in esecuzione: si
	// rilegge quella attuale. Se la sessione non esiste più resta nil.
	var serverPolicyVersion *uint64
	if sess, ok := h.sessions.Lookup(c.deviceID, c.sessionID); ok {
		v := sess.Policy.PolicyVersion
		serverPolicyVersion = &v
	}

	serverMS := float64(h.now().Sub(received)) / float64(time.Millisecond)
	receiveMS := float64(sample.Receive) / float64(time.Millisecond)
	result := api.InferenceResult{
		Schema:              api.ResultSchema,
		Type:                api.MsgResult,
		DeviceID:            c.deviceID,
		SessionID:           c.sessionID,
		SequenceID:          hdr.SequenceID,
		ModelKey:            c.modelKey,
		Mode:                api.ModeSplit,
		SplitPoint:          &hdr.SplitPoint,
		PolicyVersion:       hdr.PolicyVersion,
		ServerPolicyVersion: serverPolicyVersion,
		ComputedBy:          "server",
		CapturedAt:          hdr.CapturedAt,
		Timing: api.Timing{
			ReceiveMS:   &receiveMS,
			QueueWaitMS: &out.QueueWaitMS,
			ServerMS:    &serverMS,
			LayerMS:     out.LayerMS,
		},
		Detections: out.Detections,
	}

	h.rec.Record(frameEvent(c, hdr, len(payload), deadline.Sub(received), sample, record.OutcomeResult,
		&out, &result.Timing, nil))

	var raw []byte
	if hdr.WantRaw {
		raw = out.Output
	}
	if err := h.writeBinary(ctx, c, result, raw); err != nil {
		log.Printf("stream: device=%s seq=%d: write result: %v", c.deviceID, hdr.SequenceID, err)
	}
}

// frameEvent costruisce l'evento di registrazione di un frame.
//
//   - outcome: record.OutcomeResult o il codice d'errore del protocollo;
//   - out e timing: solo per un risultato (nil per un errore);
//   - err: solo per un errore (nil per un risultato).
func frameEvent(c *conn, hdr api.FrameHeader, requestBytes int, deadline time.Duration,
	sample netmeasure.Sample, outcome string, out *Outcome, timing *api.Timing, err error) *record.Frame {
	toMS := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

	var waitedShare float64
	if sample.Bytes > 0 {
		waitedShare = float64(sample.WaitedBytes) / float64(sample.Bytes)
	}
	f := &record.Frame{
		DeviceID:           c.deviceID,
		SessionID:          c.sessionID,
		SequenceID:         hdr.SequenceID,
		PolicyVersion:      hdr.PolicyVersion,
		SplitPoint:         hdr.SplitPoint,
		RequestBytes:       requestBytes,
		DeadlineMS:         toMS(deadline),
		ReceiveMS:          toMS(sample.Receive),
		ReceiveWaitedShare: waitedShare,
		ReceiveValid:       sample.Valid,
		Outcome:            outcome,
	}

	// La previsione viene dall'esito se il job è stato eseguito, altrimenti
	// dall'errore (rifiutato o scaduto in coda). errors.As trova un *JobError
	// anche dentro altri errori che lo avvolgono. Senza nessuna delle due (per
	// esempio worker non disponibile) resta nil.
	var adm *Admission
	var je *JobError
	switch {
	case out != nil && out.Admission != nil:
		adm = out.Admission
	case errors.As(err, &je):
		adm = &je.Admission
	}
	if adm != nil {
		f.Admission = &record.Admission{
			PredictedWaitMS: toMS(adm.PredictedWait),
			ServiceMS:       toMS(adm.Service),
			Known:           adm.Known,
		}
	}

	if out != nil {
		f.Timing = timing
		n := len(out.Detections)
		f.Detections = &n
	}
	return f
}

// errorCode traduce un errore dell'Executor nel codice del protocollo.
func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrBusy):
		return api.ErrCodeServerBusy
	case errors.Is(err, ErrDeadlineExceeded):
		return api.ErrCodeDeadlineExceeded
	default:
		// Per il device l'esito è lo stesso (esegue LOCAL): un codice
		// sconosciuto non gli servirebbe.
		return api.ErrCodeWorkerUnavailable
	}
}

// ---------------------------------------------------------------------------
// Scrittura dei messaggi
// ---------------------------------------------------------------------------

// Conn.Write di coder/websocket si può chiamare da più goroutine insieme:
// i frame eseguiti in parallelo scrivono i loro risultati senza un mutex nostro.

func (h *Handler) writeJSON(ctx context.Context, c *conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, data)
}

func (h *Handler) writeBinary(ctx context.Context, c *conn, header any, payload []byte) error {
	data, err := json.Marshal(header)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageBinary, api.EncodeBinary(data, payload))
}

func (h *Handler) sendError(ctx context.Context, c *conn, seq uint64, code, message string) {
	if err := h.writeJSON(ctx, c, api.StreamError{
		Type: api.MsgError, SequenceID: seq, Code: code, Message: message,
	}); err != nil {
		log.Printf("stream: device=%s: write error message: %v", c.deviceID, err)
	}
}

// writeHTTPError risponde prima dell'upgrade, con lo stesso formato del control plane.
func writeHTTPError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: api.ErrorDetail{Code: code, Message: message}})
}

// ---------------------------------------------------------------------------
// Stream aperti: notifiche dal control plane
// ---------------------------------------------------------------------------

// register rende c lo stream del suo device e restituisce quello precedente
// (nil se non c'era).
func (h *Handler) register(c *conn) *conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	old := h.conns[c.deviceID]
	h.conns[c.deviceID] = c
	return old
}

// unregister toglie c, ma solo se è ancora lo stream del device: se nel
// frattempo è stato sostituito, il nuovo non va toccato.
func (h *Handler) unregister(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[c.deviceID] == c {
		delete(h.conns, c.deviceID)
	}
}

// estimator restituisce la rete del device, creandola al primo stream.
func (h *Handler) estimator(deviceID string) *netmeasure.Estimator {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.nets[deviceID]
	if !ok {
		e = netmeasure.NewEstimator(h.cfg.BandwidthTau)
		h.nets[deviceID] = e
	}
	return e
}

// Network restituisce la rete del device vista dal server; false se il
// device non ha mai aperto uno stream.
func (h *Handler) Network(deviceID string) (netmeasure.State, bool) {
	h.mu.Lock()
	e, ok := h.nets[deviceID]
	h.mu.Unlock()
	if !ok {
		return netmeasure.State{}, false
	}
	return e.Snapshot(h.now()), true
}

// Segments sono i byte attesi a ogni split point del modello (dal worker).
func (h *Handler) Segments(modelKey string) ([]api.SegmentSize, bool) {
	return h.catalog.Segments(modelKey)
}

// MaxInflight è il massimo della finestra del device (stream.max_inflight,
// uguale per tutti): quello dell'hello. La finestra di ogni device la sceglie
// l'algoritmo, nella policy (docs/OFFLOAD.md, "Window").
func (h *Handler) MaxInflight(deviceID string) int { return h.cfg.MaxInflight }

// lookup restituisce lo stream aperto del device, o nil.
func (h *Handler) lookup(deviceID string) *conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[deviceID]
}

// NotifyPolicy invia al device, se ha uno stream aperto, la sua nuova policy
// (messaggio "policy": la via veloce; l'heartbeat resta quella affidabile).
func (h *Handler) NotifyPolicy(deviceID string, p api.Policy) {
	c := h.lookup(deviceID)
	if c == nil {
		return // nessuno stream: il device la riceverà con il prossimo heartbeat
	}
	// Lo stream è registrato ma l'hello potrebbe non essere ancora partito:
	// la policy non deve precederlo. Se l'hello non parte (stream chiuso
	// subito) ci pensa l'heartbeat.
	select {
	case <-c.ready:
	case <-time.After(h.cfg.WriteTimeout):
		return
	}
	// context.Background() e non il ctx di una richiesta: la notifica parte
	// dall'handler admin, la cui richiesta finisce subito dopo. writeJSON
	// applica comunque WriteTimeout.
	if err := h.writeJSON(context.Background(), c, api.PolicyMessage{Type: api.MsgPolicy, Policy: p}); err != nil {
		// Lo stream si sta chiudendo: l'heartbeat consegnerà la policy.
		log.Printf("stream: device=%s: write policy: %v", deviceID, err)
	}
}

// EndSession chiude lo stream del device con il codice 4001 (sessione
// scaduta): va chiamato quando il device si registra di nuovo, perché lo
// stream appartiene alla sessione precedente.
func (h *Handler) EndSession(deviceID string) {
	c := h.lookup(deviceID)
	if c == nil {
		return
	}
	// Close fa il closing handshake e può attendere qualche secondo che il
	// device risponda: in una goroutine, per non rallentare la risposta alla
	// registrazione.
	go c.ws.Close(websocket.StatusCode(api.CloseSessionExpired), "session expired")
}

// CloseAll chiude tutti gli stream con il codice 1001 (server in spegnimento).
//
// http.Server.Shutdown NON chiude le connessioni WebSocket: dopo l'upgrade la
// connessione non è più gestita dal server HTTP ("hijacked"). Va chiamato
// esplicitamente prima di Shutdown.
func (h *Handler) CloseAll() {
	h.mu.Lock()
	conns := make([]*conn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Go(func() { c.ws.Close(websocket.StatusGoingAway, "server shutting down") })
	}
	wg.Wait()
}
