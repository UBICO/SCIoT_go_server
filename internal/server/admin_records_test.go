package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/record"
)

func getJSON(t *testing.T, h http.Handler, path string, dst any) int {
	t.Helper()
	rec := adminReq(h, http.MethodGet, path, "", bearer(testAdminToken))
	if rec.Code == http.StatusOK {
		if err := json.NewDecoder(rec.Body).Decode(dst); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	return rec.Code
}

func writeFile(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// recordsServer: un server con record.dir in una cartella temporanea, con
// due registrazioni vecchie, il registro e file che non vanno elencati.
func recordsServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	s, _, _ := testServer(t)
	dir := t.TempDir()
	mod := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	writeFile(t, filepath.Join(dir, "sciot-20261118-090000.jsonl"), "{\"kind\":\"start\"}\n", mod)
	writeFile(t, filepath.Join(dir, "sciot-20261119-100000.jsonl"), "{\"kind\":\"start\"}\n{\"kind\":\"end\"}\n", mod)
	writeFile(t, filepath.Join(dir, AuditFile), "", mod)
	writeFile(t, filepath.Join(dir, "notes.txt"), "x", mod)
	writeFile(t, filepath.Join(dir, "sciot-20261119-100000.jsonl.bak"), "x", mod)
	if err := os.Mkdir(filepath.Join(dir, "sciot-20261120-000000.jsonl"), 0o755); err != nil { // una cartella con il nome giusto
		t.Fatal(err)
	}
	s.SetRecordDir(dir)
	return s, s.AdminRoutes(testAdminToken), dir
}

func TestRecordingsDisabled(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)
	var resp adminRecordingsResponse
	if code := getJSON(t, h, "/api/v1/admin/recordings", &resp); code != http.StatusOK || resp.Recording || resp.Files == nil || len(resp.Files) != 0 {
		t.Fatalf("senza record: %d %+v", code, resp)
	}
	if rec := adminReq(h, http.MethodGet, "/api/v1/admin/recordings/sciot-20261119-100000.jsonl", "", bearer(testAdminToken)); rec.Code != http.StatusNotFound {
		t.Errorf("download senza record: %d", rec.Code)
	}
}

