package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/dispatch"
	"github.com/UBICO/SCIoT_go_server/internal/netmeasure"
)

// fakeNotifier registra le chiamate del control plane.
type fakeNotifier struct {
	mu       sync.Mutex
	policies []api.Policy
	ended    []string
	network  *netmeasure.State // restituito da Network; nil = nessuno stream
}

func (f *fakeNotifier) Segments(modelKey string) ([]api.SegmentSize, bool) { return nil, false }
func (f *fakeNotifier) MaxInflight(deviceID string) int                    { return 3 }

func (f *fakeNotifier) Network(deviceID string) (netmeasure.State, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.network == nil {
		return netmeasure.State{}, false
	}
	return *f.network, true
}

func (f *fakeNotifier) NotifyPolicy(deviceID string, p api.Policy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies = append(f.policies, p)
}

func (f *fakeNotifier) EndSession(deviceID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, deviceID)
}

func TestStreamRouteOnlyWhenSet(t *testing.T) {
	s, _, _ := testServer(t)
	if rec := do(t, s, http.MethodGet, "/api/v1/stream", ""); rec.Code != http.StatusNotFound {
		t.Errorf("senza SetStream: status %d, atteso 404", rec.Code)
	}

	called := false
	s.SetStream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	}), &fakeNotifier{})
	if rec := do(t, s, http.MethodGet, "/api/v1/stream", ""); !called || rec.Code != http.StatusTeapot {
		t.Errorf("con SetStream: called %v, status %d", called, rec.Code)
	}
}

func TestSessionsAdapter(t *testing.T) {
	s, modelHash, store := testServer(t)
	session := registerSession(t, s, modelHash)
	sessions := Sessions(store)

	got, ok := sessions.Lookup("web_192", session)
	if !ok || got.ModelKey != "fomo_96x96" || got.Policy.Mode != api.ModeLocal || got.Policy.PolicyVersion != 1 {
		t.Errorf("Lookup = %+v, %v", got, ok)
	}
	if _, ok := sessions.Lookup("web_192", "old-session"); ok {
		t.Error("una sessione diversa non deve aprire lo stream")
	}
	if _, ok := sessions.Lookup("nobody", session); ok {
		t.Error("un device sconosciuto non deve aprire lo stream")
	}
}

func TestRegisterEndsTheOldStream(t *testing.T) {
	s, modelHash, _ := testServer(t)
	n := &fakeNotifier{}
	s.SetStream(http.NotFoundHandler(), n)

	registerSession(t, s, modelHash)
	registerSession(t, s, modelHash)
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.ended) != 2 || n.ended[0] != "web_192" {
		t.Errorf("EndSession chiamato per %v", n.ended)
	}
}

func TestAdminPolicyIsNotified(t *testing.T) {
	s, modelHash := adminServer(t)
	n := &fakeNotifier{}
	s.SetStream(http.NotFoundHandler(), n)
	registerSession(t, s, modelHash)

	rec := do(t, s, http.MethodPost, "/api/v1/admin/devices/web_192/policy", `{"mode":"SPLIT","split_point":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.policies) != 1 || n.policies[0].Mode != api.ModeSplit || n.policies[0].PolicyVersion != 2 {
		t.Errorf("policy notificate = %+v", n.policies)
	}
}

func TestAdminWindowAboveTheMaximum(t *testing.T) {
	s, modelHash := adminServer(t)
	n := &fakeNotifier{}
	s.SetStream(http.NotFoundHandler(), n)
	registerSession(t, s, modelHash)
	max := n.MaxInflight("web_192")

	body := fmt.Sprintf(`{"mode":"SPLIT","split_point":5,"max_inflight":%d}`, max+1)
	if rec := do(t, s, http.MethodPost, "/api/v1/admin/devices/web_192/policy", body); rec.Code != http.StatusBadRequest {
		t.Errorf("finestra oltre il massimo: status %d: %s", rec.Code, rec.Body)
	}
	body = fmt.Sprintf(`{"mode":"SPLIT","split_point":5,"max_inflight":%d}`, max)
	rec := do(t, s, http.MethodPost, "/api/v1/admin/devices/web_192/policy", body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), fmt.Sprintf(`"max_inflight":%d`, max)) {
		t.Errorf("finestra al massimo: status %d: %s", rec.Code, rec.Body)
	}
}

func TestHooksWithoutDataPlane(t *testing.T) {
	// Senza SetStream il notifier è nil: registrazione e admin funzionano.
	s, modelHash := adminServer(t)
	registerSession(t, s, modelHash)
	if rec := do(t, s, http.MethodPost, "/api/v1/admin/devices/web_192/policy", `{"mode":"LOCAL","split_point":null}`); rec.Code != http.StatusOK {
		t.Errorf("status %d: %s", rec.Code, rec.Body)
	}
}

type fixedQueue struct{ snap dispatch.Snapshot }

func (f fixedQueue) Snapshot() dispatch.Snapshot { return f.snap }

func (f fixedQueue) DeviceLoad(string) (float64, float64) { return 0, 0 }

// Estimate: S misurata per gli split in snap.Service, nient'altro.
func (f fixedQueue) Estimate(model string, split int) (dispatch.Stats, dispatch.Source, bool) {
	for _, st := range f.snap.Service {
		if st.ModelKey == model && st.SplitPoint == split {
			return st.Stats, dispatch.SourceMeasured, true
		}
	}
	return dispatch.Stats{}, "", false
}

func TestServerStatusDispatchSection(t *testing.T) {
	s, _, _ := testServer(t)
	if body := do(t, s, http.MethodGet, "/api/v1/server/status", "").Body.String(); strings.Contains(body, `"dispatch"`) {
		t.Errorf("senza data plane la sezione non deve esserci: %s", body)
	}

	s.SetQueue(fixedQueue{dispatch.Snapshot{Slots: 2, Queued: 3}})
	var resp struct {
		Dispatch *dispatch.Snapshot `json:"dispatch"`
	}
	rec := do(t, s, http.MethodGet, "/api/v1/server/status", "")
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Dispatch == nil || resp.Dispatch.Slots != 2 || resp.Dispatch.Queued != 3 {
		t.Errorf("dispatch = %+v", resp.Dispatch)
	}
}

func TestRegisterSaysWhetherThereIsADataPlane(t *testing.T) {
	dataPlane := func(s *Server, modelHash string) bool {
		t.Helper()
		rec := do(t, s, http.MethodPost, "/api/v1/register", registerBody(modelHash))
		var resp api.RegisterResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		return resp.DataPlane
	}
	s, modelHash, _ := testServer(t)
	if dataPlane(s, modelHash) {
		t.Error("senza SetStream data_plane deve essere false")
	}
	s.SetStream(http.NotFoundHandler(), &fakeNotifier{})
	if !dataPlane(s, modelHash) {
		t.Error("con SetStream data_plane deve essere true")
	}
}

func TestSessionsAdapterCarriesTheProbeCapability(t *testing.T) {
	s, modelHash, store := testServer(t)
	session := registerSession(t, s, modelHash) // registerBody dichiara "probe": true
	got, ok := Sessions(store).Lookup("web_192", session)
	if !ok || !got.Probe {
		t.Errorf("Lookup = %+v, %v: atteso Probe true", got, ok)
	}
}
