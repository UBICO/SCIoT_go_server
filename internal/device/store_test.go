package device

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/UBICO/sciot-go-server/internal/api"
)

var fomo = api.ModelInfo{
	ModelKey:            "fomo_96x96",
	NumLayers:           59,
	LastOffloadingLayer: 58,
	InputSizeBytes:      18432,
}

// newTestStore crea uno Store con un orologio finto fermo su t0.
func newTestStore(t0 time.Time) *Store {
	s := NewStore()
	s.now = func() time.Time { return t0 } // funzione anonima ("closure")
	return s
}

func TestRegisterNewDevice(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s := newTestStore(t0)

	d := s.Register("web_192", "python", fomo, api.Capabilities{RawInputSplit: true})

	if d.ID != "web_192" || d.Type != "python" || d.Model != fomo || !d.Capabilities.RawInputSplit {
		t.Errorf("campi errati: %+v", d)
	}
	if d.SessionID == "" {
		t.Error("SessionID vuoto")
	}
	if d.Policy.Mode != api.ModeLocal || d.Policy.SplitPoint != nil || d.Policy.PolicyVersion != 1 {
		t.Errorf("policy iniziale errata: %+v", d.Policy)
	}
	// I time.Time si confrontano con Equal, non con ==.
	if !d.RegisteredAt.Equal(t0) || !d.LastSeen.Equal(t0) {
		t.Errorf("timestamp errati: registered=%v lastSeen=%v", d.RegisteredAt, d.LastSeen)
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
}

func TestReRegisterReplacesState(t *testing.T) {
	s := newTestStore(time.Now())
	first := s.Register("web_192", "python", fomo, api.Capabilities{})

	yolo := api.ModelInfo{ModelKey: "yolo_small", NumLayers: 30, LastOffloadingLayer: 29}
	second := s.Register("web_192", "python", yolo, api.Capabilities{})

	if second.SessionID == first.SessionID {
		t.Error("una ri-registrazione deve generare un nuovo SessionID")
	}
	got, ok := s.Get("web_192")
	if !ok {
		t.Fatal("device non trovato dopo la ri-registrazione")
	}
	if got.Model.ModelKey != "yolo_small" || got.SessionID != second.SessionID {
		t.Errorf("stato non sostituito: %+v", got)
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1 (stesso device)", s.Len())
	}
}

func TestGetUnknown(t *testing.T) {
	s := NewStore()
	if _, ok := s.Get("sconosciuto"); ok {
		t.Error("Get di un device sconosciuto deve restituire false")
	}
}

func TestGetReturnsCopy(t *testing.T) {
	s := NewStore()
	s.Register("web_192", "python", fomo, api.Capabilities{})

	d, _ := s.Get("web_192")
	d.Policy.Mode = api.ModeSplit // modifico la copia...

	again, _ := s.Get("web_192")
	if again.Policy.Mode != api.ModeLocal { // ...lo Store non deve cambiare
		t.Error("modificare il valore restituito non deve alterare lo Store")
	}
}

// TestConcurrentRegister registra 100 device in parallelo.
// Ha senso soprattutto con il race detector: go test -race ./...
func TestConcurrentRegister(t *testing.T) {
	s := NewStore()

	// WaitGroup aspetta che tutte le goroutine abbiano finito.
	// wg.Go (Go ≥ 1.25) avvia la funzione in una nuova goroutine.
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			id := fmt.Sprintf("dev-%d", i)
			s.Register(id, "python", fomo, api.Capabilities{})
			s.Get(id)
			s.Len()
		})
	}
	wg.Wait()

	if s.Len() != 100 {
		t.Errorf("Len = %d, want 100", s.Len())
	}
}

// --- Heartbeat ---------------------------------------------------------------

func ptr[T any](v T) *T { return &v } // funzione generica: &v di qualunque tipo

func TestHeartbeatUpdatesState(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)
	s := newTestStore(t0)
	d := s.Register("web_96", "python", fomo, api.Capabilities{})

	s.now = func() time.Time { return t1 } // il tempo avanza di 1 s
	tel := api.Telemetry{InferenceCount: 47, InferenceTimeMS: ptr(41.7), InferenceTimeMaxMS: ptr(60.0), CPULoad: ptr(0.42)}

	policy, err := s.Heartbeat("web_96", d.SessionID, 1, tel)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if policy != d.Policy {
		t.Errorf("policy = %+v, want %+v", policy, d.Policy)
	}

	got, _ := s.Get("web_96")
	if !got.LastSeen.Equal(t1) {
		t.Errorf("LastSeen = %v, want %v (heartbeat non salvato nella map?)", got.LastSeen, t1)
	}
	if !got.RegisteredAt.Equal(t0) {
		t.Errorf("RegisteredAt cambiato: %v", got.RegisteredAt)
	}
	if got.AppliedPolicyVersion != 1 {
		t.Errorf("AppliedPolicyVersion = %d, want 1", got.AppliedPolicyVersion)
	}
	if got.LastTelemetry.InferenceCount != 47 || got.LastTelemetry.CPULoad == nil || *got.LastTelemetry.CPULoad != 0.42 {
		t.Errorf("LastTelemetry = %+v", got.LastTelemetry)
	}
}

func TestHeartbeatSessionExpired(t *testing.T) {
	s := newTestStore(time.Now())
	old := s.Register("web_96", "python", fomo, api.Capabilities{})
	current := s.Register("web_96", "python", fomo, api.Capabilities{}) // nuova sessione

	cases := []struct {
		name, id, session string
	}{
		{"device sconosciuto", "mai-visto", "x"},
		{"sessione vecchia", "web_96", old.SessionID},
		{"sessione vuota", "web_96", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Heartbeat(tc.id, tc.session, 1, api.Telemetry{})
			if !errors.Is(err, ErrSessionExpired) {
				t.Errorf("err = %v, want ErrSessionExpired", err)
			}
		})
	}

	// Un heartbeat rifiutato non deve toccare la sessione corrente.
	got, _ := s.Get("web_96")
	if got.SessionID != current.SessionID || got.AppliedPolicyVersion != 0 {
		t.Errorf("stato modificato da un heartbeat rifiutato: %+v", got)
	}
}

func TestReRegisterResetsHeartbeatState(t *testing.T) {
	s := newTestStore(time.Now())
	d := s.Register("web_96", "python", fomo, api.Capabilities{})
	if _, err := s.Heartbeat("web_96", d.SessionID, 1, api.Telemetry{InferenceCount: 10}); err != nil {
		t.Fatal(err)
	}

	again := s.Register("web_96", "python", fomo, api.Capabilities{})
	if again.AppliedPolicyVersion != 0 || again.LastTelemetry.InferenceCount != 0 {
		t.Errorf("la ri-registrazione deve azzerare lo stato dell'heartbeat: %+v", again)
	}
}

func TestConcurrentHeartbeat(t *testing.T) {
	s := NewStore()
	d := s.Register("web_96", "python", fomo, api.Capabilities{})

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if _, err := s.Heartbeat("web_96", d.SessionID, 1, api.Telemetry{InferenceCount: 1}); err != nil {
				t.Error(err)
			}
			s.Get("web_96")
		})
	}
	wg.Wait()
}
