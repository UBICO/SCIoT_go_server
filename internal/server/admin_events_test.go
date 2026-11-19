package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

// sseEvent è un evento letto dallo stream.
type sseEvent struct {
	name string
	data map[string]any
}

// eventStream legge gli eventi di una connessione /events in una goroutine.
type eventStream struct {
	resp   *http.Response
	events chan sseEvent
	cancel context.CancelFunc
}

// openEvents apre /api/v1/admin/events su un server HTTP vero (httptest),
// perché ResponseRecorder non manda nulla al client prima della fine.
func openEvents(t *testing.T, url string) *eventStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/v1/admin/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	st := &eventStream{resp: resp, events: make(chan sseEvent, 64), cancel: cancel}
	go func() {
		defer close(st.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.data); err != nil {
					t.Errorf("data non JSON: %q", line)
				}
			case line == "":
				st.events <- ev
				ev = sseEvent{}
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		resp.Body.Close()
	})
	return st
}

// next restituisce il prossimo evento, o fallisce dopo timeout.
func (st *eventStream) next(t *testing.T, timeout time.Duration) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-st.events:
		if !ok {
			t.Fatal("lo stream si è chiuso")
		}
		return ev
	case <-time.After(timeout):
		t.Fatalf("nessun evento in %v", timeout)
	}
	return sseEvent{}
}

// nextDevice salta gli eventi "status" e restituisce il prossimo "device".
func (st *eventStream) nextDevice(t *testing.T, timeout time.Duration) sseEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ev := st.next(t, time.Until(deadline))
		if ev.name == "device" {
			return ev
		}
	}
}

// eventsServer: un Server con heartbeat a 200 ms (tick di 50 ms, test veloci)
// e un device registrato; restituisce anche la sessione del device.
func eventsServer(t *testing.T, writeTimeout time.Duration) (*Server, *httptest.Server, string) {
	t.Helper()
	s, modelHash, _ := testServer(t)
	s.cfg.HeartbeatInterval = 200 * time.Millisecond
	rec := do(t, s, http.MethodPost, "/api/v1/register", registerBody(modelHash))
	var reg api.RegisterResponse
	if err := json.NewDecoder(rec.Body).Decode(&reg); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s.AdminRoutes(testAdminToken))
	ts.Config.WriteTimeout = writeTimeout
	ts.Start()
	t.Cleanup(ts.Close)
	return s, ts, reg.SessionID
}

func TestAdminEventsSnapshot(t *testing.T) {
	_, ts, session := eventsServer(t, 0)
	st := openEvents(t, ts.URL)

	if st.resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", st.resp.StatusCode)
	}
	if ct := st.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type %q", ct)
	}
	if cc := st.resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control %q", cc)
	}

	ev := st.next(t, time.Second)
	if ev.name != "status" || ev.data["devices"] != float64(1) {
		t.Fatalf("primo evento: %s %v, voglio status con 1 device", ev.name, ev.data)
	}
	ev = st.next(t, time.Second)
	if ev.name != "device" || ev.data["device_id"] != "web_192" || ev.data["session_id"] != session {
		t.Fatalf("secondo evento: %s %v, voglio il device", ev.name, ev.data)
	}
}

func TestAdminEventsHeartbeatSendsTheDevice(t *testing.T) {
	s, ts, session := eventsServer(t, 0)
	st := openEvents(t, ts.URL)
	st.nextDevice(t, time.Second) // istantanea

	heartbeatPolicy(t, s, session)
	ev := st.nextDevice(t, time.Second)
	if ev.data["applied_policy_version"] != float64(1) || ev.data["telemetry"] == nil {
		t.Fatalf("dopo l'heartbeat: %v", ev.data)
	}
}

func TestAdminEventsNoChangeNoDevice(t *testing.T) {
	_, ts, _ := eventsServer(t, 0)
	st := openEvents(t, ts.URL)
	st.nextDevice(t, time.Second) // istantanea

	// 600 ms = 3 heartbeat interval = 12 tick: solo "status", circa 3.
	statuses := 0
	deadline := time.After(600 * time.Millisecond)
	for {
		select {
		case ev := <-st.events:
			if ev.name == "device" {
				t.Fatalf("device senza cambiamenti: %v", ev.data)
			}
			statuses++
			continue
		case <-deadline:
		}
		break
	}
	if statuses < 2 || statuses > 4 {
		t.Errorf("%d eventi status in 600 ms con intervallo 200 ms, voglio 2–4", statuses)
	}
}

func TestAdminEventsPinAndDecision(t *testing.T) {
	s, ts, _ := eventsServer(t, 0)
	st := openEvents(t, ts.URL)
	st.nextDevice(t, time.Second)

	// Il pin passa dalla porta admin stessa, come farà la dashboard.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/admin/devices/web_192/policy", strings.NewReader(`{"mode":"SPLIT","split_point":5}`))
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pin: %d", resp.StatusCode)
	}
	if ev := st.nextDevice(t, time.Second); ev.data["pinned"] != true {
		t.Fatalf("dopo il pin: %v", ev.data)
	}

	// Anche una decisione (senza heartbeat nuovo) rimanda il device.
	s.decisions.set("web_192", time.Now(), &record.Decision{Reason: "faster"})
	ev := st.nextDevice(t, time.Second)
	if dec, _ := ev.data["last_decision"].(map[string]any); dec == nil {
		t.Fatalf("dopo la decisione: %v", ev.data)
	}
}

// Il WriteTimeout del server non deve chiudere lo stream.
func TestAdminEventsOutliveWriteTimeout(t *testing.T) {
	_, ts, _ := eventsServer(t, 100*time.Millisecond)
	st := openEvents(t, ts.URL)
	st.nextDevice(t, time.Second)

	time.Sleep(500 * time.Millisecond) // 5 volte il WriteTimeout
	for len(st.events) > 0 {
		<-st.events // eventi arrivati nel frattempo
	}
	if ev := st.next(t, time.Second); ev.name != "status" {
		t.Fatalf("dopo il WriteTimeout: %s", ev.name)
	}
}

// Con il context cancellato (client andato via o server che si ferma)
// l'handler esce. Con ResponseRecorder, che non supporta SetWriteDeadline:
// l'handler deve andare avanti lo stesso.
func TestAdminEventsStopsWithTheContext(t *testing.T) {
	s, _, _ := eventsServer(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/admin/events", nil)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleAdminEvents(rec, req)
	}()
	time.Sleep(120 * time.Millisecond) // qualche tick
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("l'handler non esce con il context cancellato")
	}
	// Dopo l'uscita il corpo si può leggere senza race.
	if body := rec.Body.String(); !strings.HasPrefix(body, "event: status\ndata: {") || !strings.Contains(body, "event: device\n") {
		t.Fatalf("corpo:\n%s", body)
	}
}

func TestAdminEventsUnauthorized(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)
	if rec := adminReq(h, http.MethodGet, "/api/v1/admin/events", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("senza credenziali: %d, voglio 401", rec.Code)
	}
}