func TestRecordingsList(t *testing.T) {
	s, h, dir := recordsServer(t)
	// Il file di questo avvio: aperto dal Recorder, come in main.
	r, err := record.Open(dir, record.Start{}, record.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	s.SetRecorder(r)
	current := filepath.Base(r.Stats().File)

	var resp adminRecordingsResponse
	if code := getJSON(t, h, "/api/v1/admin/recordings", &resp); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !resp.Recording || resp.Dir != dir {
		t.Errorf("recording %v dir %q", resp.Recording, resp.Dir)
	}
	var names []string
	for _, f := range resp.Files {
		names = append(names, f.Name)
	}
	// Dal più recente; niente registro, .bak, .txt, cartelle.
	want := []string{current, "sciot-20261119-100000.jsonl", "sciot-20261118-090000.jsonl"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("file %v, voglio %v", names, want)
	}
	if !resp.Files[0].Current || resp.Files[1].Current || resp.Files[2].Current {
		t.Errorf("current: %+v", resp.Files)
	}
	if f := resp.Files[1]; f.SizeBytes != int64(len("{\"kind\":\"start\"}\n{\"kind\":\"end\"}\n")) || !f.Modified.Equal(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("dimensione/data: %+v", f)
	}
}

func TestRecordingDownload(t *testing.T) {
	_, h, _ := recordsServer(t)
	rec := adminReq(h, http.MethodGet, "/api/v1/admin/recordings/sciot-20261119-100000.jsonl", "", bearer(testAdminToken))
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"kind\":\"start\"}\n{\"kind\":\"end\"}\n" {
		t.Fatalf("download: %d %q", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="sciot-20261119-100000.jsonl"` {
		t.Errorf("Content-Disposition %q", cd)
	}

	// Range: un download ripreso.
	rec = adminReq(h, http.MethodGet, "/api/v1/admin/recordings/sciot-20261119-100000.jsonl", "", bearer(testAdminToken), func(r *http.Request) {
		r.Header.Set("Range", "bytes=17-")
	})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "{\"kind\":\"end\"}\n" {
		t.Errorf("Range: %d %q", rec.Code, rec.Body)
	}
}

// Solo i nomi di registrazione, solo dentro record.dir.
func TestRecordingDownloadRefusesOtherFiles(t *testing.T) {
	_, h, dir := recordsServer(t)
	outside := filepath.Join(filepath.Dir(dir), "secret.jsonl")
	writeFile(t, outside, "secret", time.Now())
	for _, name := range []string{
		AuditFile,
		"notes.txt",
		"sciot-20261119-100000.jsonl.bak",
		"sciot-20990101-000000.jsonl", // nome giusto, file inesistente
		"..%2Fsecret.jsonl",
		"sciot-20261119-100000.jsonl%2F..%2F..%2Fsecret.jsonl",
	} {
		rec := adminReq(h, http.MethodGet, "/api/v1/admin/recordings/"+name, "", bearer(testAdminToken))
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("%s: %d %q, voglio 404", name, rec.Code, rec.Body)
		}
	}
}

func TestRecordingsNeedLogin(t *testing.T) {
	_, h, _ := recordsServer(t)
	for _, p := range []string{"/api/v1/admin/recordings", "/api/v1/admin/recordings/sciot-20261119-100000.jsonl", "/api/v1/admin/changelog", "/api/v1/admin/models"} {
		if rec := adminReq(h, http.MethodGet, p, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s senza credenziali: %d", p, rec.Code)
		}
	}
}

func TestChangelog(t *testing.T) {
	_, h, dir := recordsServer(t)
	var lines []string
	for i := 1; i <= 5; i++ {
		lines = append(lines, `{"ts":"2026-11-19T10:00:0`+string(rune('0'+i))+`Z","who":"francesco","action":"pin","n":`+string(rune('0'+i))+`}`)
	}
	// Una riga a metà (crash durante la scrittura) e una vuota: saltate.
	content := strings.Join(lines[:3], "\n") + "\n{\"ts\":\"rotta\n\n" + strings.Join(lines[3:], "\n") + "\n"
	writeFile(t, filepath.Join(dir, AuditFile), content, time.Now())

	var resp struct {
		Enabled bool             `json:"enabled"`
		Entries []map[string]any `json:"entries"`
	}
	if code := getJSON(t, h, "/api/v1/admin/changelog", &resp); code != http.StatusOK || !resp.Enabled {
		t.Fatalf("changelog: %d %+v", code, resp)
	}
	var ns []float64
	for _, e := range resp.Entries {
		ns = append(ns, e["n"].(float64))
	}
	if len(ns) != 5 || ns[0] != 5 || ns[4] != 1 {
		t.Fatalf("righe %v, voglio 5..1", ns)
	}
	if code := getJSON(t, h, "/api/v1/admin/changelog?limit=2", &resp); code != http.StatusOK || len(resp.Entries) != 2 || resp.Entries[0]["n"] != float64(5) || resp.Entries[1]["n"] != float64(4) {
		t.Fatalf("limit=2: %d %+v", code, resp.Entries)
	}
	for _, bad := range []string{"0", "-1", "1001", "abc"} {
		if code := getJSON(t, h, "/api/v1/admin/changelog?limit="+bad, &resp); code != http.StatusBadRequest {
			t.Errorf("limit=%s: %d, voglio 400", bad, code)
		}
	}
}

func TestChangelogEmptyAndDisabled(t *testing.T) {
	_, h, dir := recordsServer(t)
	if err := os.Remove(filepath.Join(dir, AuditFile)); err != nil {
		t.Fatal(err)
	}
	var resp adminChangelogResponse
	if code := getJSON(t, h, "/api/v1/admin/changelog", &resp); code != http.StatusOK || !resp.Enabled || resp.Entries == nil || len(resp.Entries) != 0 {
		t.Fatalf("senza file: %d %+v", code, resp)
	}

	s, _, _ := testServer(t)
	h = s.AdminRoutes(testAdminToken)
	resp = adminChangelogResponse{}
	if code := getJSON(t, h, "/api/v1/admin/changelog", &resp); code != http.StatusOK || resp.Enabled || resp.Entries == nil {
		t.Fatalf("senza record: %d %+v", code, resp)
	}
}

func TestAdminModels(t *testing.T) {
	s, modelHash, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)
	var resp struct {
		Models []map[string]any `json:"models"`
	}
	if code := getJSON(t, h, "/api/v1/admin/models", &resp); code != http.StatusOK || len(resp.Models) != 1 {
		t.Fatalf("modelli: %d %+v", code, resp)
	}
	m := resp.Models[0]
	if m["model_key"] != "fomo_96x96" || m["model_hash"] != modelHash || m["devices"] != float64(0) || m["num_layers"] == nil || m["last_offloading_layer"] == nil {
		t.Errorf("modello: %v", m)
	}
	registerSession(t, s, modelHash)
	getJSON(t, h, "/api/v1/admin/models", &resp)
	if resp.Models[0]["devices"] != float64(1) {
		t.Errorf("dopo una registrazione: devices %v", resp.Models[0]["devices"])
	}
}

func TestCompleteLines(t *testing.T) {
	long := strings.Repeat("x", tailScan+10)
	cases := []struct {
		name, content string
		want          int
	}{
		{"complete", "{\"a\":1}\n{\"b\":2}\n", 16},
		{"partial last line", "{\"a\":1}\n{\"b\":", 8},
		{"empty", "", 0},
		{"no newline", "{\"a\":1}", 7},
		{"partial line longer than tailScan", "{\"a\":1}\n" + long, 8 + len(long)},
		{"newline far back, short tail", long + "\n{\"b\":", len(long) + 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, err := completeLines(strings.NewReader(c.content), int64(len(c.content)))
			if err != nil || n != int64(c.want) {
				t.Fatalf("n %d err %v, voglio %d", n, err, c.want)
			}
		})
	}
}

// Il file in scrittura con una riga a metà: il download si ferma all'ultima
// riga intera, così sciot-replay lo legge.
func TestRecordingDownloadDropsPartialLine(t *testing.T) {
	_, h, dir := recordsServer(t)
	writeFile(t, filepath.Join(dir, "sciot-20261120-080000.jsonl"), "{\"kind\":\"start\"}\n{\"kind\":\"heart", time.Now())
	rec := adminReq(h, http.MethodGet, "/api/v1/admin/recordings/sciot-20261120-080000.jsonl", "", bearer(testAdminToken))
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"kind\":\"start\"}\n" {
		t.Fatalf("download: %d %q", rec.Code, rec.Body)
	}
}
