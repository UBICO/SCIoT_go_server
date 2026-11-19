package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

const testAdminToken = "0123456789abcdef-test"

// adminReq costruisce una richiesta per la porta admin; opts la modificano
// (header, cookie) prima dell'invio.
func adminReq(h http.Handler, method, path, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func withCookie(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(c) }
}

// login fa il login e restituisce il cookie di sessione.
func login(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := adminReq(h, http.MethodPost, "/api/v1/admin/login", `{"token":"`+testAdminToken+`","name":"francesco"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminCookie {
			return c
		}
	}
	t.Fatal("login senza cookie " + adminCookie)
	return nil
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e api.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&e); err != nil {
		t.Fatalf("errore non decodificabile: %v", err)
	}
	return e.Error.Code
}

// ---------------------------------------------------------------------------
// Autenticazione
// ---------------------------------------------------------------------------

func TestAdminWithoutCredentials(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)

	for _, path := range []string{"/api/v1/admin/status", "/api/v1/admin/devices"} {
		rec := adminReq(h, http.MethodGet, path, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s senza credenziali: %d, voglio 401", path, rec.Code)
		}
		if code := errorCode(t, rec); code != errCodeUnauthorized {
			t.Errorf("%s: codice %q", path, code)
		}
	}
}

func TestAdminBearer(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)

	if rec := adminReq(h, http.MethodGet, "/api/v1/admin/status", "", bearer(testAdminToken)); rec.Code != http.StatusOK {
		t.Fatalf("Bearer giusto: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{"sbagliato", testAdminToken + "x", testAdminToken[:len(testAdminToken)-1], ""} {
		if rec := adminReq(h, http.MethodGet, "/api/v1/admin/status", "", bearer(bad)); rec.Code != http.StatusUnauthorized {
			t.Errorf("Bearer %q: %d, voglio 401", bad, rec.Code)
		}
	}
	// Il token senza "Bearer " davanti non vale.
	raw := func(r *http.Request) { r.Header.Set("Authorization", testAdminToken) }
	if rec := adminReq(h, http.MethodGet, "/api/v1/admin/status", "", raw); rec.Code != http.StatusUnauthorized {
		t.Errorf("Authorization senza Bearer: %d, voglio 401", rec.Code)
	}
}

func TestAdminWrongBearerDoesNotFallBackToCookie(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)
	c := login(t, h)

	rec := adminReq(h, http.MethodGet, "/api/v1/admin/status", "", bearer("sbagliato"), withCookie(c))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Bearer sbagliato con cookie valido: %d, voglio 401", rec.Code)
	}
}

func TestAdminLoginCookie(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)

	rec := adminReq(h, http.MethodPost, "/api/v1/admin/login", `{"token":"`+testAdminToken+`","name":"francesco"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var resp adminLoginResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Name != "francesco" || resp.ExpiresAt.IsZero() {
		t.Errorf("risposta del login: %+v", resp)
	}

	var c *http.Cookie
	for _, k := range rec.Result().Cookies() {
		if k.Name == adminCookie {
			c = k
		}
	}
	if c == nil {
		t.Fatal("nessun cookie")
	}
	if c.Value == testAdminToken || strings.Contains(c.Value, testAdminToken) {
		t.Error("il cookie non deve contenere il token")
	}
	if len(c.Value) != 43 { // 32 byte in base64 URL senza padding
		t.Errorf("id di sessione lungo %d, voglio 43", len(c.Value))
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure {
		t.Errorf("attributi del cookie: HttpOnly=%v SameSite=%v Path=%q Secure=%v", c.HttpOnly, c.SameSite, c.Path, c.Secure)
	}
	if c.MaxAge != int(adminSessionTTL.Seconds()) {
		t.Errorf("MaxAge = %d, voglio %d", c.MaxAge, int(adminSessionTTL.Seconds()))
	}

	if rec := adminReq(h, http.MethodGet, "/api/v1/admin/status", "", withCookie(c)); rec.Code != http.StatusOK {
		t.Fatalf("con il cookie: %d %s", rec.Code, rec.Body)
	}
	if got := s.admin.sessions[c.Value].name; got != "francesco" {
		t.Errorf("nome della sessione %q", got)
	}

	// Due login: due sessioni diverse.
	if c2 := login(t, h); c2.Value == c.Value {
		t.Error("due login con lo stesso id di sessione")
	}
}

func TestAdminLoginErrors(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)

	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"token sbagliato", `{"token":"sbagliato","name":"x"}`, http.StatusUnauthorized, errCodeUnauthorized},
		{"nome vuoto", `{"token":"` + testAdminToken + `","name":"  "}`, http.StatusBadRequest, api.ErrCodeInvalidRequest},
		{"JSON rotto", `{"token":`, http.StatusBadRequest, api.ErrCodeInvalidRequest},
		{"chiave sconosciuta", `{"token":"` + testAdminToken + `","name":"x","admin":true}`, http.StatusBadRequest, api.ErrCodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := adminReq(h, http.MethodPost, "/api/v1/admin/login", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status %d, voglio %d; %s", rec.Code, tc.status, rec.Body)
			}
			if len(rec.Result().Cookies()) != 0 {
				t.Error("un login fallito non deve dare cookie")
			}
			if code := errorCode(t, rec); code != tc.code {
				t.Errorf("codice %q, voglio %q", code, tc.code)
			}
		})
	}
}

