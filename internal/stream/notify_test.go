package stream

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/UBICO/SCIoT_go_server/internal/api"
)

// closeStatus legge finché lo stream si chiude e restituisce il codice.
func closeStatus(t *testing.T, ws *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Error("lo stream non è stato chiuso") // non Fatal: chiamata anche da una goroutine
				return -1
			}
			return websocket.CloseStatus(err)
		}
	}
}

func TestNewerStreamReplacesTheOldOne(t *testing.T) {
	h := newHarness(t, &FakeExecutor{}, testConfig())
	old := h.open(t)
	newer := h.open(t)

	if code := closeStatus(t, old); code != api.CloseReplaced {
		t.Errorf("vecchio stream chiuso con %d, atteso %d", code, api.CloseReplaced)
	}
	// La chiusura del vecchio non deve togliere il nuovo dal registro.
	eventually(t, func() bool { return h.handler.lookup("web_96") != nil })
	h.handler.NotifyPolicy("web_96", api.Policy{Mode: api.ModeLocal, PolicyVersion: 3})
	var msg api.PolicyMessage
	readText(t, newer, &msg)
	if msg.Type != api.MsgPolicy {
		t.Errorf("messaggio = %+v", msg)
	}
}

func TestNotifyPolicy(t *testing.T) {
	h := newHarness(t, &FakeExecutor{}, testConfig())
	ws := h.open(t)

	h.handler.NotifyPolicy("web_96", api.Policy{Mode: api.ModeSplit, SplitPoint: intp(7), PolicyVersion: 5})
	var msg api.PolicyMessage
	readText(t, ws, &msg)
	if msg.Type != api.MsgPolicy || msg.Policy.Mode != api.ModeSplit ||
		msg.Policy.SplitPoint == nil || *msg.Policy.SplitPoint != 7 || msg.Policy.PolicyVersion != 5 {
		t.Errorf("messaggio = %+v", msg)
	}
}

func TestEndSession(t *testing.T) {
	h := newHarness(t, &FakeExecutor{}, testConfig())
	ws := h.open(t)

	start := time.Now()
	h.handler.EndSession("web_96")
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("EndSession ha bloccato per %v: la chiusura va fatta in una goroutine", d)
	}
	if code := closeStatus(t, ws); code != api.CloseSessionExpired {
		t.Errorf("chiuso con %d, atteso %d", code, api.CloseSessionExpired)
	}
	eventually(t, func() bool { return h.handler.lookup("web_96") == nil })
}

func TestNotifyUnknownDeviceIsANoop(t *testing.T) {
	h := newHarness(t, &FakeExecutor{}, testConfig())
	h.handler.NotifyPolicy("nobody", api.Policy{Mode: api.ModeLocal, PolicyVersion: 1})
	h.handler.EndSession("nobody")
}

func TestCloseAll(t *testing.T) {
	h := newHarness(t, &FakeExecutor{}, testConfig())
	ws := h.open(t)
	// Il device legge mentre il server chiude, come un client vero: CloseAll
	// aspetta la risposta alla chiusura (closing handshake).
	code := make(chan websocket.StatusCode, 1)
	go func() { code <- closeStatus(t, ws) }()

	start := time.Now()
	h.handler.CloseAll()
	if d := time.Since(start); d > time.Second {
		t.Errorf("CloseAll ha impiegato %v", d)
	}
	if code := <-code; code != websocket.StatusGoingAway {
		t.Errorf("chiuso con %d, atteso %d", code, websocket.StatusGoingAway)
	}
}

// Regressione: ReadTimeout e WriteTimeout di http.Server non devono
// chiudere lo stream (la connessione esce da http.Server con l'upgrade).
// Se un aggiornamento di coder/websocket o di net/http cambiasse questo
// comportamento, main.go dovrebbe rivedere i suoi timeout.
func TestStreamOutlivesHTTPServerTimeouts(t *testing.T) {
	if testing.Short() {
		t.Skip("dura più di un secondo")
	}
	sessions := &fakeSessions{device: "web_96", sess: "S1", state: Session{
		ModelKey: "yolo", Policy: api.Policy{Mode: api.ModeSplit, SplitPoint: intp(4), PolicyVersion: 2},
	}}
	srv := httptest.NewUnstartedServer(NewHandler(sessions, fakeCatalog{"yolo": sizes}, &FakeExecutor{}, testConfig()))
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	h := &harness{srv: srv, sessions: sessions}

	ws := h.open(t)
	time.Sleep(time.Second) // oltre entrambi i timeout
	sendFrame(t, ws, frame(1), make([]byte, split4Bytes))
	if r, _ := readResult(t, ws); r.SequenceID != 1 {
		t.Errorf("risultato = %+v", r)
	}
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condizione non raggiunta")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// endedDuringUpgrade: la sessione è valida al primo Lookup (prima
// dell'upgrade) e scaduta al secondo, come se il device si fosse registrato
// di nuovo proprio mentre lo stream si apriva.
type endedDuringUpgrade struct {
	*fakeSessions
	calls atomic.Int32
}

func (e *endedDuringUpgrade) Lookup(deviceID, sessionID string) (Session, bool) {
	if e.calls.Add(1) > 1 {
		return Session{}, false
	}
	return e.fakeSessions.Lookup(deviceID, sessionID)
}

func TestSessionEndedDuringUpgrade(t *testing.T) {
	h := newHarness(t, &FakeExecutor{}, testConfig())
	h.srv.Config.Handler = NewHandler(&endedDuringUpgrade{fakeSessions: h.sessions},
		fakeCatalog{"yolo": sizes}, &FakeExecutor{}, testConfig())

	ws, _, err := h.dial(t, "web_96", "S1")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()
	// Niente hello: la prima lettura deve essere già la chiusura con 4001.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, msg, err := ws.Read(ctx)
	if code := websocket.CloseStatus(err); code != api.CloseSessionExpired {
		t.Errorf("prima lettura: messaggio %q, err %v; attesa la chiusura %d", msg, err, api.CloseSessionExpired)
	}
}
