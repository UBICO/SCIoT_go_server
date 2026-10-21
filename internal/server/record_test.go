package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/UBICO/SCIoT_go_server/internal/dispatch"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

func readRecord(t *testing.T, rec *record.Recorder) []map[string]any {
	t.Helper()
	rec.Close()
	f, err := os.Open(rec.Stats().File)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestControlPlaneEventsAreRecorded(t *testing.T) {
	s, modelHash := adminServer(t)
	rec, err := record.Open(t.TempDir(), record.Start{}, record.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	s.SetRecorder(rec)
	s.SetQueue(fixedQueue{dispatch.Snapshot{Slots: 2, Queued: 1}})

	session := registerSession(t, s, modelHash)
	do(t, s, http.MethodPost, "/api/v1/heartbeat", heartbeatBody(session))
	do(t, s, http.MethodPost, "/api/v1/admin/devices/web_192/policy", `{"mode":"SPLIT","split_point":5}`)

	lines := readRecord(t, rec)
	var got []string
	for _, l := range lines {
		got = append(got, l["kind"].(string))
	}
	if strings.Join(got, ",") != "start,session,heartbeat,policy,end" {
		t.Fatalf("eventi = %v", got)
	}
	if sess := lines[1]; sess["device_id"] != "web_192" || sess["session_id"] != session || sess["model_key"] != "fomo_96x96" {
		t.Errorf("session = %v", sess)
	}
	hb := lines[2]
	if hb["session_id"] != session || hb["applied"].(map[string]any)["mode"] != "LOCAL" {
		t.Errorf("heartbeat = %v", hb)
	}
	if _, ok := hb["server_load"]; !ok {
		t.Error("heartbeat senza server_load")
	}
	if q, ok := hb["queue"].(map[string]any); !ok || q["slots"] != 2.0 || q["queued"] != 1.0 {
		t.Errorf("queue = %v", hb["queue"])
	}
	if p := lines[3]; p["reason"] != "admin" || p["policy"].(map[string]any)["mode"] != "SPLIT" {
		t.Errorf("policy = %v", p)
	}
}

func TestServerStatusRecordSection(t *testing.T) {
	s, _, _ := testServer(t)
	if body := do(t, s, http.MethodGet, "/api/v1/server/status", "").Body.String(); strings.Contains(body, `"record"`) {
		t.Errorf("senza registrazione la sezione non deve esserci: %s", body)
	}
	rec, _ := record.Open(t.TempDir(), record.Start{}, record.DefaultOptions())
	defer rec.Close()
	s.SetRecorder(rec)
	var resp struct {
		Record *record.Stats `json:"record"`
	}
	json.NewDecoder(do(t, s, http.MethodGet, "/api/v1/server/status", "").Body).Decode(&resp)
	if resp.Record == nil || resp.Record.File != rec.Stats().File {
		t.Errorf("record = %+v", resp.Record)
	}
}