func TestAdminSessionExpires(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)
	t0 := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	now := t0
	s.admin.now = func() time.Time { return now }
	c := login(t, h)

	now = t0.Add(adminSessionTTL - time.Second)
	if rec := adminReq(h, http.MethodGet, "/api/v1/admin/status", "", withCookie(c)); rec.Code != http.StatusOK {
		t.Fatalf("un secondo prima della scadenza: %d", rec.Code)
	}
	now = t0.Add(adminSessionTTL)
	if rec := adminReq(h, http.MethodGet, "/api/v1/admin/status", "", withCookie(c)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("alla scadenza: %d, voglio 401", rec.Code)
	}
	if _, ok := s.admin.sessions[c.Value]; ok {
		t.Error("la sessione scaduta deve essere cancellata")
	}
}

func TestAdminLogout(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)
	c := login(t, h)

	rec := adminReq(h, http.MethodPost, "/api/v1/admin/logout", "", withCookie(c))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}
	cleared := false
	for _, k := range rec.Result().Cookies() {
		if k.Name == adminCookie && k.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("il logout deve cancellare il cookie (MaxAge < 0)")
	}
	if rec := adminReq(h, http.MethodGet, "/api/v1/admin/status", "", withCookie(c)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("dopo il logout: %d, voglio 401", rec.Code)
	}
}

