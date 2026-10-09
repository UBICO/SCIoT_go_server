package server

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UBICO/sciot-go-server/internal/api"
	"github.com/UBICO/sciot-go-server/internal/device"
	"github.com/UBICO/sciot-go-server/internal/registry"
)

// testServer crea un Server con un solo modello ("fomo_96x96") e restituisce
// anche l'hash del modello e lo Store, per le verifiche.
func testServer(t *testing.T) (*Server, string, *device.Store) {
	t.Helper()

	dir := t.TempDir()
	content := []byte("segmento finto")
	if err := os.WriteFile(filepath.Join(dir, "submodel_0.tflite"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(content)
	modelHash := hex.EncodeToString(sum[:])

	reg, err := registry.New(registry.Config{Models: []registry.ModelConfig{{
		ModelKey:            "fomo_96x96",
		TFLiteDir:           dir,
		LastOffloadingLayer: 58,
		InputSizeBytes:      18432,
	}}})
	if err != nil {
		t.Fatal(err)
	}

	store := device.NewStore()
	cfg := Config{HeartbeatInterval: time.Second, MaxBodyBytes: 1024}
	return New(reg, store, cfg), modelHash, store
}

// do esegue una richiesta sul router SENZA avviare un server vero:
// httptest.NewRecorder è un http.ResponseWriter che registra in memoria
// status, header e corpo della risposta.
func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

// registerBody costruisce un corpo di registrazione valido per modelHash.
func registerBody(modelHash string) string {
	return `{
		"protocol_version": 1,
		"device_id": "web_192",
		"device_type": "python",
		"model_hash": "` + modelHash + `",
		"capabilities": {"raw_input_split": true}
	}`
}

func TestRegisterOK(t *testing.T) {
	s, modelHash, store := testServer(t)

	rec := do(t, s, http.MethodPost, "/api/v1/register", registerBody(modelHash))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}

	var resp api.RegisterResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("risposta non decodificabile: %v", err)
	}
	if resp.SessionID == "" {
		t.Error("session_id vuoto")
	}
	if resp.Model.ModelKey != "fomo_96x96" || resp.Model.NumLayers != 59 {
		t.Errorf("model errato: %+v", resp.Model)
	}
	if resp.Policy.Mode != api.ModeLocal || resp.Policy.SplitPoint != nil || resp.Policy.PolicyVersion != 1 {
		t.Errorf("policy errata: %+v", resp.Policy)
	}
	if resp.HeartbeatIntervalMS != 1000 {
		t.Errorf("heartbeat_interval_ms = %d, want 1000", resp.HeartbeatIntervalMS)
	}

	// Il device deve essere nello Store, con la stessa sessione della risposta.
	d, ok := store.Get("web_192")
	if !ok {
		t.Fatal("device non salvato nello Store")
	}
	if d.SessionID != resp.SessionID || d.Type != "python" || !d.Capabilities.RawInputSplit {
		t.Errorf("stato nello Store errato: %+v", d)
	}
}

