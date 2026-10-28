package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/device"
	"github.com/UBICO/SCIoT_go_server/internal/load"
	"github.com/UBICO/SCIoT_go_server/internal/registry"
	"github.com/UBICO/SCIoT_go_server/internal/registry/registrytest"
)

// testServer crea un Server con un solo modello ("fomo_96x96", 59 segmenti)
// e restituisce anche l'hash del modello e lo Store, per le verifiche.
func testServer(t *testing.T) (*Server, string, *device.Store) {
	t.Helper()

	dir := t.TempDir()
	model := registrytest.Default()
	model.Segments = make([]string, 59)
	for i := range model.Segments {
		model.Segments[i] = fmt.Sprintf("segmento finto %d", i)
	}
	model.Input = []int{1, 96, 96, 3}
	modelHash := registrytest.Write(t, dir, model)

	reg, err := registry.New(registry.Config{Models: []registry.ModelConfig{{
		ModelKey:  "fomo_96x96",
		TFLiteDir: dir,
	}}})
	if err != nil {
		t.Fatal(err)
	}

	store := device.NewStore()
	cfg := Config{HeartbeatInterval: time.Second, MaxBodyBytes: 1024}
	return New(reg, store, cfg, nil), modelHash, store
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
		"capabilities": {"probe": true, "raw_input_split": true}
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
	if d.SessionID != resp.SessionID || d.Type != "python" || !d.Capabilities.Probe {
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
		"telemetry": {"inference_count": 10, "inference_time_ms": 41.5, "inference_time_max_ms": 50, "cpu_load": 0.4}
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
	if d.AppliedPolicyVersion != 1 || d.LastTelemetry.InferenceCount != 10 {
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
		{"media sopra il massimo", strings.Replace(heartbeatBody(session), `"inference_time_max_ms": 50`, `"inference_time_max_ms": 30`, 1), 400, api.ErrCodeInvalidRequest},
		{"segmenti incoerenti", strings.Replace(heartbeatBody(session), `"cpu_load": 0.4`, `"cpu_load": 0.4, "segment_ms": [1.5], "segment_count": [0]`, 1), 400, api.ErrCodeInvalidRequest},
		{"segmenti diversi dal modello", strings.Replace(heartbeatBody(session), `"cpu_load": 0.4`, `"cpu_load": 0.4, "segment_ms": [1.5], "segment_count": [1]`, 1), 400, api.ErrCodeInvalidRequest},
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

// --- Verbose -------------------------------------------------------------------

func TestHeartbeatVerbosePrintsRow(t *testing.T) {
	s, modelHash, _ := testServer(t)
	var buf bytes.Buffer
	s.table = newHeartbeatTable(&buf)
	s.cfg.Verbose = true
	s.now = func() time.Time { return time.Date(2026, 10, 10, 14, 3, 7, 0, time.UTC) }

	session := registerSession(t, s, modelHash)
	body := heartbeatBody(session)
	if rec := do(t, s, http.MethodPost, "/api/v1/heartbeat", body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	out := buf.String()
	for _, want := range []string{
		tableHeader,
		"14:03:07.000 | web_192 ",
		"| LOCAL | 1/1 ",
		"|   10 |", // inference_count di heartbeatBody
		// HB req: il corpo esatto inviato, contato da countingReader.
		fmt.Sprintf("| %10d", len(body)),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output senza %q:\n%s", want, out)
		}
	}
}

func TestHeartbeatNotVerbosePrintsNothing(t *testing.T) {
	s, modelHash, _ := testServer(t)
	var buf bytes.Buffer
	s.table = newHeartbeatTable(&buf)

	session := registerSession(t, s, modelHash)
	do(t, s, http.MethodPost, "/api/v1/heartbeat", heartbeatBody(session))

	if buf.Len() != 0 {
		t.Errorf("con Verbose=false non deve stampare nulla, got:\n%s", buf.String())
	}
}

func TestRejectedHeartbeatPrintsNothing(t *testing.T) {
	s, _, _ := testServer(t)
	var buf bytes.Buffer
	s.table = newHeartbeatTable(&buf)
	s.cfg.Verbose = true

	do(t, s, http.MethodPost, "/api/v1/heartbeat", heartbeatBody("SESSIONE-VECCHIA"))

	if buf.Len() != 0 {
		t.Errorf("un heartbeat rifiutato non deve produrre righe, got:\n%s", buf.String())
	}
}

// --- Models ----------------------------------------------------------------------

func TestModels(t *testing.T) {
	s, modelHash, _ := testServer(t)

	rec := do(t, s, http.MethodGet, "/api/v1/models", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp api.ModelsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 1 || resp.Models[0].ModelHash != modelHash || resp.Models[0].ModelKey != "fomo_96x96" {
		t.Errorf("models = %+v", resp.Models)
	}

	rec = do(t, s, http.MethodGet, "/api/v1/models/"+modelHash, "")
	var entry api.ModelEntry
	if err := json.NewDecoder(rec.Body).Decode(&entry); err != nil || rec.Code != http.StatusOK || entry.NumLayers != 59 {
		t.Errorf("model by hash: status %d, entry %+v, err %v", rec.Code, entry, err)
	}

	rec = do(t, s, http.MethodGet, "/api/v1/models/sconosciuto", "")
	var e api.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&e); err != nil || rec.Code != http.StatusNotFound || e.Error.Code != api.ErrCodeModelNotFound {
		t.Errorf("hash sconosciuto: status %d, %+v", rec.Code, e)
	}
}

// --- Admin -----------------------------------------------------------------------

func adminServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, modelHash, _ := testServer(t)
	s.cfg.Admin = true
	return s, modelHash
}

func TestAdminDisabledByDefault(t *testing.T) {
	s, modelHash, _ := testServer(t)
	registerSession(t, s, modelHash)
	rec := do(t, s, http.MethodPost, "/api/v1/admin/devices/web_192/policy", `{"mode":"LOCAL","split_point":null}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("con Admin=false la route non deve esistere: status %d", rec.Code)
	}
}

func TestAdminSetPolicyReachesHeartbeat(t *testing.T) {
	s, modelHash := adminServer(t)
	session := registerSession(t, s, modelHash)

	rec := do(t, s, http.MethodPost, "/api/v1/admin/devices/web_192/policy", `{"mode":"SPLIT","split_point":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var resp api.SetPolicyResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.SessionID != session || resp.Policy.Mode != api.ModeSplit || resp.Policy.PolicyVersion != 2 {
		t.Errorf("risposta = %+v", resp)
	}

	rec = do(t, s, http.MethodPost, "/api/v1/heartbeat", heartbeatBody(session))
	got := strings.TrimSpace(rec.Body.String())
	if want := `{"policy":{"mode":"SPLIT","split_point":5,"policy_version":2}}`; got != want {
		t.Errorf("heartbeat\n got: %s\nwant: %s", got, want)
	}
}

func TestAdminSetPolicyErrors(t *testing.T) {
	s, modelHash := adminServer(t)
	registerSession(t, s, modelHash)

	cases := []struct {
		name, path, body string
		wantStatus       int
		wantCode         string
	}{
		{"device sconosciuto", "/api/v1/admin/devices/mai-visto/policy", `{"mode":"LOCAL","split_point":null}`, 404, api.ErrCodeDeviceNotFound},
		{"split oltre il modello", "/api/v1/admin/devices/web_192/policy", `{"mode":"SPLIT","split_point":59}`, 400, api.ErrCodeInvalidRequest},
		{"local con split_point", "/api/v1/admin/devices/web_192/policy", `{"mode":"LOCAL","split_point":2}`, 400, api.ErrCodeInvalidRequest},
		{"chiave sconosciuta", "/api/v1/admin/devices/web_192/policy", `{"mode":"LOCAL","version":9}`, 400, api.ErrCodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, s, http.MethodPost, tc.path, tc.body)
			var e api.ErrorResponse
			if err := json.NewDecoder(rec.Body).Decode(&e); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tc.wantStatus || e.Error.Code != tc.wantCode {
				t.Errorf("status %d code %q, want %d %q", rec.Code, e.Error.Code, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

// --- Server status ---------------------------------------------------------------

// fixedLoad è un LoadSource che restituisce sempre lo stesso snapshot.
type fixedLoad struct{ snap load.Snapshot }

func (f fixedLoad) Snapshot() load.Snapshot { return f.snap }

func TestServerStatus(t *testing.T) {
	s, modelHash, _ := testServer(t)
	cpu, mem := 23.5, uint64(4096<<20)
	s.load = fixedLoad{load.Snapshot{CPUPercent: &cpu, MemoryAvailableBytes: &mem, Goroutines: 9}}
	started := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	s.startedAt = started
	s.now = func() time.Time { return started.Add(90 * time.Second) }
	registerSession(t, s, modelHash)

	rec := do(t, s, http.MethodGet, "/api/v1/server/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Devices != 1 || got.Models != 1 || got.UptimeSeconds != 90 || !got.StartedAt.Equal(started) {
		t.Errorf("contatori errati: %+v", got)
	}
	if got.Load.CPUPercent == nil || *got.Load.CPUPercent != 23.5 || got.Load.Goroutines != 9 {
		t.Errorf("load = %+v", got.Load)
	}
}

func TestServerStatusWithoutLoadSource(t *testing.T) {
	s, _, _ := testServer(t) // testServer passa nil: si usa noLoad
	rec := do(t, s, http.MethodGet, "/api/v1/server/status", "")
	var got map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status %d, err %v", rec.Code, err)
	}
	// Nessuna metrica misurata: "load" contiene solo goroutines (0).
	if l := got["load"].(map[string]any); len(l) != 1 {
		t.Errorf("load senza sorgente = %v", l)
	}
}

func TestHeartbeatVerboseShowsServerLoad(t *testing.T) {
	s, modelHash, _ := testServer(t)
	var buf bytes.Buffer
	s.table = newHeartbeatTable(&buf)
	s.cfg.Verbose = true
	cpu := 12.0
	s.load = fixedLoad{load.Snapshot{CPUPercent: &cpu}}

	session := registerSession(t, s, modelHash)
	do(t, s, http.MethodPost, "/api/v1/heartbeat", heartbeatBody(session))

	if !strings.Contains(buf.String(), "|      12.0 |          N/A") {
		t.Errorf("colonne del server mancanti:\n%s", buf.String())
	}
}