func TestAdminWithoutToken(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes("")

	if rec := adminReq(h, http.MethodGet, "/api/v1/admin/devices", ""); rec.Code != http.StatusOK {
		t.Fatalf("senza token configurato: %d, voglio 200", rec.Code)
	}
	// Il login funziona comunque: dà il nome per il registro delle modifiche.
	rec := adminReq(h, http.MethodPost, "/api/v1/admin/login", `{"token":"","name":"francesco"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login senza token configurato: %d %s", rec.Code, rec.Body)
	}
}

func TestAdminCrossOriginIsRejected(t *testing.T) {
	s, modelHash, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)
	do(t, s, http.MethodPost, "/api/v1/register", registerBody(modelHash))
	c := login(t, h)
	body := `{"mode":"SPLIT","split_point":5}`

	crossSite := func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }
	if rec := adminReq(h, http.MethodPost, "/api/v1/admin/devices/web_192/policy", body, withCookie(c), crossSite); rec.Code != http.StatusForbidden {
		t.Fatalf("POST da un altro sito: %d, voglio 403", rec.Code)
	}
	sameOrigin := func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") }
	if rec := adminReq(h, http.MethodPost, "/api/v1/admin/devices/web_192/policy", body, withCookie(c), sameOrigin); rec.Code != http.StatusOK {
		t.Fatalf("POST dalla dashboard: %d %s", rec.Code, rec.Body)
	}
	// Una GET da un altro sito passa (è sicura): resta il 401 senza cookie...
	if rec := adminReq(h, http.MethodGet, "/api/v1/admin/status", "", crossSite); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET da un altro sito senza cookie: %d, voglio 401", rec.Code)
	}
}

func TestAdminPinAndUnpin(t *testing.T) {
	s, modelHash, store := testServer(t)
	h := s.AdminRoutes(testAdminToken)
	do(t, s, http.MethodPost, "/api/v1/register", registerBody(modelHash))

	rec := adminReq(h, http.MethodPost, "/api/v1/admin/devices/web_192/policy", `{"mode":"SPLIT","split_point":5}`, bearer(testAdminToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("pin: %d %s", rec.Code, rec.Body)
	}
	if d, _ := store.Get("web_192"); !d.Pinned || d.Policy.Mode != api.ModeSplit {
		t.Fatalf("dopo il pin: %+v", d)
	}
	if rec := adminReq(h, http.MethodDelete, "/api/v1/admin/devices/web_192/policy", "", bearer(testAdminToken)); rec.Code != http.StatusOK {
		t.Fatalf("unpin: %d %s", rec.Code, rec.Body)
	}
	if d, _ := store.Get("web_192"); d.Pinned {
		t.Fatal("dopo l'unpin il device è ancora bloccato")
	}
}

// Le due porte non si mescolano: niente admin sulla porta dei device (senza
// -admin), niente protocollo dei device sulla porta admin.
func TestAdminRoutesAreSeparate(t *testing.T) {
	s, modelHash, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)

	if rec := do(t, s, http.MethodGet, "/api/v1/admin/devices", ""); rec.Code != http.StatusNotFound {
		t.Errorf("admin sulla porta dei device: %d, voglio 404", rec.Code)
	}
	if rec := adminReq(h, http.MethodPost, "/api/v1/register", registerBody(modelHash), bearer(testAdminToken)); rec.Code != http.StatusNotFound {
		t.Errorf("register sulla porta admin: %d, voglio 404", rec.Code)
	}
}

// La dashboard si carica senza login (i dati sono dietro l'API); una route
// /api/ sconosciuta è un 404 JSON, non la pagina della dashboard. Il
// contenuto delle pagine lo verifica internal/dashboard.
func TestAdminServesTheDashboard(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)

	for _, path := range []string{"/", "/devices/pi_a"} {
		rec := adminReq(h, http.MethodGet, path, "")
		if rec.Code == http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("GET %s: %d %q, voglio una pagina HTML senza login", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	for _, path := range []string{"/api/v1/nope", "/api/"} {
		rec := adminReq(h, http.MethodGet, path, "", bearer(testAdminToken))
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Errorf("GET %s: %d %s, voglio 404 not_found", path, rec.Code, rec.Body)
		}
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/admin/devices
// ---------------------------------------------------------------------------

func adminDevices(t *testing.T, h http.Handler) []map[string]any {
	t.Helper()
	rec := adminReq(h, http.MethodGet, "/api/v1/admin/devices", "", bearer(testAdminToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("devices: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Devices []map[string]any `json:"devices"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp.Devices
}

func TestAdminDevicesEmpty(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)
	rec := adminReq(h, http.MethodGet, "/api/v1/admin/devices", "", bearer(testAdminToken))
	if got := strings.TrimSpace(rec.Body.String()); got != `{"devices":[]}` {
		t.Fatalf("senza device: %s, voglio {\"devices\":[]}", got)
	}
}

