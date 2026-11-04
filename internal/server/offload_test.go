package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/offload"
)

// splitAlg porta sempre il device a SPLIT 5 e ricorda cosa ha visto.
type splitAlg struct {
	mu        sync.Mutex
	states    []offload.State
	forgotten []string
}

func (a *splitAlg) Name() string { return "split5" }
func (a *splitAlg) Decide(now time.Time, s offload.State) offload.Decision {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.states = append(a.states, s)
	k := 5
	return offload.Decision{Mode: api.ModeSplit, SplitPoint: &k, Reason: "test"}
}
func (a *splitAlg) Forget(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forgotten = append(a.forgotten, id)
}

func heartbeatPolicy(t *testing.T, s *Server, session string) api.Policy {
	t.Helper()
	rec := do(t, s, http.MethodPost, "/api/v1/heartbeat", heartbeatBody(session))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body)
	}
	var resp api.HeartbeatResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp.Policy
}

func TestAlgorithmChangesThePolicy(t *testing.T) {
	s, modelHash := adminServer(t)
	n := &fakeNotifier{}
	s.SetStream(http.NotFoundHandler(), n)
	alg := &splitAlg{}
	s.SetAlgorithm(alg)
	session := registerSession(t, s, modelHash)
	if len(alg.forgotten) != 1 {
		t.Errorf("la registrazione deve far dimenticare la sessione precedente: %v", alg.forgotten)
	}

	// La decisione arriva nella risposta dello stesso heartbeat e sullo stream.
	p := heartbeatPolicy(t, s, session)
	if p.Mode != api.ModeSplit || p.SplitPoint == nil || *p.SplitPoint != 5 || p.PolicyVersion != 2 {
		t.Fatalf("policy = %+v", p)
	}
	if len(n.policies) != 1 || n.policies[0].PolicyVersion != 2 {
		t.Errorf("notifiche = %+v", n.policies)
	}
	// L'algoritmo vede lo stato costruito dall'heartbeat.
	if st := alg.states[0]; st.DeviceID != "web_192" || st.Last != 58 || st.Split() != 58 {
		t.Errorf("stato = %+v", st)
	}
	// Al prossimo heartbeat la decisione è la stessa: nessun cambio.
	if p := heartbeatPolicy(t, s, session); p.PolicyVersion != 2 || len(n.policies) != 1 {
		t.Errorf("senza cambio: policy %+v, notifiche %d", p, len(n.policies))
	}
}

func TestAdminPinStopsTheAlgorithm(t *testing.T) {
	s, modelHash := adminServer(t)
	alg := &splitAlg{}
	s.SetAlgorithm(alg)
	session := registerSession(t, s, modelHash)

	if rec := do(t, s, http.MethodPost, "/api/v1/admin/devices/web_192/policy", `{"mode":"LOCAL","split_point":null}`); rec.Code != http.StatusOK {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body)
	}
	p := heartbeatPolicy(t, s, session)
	if p.Mode != api.ModeLocal || p.PolicyVersion != 2 || len(alg.states) != 0 {
		t.Errorf("bloccato dall'admin: policy %+v, decisioni %d", p, len(alg.states))
	}

	// Rilasciato, l'algoritmo decide di nuovo.
	rec := do(t, s, http.MethodDelete, "/api/v1/admin/devices/web_192/policy", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unpin: %d %s", rec.Code, rec.Body)
	}
	if p := heartbeatPolicy(t, s, session); p.Mode != api.ModeSplit || p.PolicyVersion != 3 {
		t.Errorf("dopo unpin: %+v", p)
	}
	if rec := do(t, s, http.MethodDelete, "/api/v1/admin/devices/mai-visto/policy", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unpin di un device sconosciuto: %d", rec.Code)
	}
}

func TestNoAlgorithmKeepsThePolicy(t *testing.T) {
	s, modelHash, _ := testServer(t)
	session := registerSession(t, s, modelHash)
	if p := heartbeatPolicy(t, s, session); p.Mode != api.ModeLocal || p.PolicyVersion != 1 {
		t.Errorf("senza algoritmo: %+v", p)
	}
}