// TestRegisterErrors è table-driven: ogni caso è un corpo sbagliato e
// l'errore atteso.
func TestRegisterErrors(t *testing.T) {
	s, modelHash, store := testServer(t)

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "JSON malformato",
			body:       `{"protocol_version": 1,`,
			wantStatus: http.StatusBadRequest,
			wantCode:   api.ErrCodeInvalidRequest,
		},
		{
			name:       "chiave sconosciuta",
			body:       `{"protocol_version": 1, "device_id": "x", "model_hash": "` + modelHash + `", "devce_type": "python"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   api.ErrCodeInvalidRequest,
		},
		{
			name:       "corpo troppo grande",
			body:       `{"device_id": "` + strings.Repeat("x", 2048) + `"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   api.ErrCodeInvalidRequest,
		},
		{
			name:       "versione protocollo non supportata",
			body:       `{"protocol_version": 2, "device_id": "x", "model_hash": "` + modelHash + `"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   api.ErrCodeUnsupportedProtocol,
		},
		{
			name:       "device_id mancante",
			body:       `{"protocol_version": 1, "model_hash": "` + modelHash + `"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   api.ErrCodeInvalidRequest,
		},
		{
			name:       "model_hash mancante",
			body:       `{"protocol_version": 1, "device_id": "x"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   api.ErrCodeInvalidRequest,
		},
		{
			name:       "modello sconosciuto",
			body:       registerBody("00000000000000000000000000000000"),
			wantStatus: http.StatusNotFound,
			wantCode:   api.ErrCodeModelNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, s, http.MethodPost, "/api/v1/register", tc.body)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tc.wantStatus, rec.Body)
			}
			var resp api.ErrorResponse
			// Decode e non Unmarshal: se l'handler scrivesse DUE risposte
			// (return dimenticato), il corpo conterrebbe due JSON e il
			// controllo qui sotto lo rileverebbe.
			dec := json.NewDecoder(rec.Body)
			if err := dec.Decode(&resp); err != nil {
				t.Fatalf("errore non decodificabile: %v; body: %s", err, rec.Body)
			}
			if resp.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", resp.Error.Code, tc.wantCode)
			}
			if resp.Error.Message == "" {
				t.Error("message vuoto")
			}
			if dec.More() {
				t.Error("il corpo contiene più di una risposta JSON: manca un return?")
			}
		})
	}

	// Nessuna richiesta sbagliata deve aver registrato un device.
	if store.Len() != 0 {
		t.Errorf("Store.Len = %d, want 0", store.Len())
	}
}

func TestRegisterMethodNotAllowed(t *testing.T) {
	s, _, _ := testServer(t)
	rec := do(t, s, http.MethodGet, "/api/v1/register", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	s, _, _ := testServer(t)
	rec := do(t, s, http.MethodGet, "/healthz", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Encode aggiunge un "\n" finale: lo togliamo prima del confronto.
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Errorf("body = %s", got)
	}
}

// --- Heartbeat ---------------------------------------------------------------

// registerSession registra web_192 via HTTP e restituisce il session_id.
func registerSession(t *testing.T, s *Server, modelHash string) string {
	t.Helper()
	rec := do(t, s, http.MethodPost, "/api/v1/register", registerBody(modelHash))
	if rec.Code != http.StatusOK {
		t.Fatalf("register: status %d, body %s", rec.Code, rec.Body)
	}
	var resp api.RegisterResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp.SessionID
}

func heartbeatBody(session string) string {
	return `{
		"device_id": "web_192",
		"session_id": "` + session + `",
		"policy_version": 1,
		"mode": "LOCAL",
		"telemetry": {"inference_time_ms": 41.5, "cpu_load": 0.4}
	}`
}

func TestHeartbeatOK(t *testing.T) {
	s, modelHash, store := testServer(t)
	session := registerSession(t, s, modelHash)

	rec := do(t, s, http.MethodPost, "/api/v1/heartbeat", heartbeatBody(session))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	got := strings.TrimSpace(rec.Body.String())
	if want := `{"policy":{"mode":"LOCAL","split_point":null,"policy_version":1}}`; got != want {
		t.Errorf("body\n got: %s\nwant: %s", got, want)
	}

	d, _ := store.Get("web_192")
	if d.AppliedPolicyVersion != 1 || d.LastTelemetry.InferenceTimeMS != 41.5 {
		t.Errorf("stato non aggiornato: %+v", d)
	}
}

func TestHeartbeatErrors(t *testing.T) {
	s, modelHash, _ := testServer(t)
	session := registerSession(t, s, modelHash)

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"JSON malformato", `{"device_id":`, 400, api.ErrCodeInvalidRequest},
		{"chiave sconosciuta", strings.Replace(heartbeatBody(session), `"mode"`, `"mdoe"`, 1), 400, api.ErrCodeInvalidRequest},
		{"device_id vuoto", strings.Replace(heartbeatBody(session), `"web_192"`, `""`, 1), 400, api.ErrCodeInvalidRequest},
		{"session_id vuoto", heartbeatBody(""), 400, api.ErrCodeInvalidRequest},
		{"mode non valido", strings.Replace(heartbeatBody(session), `"LOCAL"`, `"REMOTE"`, 1), 400, api.ErrCodeInvalidRequest},
		{"telemetria fuori range", strings.Replace(heartbeatBody(session), `0.4`, `1.5`, 1), 400, api.ErrCodeInvalidRequest},
		{"sessione sconosciuta", heartbeatBody("SESSIONE-VECCHIA"), 409, api.ErrCodeSessionExpired},
		{"device sconosciuto", strings.Replace(heartbeatBody(session), `"web_192"`, `"mai-visto"`, 1), 409, api.ErrCodeSessionExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, s, http.MethodPost, "/api/v1/heartbeat", tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tc.wantStatus, rec.Body)
			}
			var resp api.ErrorResponse
			dec := json.NewDecoder(rec.Body)
			if err := dec.Decode(&resp); err != nil {
				t.Fatalf("errore non decodificabile: %v", err)
			}
			if resp.Error.Code != tc.wantCode || resp.Error.Message == "" {
				t.Errorf("errore = %+v, want code %q", resp.Error, tc.wantCode)
			}
			if dec.More() {
				t.Error("il corpo contiene più di una risposta JSON: manca un return?")
			}
		})
	}
}

// TestHeartbeatAfterReRegister: dopo una nuova registrazione, la sessione
// precedente non è più valida (es. il client si è riavviato).
func TestHeartbeatAfterReRegister(t *testing.T) {
	s, modelHash, _ := testServer(t)
	first := registerSession(t, s, modelHash)
	second := registerSession(t, s, modelHash)

	if rec := do(t, s, http.MethodPost, "/api/v1/heartbeat", heartbeatBody(first)); rec.Code != http.StatusConflict {
		t.Errorf("sessione vecchia: status = %d, want 409", rec.Code)
	}
	if rec := do(t, s, http.MethodPost, "/api/v1/heartbeat", heartbeatBody(second)); rec.Code != http.StatusOK {
		t.Errorf("sessione nuova: status = %d, want 200", rec.Code)
	}
}
