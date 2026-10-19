package server

import (
	"net/http"
	"sync"
	"testing"

	"github.com/UBICO/SCIoT_go_server/internal/api"
)

// fakeNotifier registra le chiamate del control plane.
type fakeNotifier struct {
	mu       sync.Mutex
	policies []api.Policy
	ended    []string
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

func TestHooksWithoutDataPlane(t *testing.T) {
	// Senza SetStream il notifier è nil: registrazione e admin funzionano.
	s, modelHash := adminServer(t)
	registerSession(t, s, modelHash)
	if rec := do(t, s, http.MethodPost, "/api/v1/admin/devices/web_192/policy", `{"mode":"LOCAL","split_point":null}`); rec.Code != http.StatusOK {
		t.Errorf("status %d: %s", rec.Code, rec.Body)
	}
}
