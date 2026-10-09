package device

import (
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
