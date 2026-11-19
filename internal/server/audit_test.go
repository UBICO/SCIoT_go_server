package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// readAudit legge le righe di dir/AuditFile.
func readAudit(t *testing.T, dir string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, AuditFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("riga non JSON: %q", sc.Text())
		}
		out = append(out, m)
	}
	return out
}

// auditServer: un server con un device registrato e il registro in una
// cartella temporanea (che OpenAudit deve creare).
func auditServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, modelHash, _ := testServer(t)
	registerSession(t, s, modelHash)
	dir := filepath.Join(t.TempDir(), "record")
	a, err := OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	s.SetAudit(a)
	return s, dir
}

func TestOpenAuditAppendsAcrossRestarts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nuova") // non esiste ancora
	a, err := OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 11, 19, 15, 4, 5, 0, time.UTC)
	for _, action := range []string{auditPin, auditUnpin} {
		if err := a.write(auditEntry{TS: ts, Who: "francesco", Action: action, DeviceID: "pi_a"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// Un riavvio: il file si riapre e si continua in coda.
	a, err = OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.write(auditEntry{TS: ts, Who: "token", Action: auditPin, DeviceID: "pi_b"}); err != nil {
		t.Fatal(err)
	}
	a.Close()

	lines := readAudit(t, dir)
	if len(lines) != 3 {
		t.Fatalf("%d righe, voglio 3: %v", len(lines), lines)
	}
	if lines[0]["ts"] != "2026-11-19T15:04:05Z" || lines[0]["who"] != "francesco" || lines[1]["action"] != "unpin" || lines[2]["device_id"] != "pi_b" {
		t.Errorf("righe: %v", lines)
	}
	info, err := os.Stat(filepath.Join(dir, AuditFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permessi %o, voglio 600", perm)
	}
}

func TestAuditPinAndUnpinOnTheAdminPort(t *testing.T) {
	s, dir := auditServer(t)
	h := s.AdminRoutes(testAdminToken)
	cookie := login(t, h) // nome "francesco"

	if rec := adminReq(h, http.MethodPost, "/api/v1/admin/devices/web_192/policy", `{"mode":"SPLIT","split_point":5}`, withCookie(cookie)); rec.Code != http.StatusOK {
		t.Fatalf("pin: %d %s", rec.Code, rec.Body)
	}
	if rec := adminReq(h, http.MethodDelete, "/api/v1/admin/devices/web_192/policy", "", bearer(testAdminToken)); rec.Code != http.StatusOK {
		t.Fatalf("unpin: %d %s", rec.Code, rec.Body)
	}

	lines := readAudit(t, dir)
	if len(lines) != 2 {
		t.Fatalf("%d righe, voglio 2: %v", len(lines), lines)
	}
	pin, unpin := lines[0], lines[1]
	if pin["who"] != "francesco" || pin["action"] != "pin" || pin["device_id"] != "web_192" || pin["pinned"] != true {
		t.Errorf("pin: %v", pin)
	}
	if p, _ := pin["policy"].(map[string]any); p["mode"] != "SPLIT" || p["split_point"] != float64(5) || p["policy_version"] != float64(2) {
		t.Errorf("policy del pin: %v", pin["policy"])
	}
	if pin["session_id"] == "" || pin["ts"] == "" {
		t.Errorf("sessione/ora: %v", pin)
	}
	// Bearer: lo script non ha un nome. Unpin: la policy resta, pinned no.
	if unpin["who"] != actorToken || unpin["action"] != "unpin" || unpin["pinned"] != false {
		t.Errorf("unpin: %v", unpin)
	}
}

func TestAuditOnTheDevicePort(t *testing.T) {
	s, dir := auditServer(t)
	s.cfg.Admin = true
	if rec := do(t, s, http.MethodPost, "/api/v1/admin/devices/web_192/policy", `{"mode":"LOCAL","split_point":null}`); rec.Code != http.StatusOK {
		t.Fatalf("pin: %d %s", rec.Code, rec.Body)
	}
	if lines := readAudit(t, dir); len(lines) != 1 || lines[0]["who"] != actorDevicePort {
		t.Fatalf("righe: %v", lines)
	}
}

func TestAuditWithoutToken(t *testing.T) {
	s, dir := auditServer(t)
	h := s.AdminRoutes("")

	// Senza login: anonimo. Con il login: il nome, anche senza token.
	adminReq(h, http.MethodPost, "/api/v1/admin/devices/web_192/policy", `{"mode":"SPLIT","split_point":3}`)
	rec := adminReq(h, http.MethodPost, "/api/v1/admin/login", `{"token":"qualunque","name":"giulia"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d", rec.Code)
	}
	adminReq(h, http.MethodDelete, "/api/v1/admin/devices/web_192/policy", "", withCookie(rec.Result().Cookies()[0]))

	lines := readAudit(t, dir)
	if len(lines) != 2 || lines[0]["who"] != actorAnonymous || lines[1]["who"] != "giulia" {
		t.Fatalf("righe: %v", lines)
	}
}

// Un pin rifiutato non è una modifica: niente riga.
func TestAuditSkipsFailedActions(t *testing.T) {
	s, dir := auditServer(t)
	h := s.AdminRoutes(testAdminToken)
	adminReq(h, http.MethodPost, "/api/v1/admin/devices/web_192/policy", `{"mode":"SPLIT","split_point":999}`, bearer(testAdminToken))
	adminReq(h, http.MethodPost, "/api/v1/admin/devices/ghost/policy", `{"mode":"LOCAL","split_point":null}`, bearer(testAdminToken))
	adminReq(h, http.MethodDelete, "/api/v1/admin/devices/ghost/policy", "", bearer(testAdminToken))
	if lines := readAudit(t, dir); len(lines) != 0 {
		t.Fatalf("righe: %v", lines)
	}
}

// Senza registro (nessuna sezione "record") il pin funziona lo stesso.
func TestPinWithoutAuditLog(t *testing.T) {
	s, modelHash, _ := testServer(t)
	registerSession(t, s, modelHash)
	h := s.AdminRoutes(testAdminToken)
	if rec := adminReq(h, http.MethodPost, "/api/v1/admin/devices/web_192/policy", `{"mode":"SPLIT","split_point":5}`, bearer(testAdminToken)); rec.Code != http.StatusOK {
		t.Fatalf("pin: %d %s", rec.Code, rec.Body)
	}
}

// I limiti per il pin: lo split massimo dal modello, la finestra dallo
// stream (null senza data plane).
func TestAdminDevicesPinLimits(t *testing.T) {
	s, modelHash, store := testServer(t)
	registerSession(t, s, modelHash)
	h := s.AdminRoutes(testAdminToken)

	d := adminDevices(t, h)[0]
	want := float64(store.List()[0].Model.LastOffloadingLayer)
	if d["last_offloading_layer"] != want || want <= 0 {
		t.Errorf("last_offloading_layer %v, voglio %v", d["last_offloading_layer"], want)
	}
	if v, ok := d["max_inflight_max"]; !ok || v != nil {
		t.Errorf("max_inflight_max senza data plane: %v (presente=%v), voglio null", v, ok)
	}

	s.SetStream(http.NotFoundHandler(), &fakeNotifier{}) // MaxInflight = 3
	if d := adminDevices(t, h)[0]; d["max_inflight_max"] != float64(3) {
		t.Errorf("max_inflight_max con data plane: %v, voglio 3", d["max_inflight_max"])
	}
}