func TestAdminDevices(t *testing.T) {
	s, modelHash, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)
	rec := do(t, s, http.MethodPost, "/api/v1/register", registerBody(modelHash))
	var reg api.RegisterResponse
	if err := json.NewDecoder(rec.Body).Decode(&reg); err != nil {
		t.Fatal(err)
	}

	// Appena registrato: nessun heartbeat, nessuna decisione, nessuno stream.
	devs := adminDevices(t, h)
	if len(devs) != 1 {
		t.Fatalf("%d device, voglio 1", len(devs))
	}
	d := devs[0]
	if d["device_id"] != "web_192" || d["device_type"] != "python" || d["model_key"] != "fomo_96x96" || d["session_id"] != reg.SessionID {
		t.Errorf("campi dello Store: %v", d)
	}
	if d["pinned"] != false || d["applied_policy_version"] != float64(0) {
		t.Errorf("pinned/applied: %v %v", d["pinned"], d["applied_policy_version"])
	}
	for _, k := range []string{"telemetry", "network", "last_decision"} {
		if v, ok := d[k]; !ok || v != nil {
			t.Errorf("%s: %v (presente=%v), voglio null", k, v, ok)
		}
	}

	// Dopo un heartbeat c'è la telemetria.
	heartbeatPolicy(t, s, reg.SessionID)
	d = adminDevices(t, h)[0]
	tel, ok := d["telemetry"].(map[string]any)
	if !ok {
		t.Fatalf("telemetry dopo un heartbeat: %v", d["telemetry"])
	}
	if _, ok := tel["inference_count"]; !ok {
		t.Errorf("telemetry senza inference_count: %v", tel)
	}

	// Una decisione dell'algoritmo compare con l'ora e il motivo.
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	s.decisions.set("web_192", at, &record.Decision{DeviceID: "web_192", Algorithm: "latency", Reason: "faster"})
	d = adminDevices(t, h)[0]
	dec, ok := d["last_decision"].(map[string]any)
	if !ok {
		t.Fatalf("last_decision: %v", d["last_decision"])
	}
	if dec["at"] != "2026-11-19T09:00:00Z" {
		t.Errorf("at = %v", dec["at"])
	}
	if inner, _ := dec["decision"].(map[string]any); inner["reason"] != "faster" || inner["algorithm"] != "latency" {
		t.Errorf("decision = %v", dec["decision"])
	}

	// Una nuova registrazione cancella la decisione della sessione vecchia.
	do(t, s, http.MethodPost, "/api/v1/register", registerBody(modelHash))
	if d = adminDevices(t, h)[0]; d["last_decision"] != nil {
		t.Errorf("dopo la ri-registrazione last_decision = %v, voglio null", d["last_decision"])
	}
}

func TestDecisionLogZeroValue(t *testing.T) {
	var l decisionLog // lo zero value deve funzionare, come in Server
	if l.get("x") != nil {
		t.Fatal("get su un log vuoto")
	}
	l.forget("x") // non deve andare in panic
	l.set("x", time.Unix(0, 0), &record.Decision{Reason: "r"})
	if got := l.get("x"); got == nil || got.Decision.Reason != "r" {
		t.Fatalf("get dopo set: %+v", got)
	}
	l.forget("x")
	if l.get("x") != nil {
		t.Fatal("get dopo forget")
	}
}

// ---------------------------------------------------------------------------
// CheckAdminAddr
// ---------------------------------------------------------------------------

func TestCheckAdminAddr(t *testing.T) {
	cases := []struct {
		addr, token string
		ok          bool
	}{
		{"127.0.0.1:8081", "", true},
		{"127.1.2.3:8081", "", true},
		{"[::1]:8081", "", true},
		{"localhost:8081", "", true},
		{":8081", "", false},
		{"0.0.0.0:8081", "", false},
		{"192.168.1.10:8081", "", false},
		{"[::]:8081", "", false},
		{"192.168.1.10:8081", testAdminToken, true},
		{":8081", testAdminToken, true},
		{"127.0.0.1:8081", "corto", false}, // un token corto è un errore anche su loopback
		{"127.0.0.1", "", false},           // manca la porta
	}
	for _, tc := range cases {
		err := CheckAdminAddr(tc.addr, tc.token)
		if (err == nil) != tc.ok {
			t.Errorf("CheckAdminAddr(%q, %q) = %v, voglio ok=%v", tc.addr, tc.token, err, tc.ok)
		}
	}
}
