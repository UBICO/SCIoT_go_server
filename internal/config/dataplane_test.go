package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/device"
	"github.com/UBICO/SCIoT_go_server/internal/stream"
)

const modelsJSON = `"models": [{
	"model_key": "fomo_96x96",
	"tflite_dir": "/models/fomo_96x96"
}]`

var known = []api.ModelEntry{{ModelHash: "h", ModelInfo: api.ModelInfo{ModelKey: "fomo_96x96", NumLayers: 59}}}

func loadSections(t *testing.T, sections string) File {
	t.Helper()
	f, err := Load(writeConfig(t, "{"+sections+modelsJSON+"}"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return f
}

func TestWorkerOptionsDisabledWithoutSection(t *testing.T) {
	opts, enabled, err := loadSections(t, "").WorkerOptions(known)
	if err != nil || enabled {
		t.Errorf("senza sezione worker: enabled %v, err %v, opts %+v", enabled, err, opts)
	}
}

func TestWorkerOptionsManaged(t *testing.T) {
	f := loadSections(t, `"worker": {"command": ["uv", "run", "sciot-worker"], "address": "127.0.0.1:50051",
		"slots": 2, "threads": 6, "startup_timeout_ms": 30000},`)
	opts, enabled, err := f.WorkerOptions(known)
	if err != nil || !enabled {
		t.Fatalf("enabled %v, err %v", enabled, err)
	}
	if strings.Join(opts.Command, " ") != "uv run sciot-worker" || opts.Address != "127.0.0.1:50051" ||
		opts.Slots != 2 || opts.Threads != 6 || opts.StartupTimeout != 30*time.Second {
		t.Errorf("opts = %+v", opts)
	}
	if len(opts.Models) != 1 || opts.Models[0].Key != "fomo_96x96" || opts.Models[0].Dir != "/models/fomo_96x96" {
		t.Errorf("Models = %+v", opts.Models)
	}
	if len(opts.Known) != 1 {
		t.Errorf("Known = %+v", opts.Known)
	}
	if opts.MinBackoff != workerMinBackoff || opts.MaxBackoff != workerMaxBackoff ||
		opts.StopGrace != workerStopGrace || opts.HealthInterval != workerHealthInterval {
		t.Errorf("durate fisse = %+v", opts)
	}
}

func TestWorkerOptionsExternal(t *testing.T) {
	// Worker esterno: slots e threads li decide chi lo avvia.
	opts, enabled, err := loadSections(t, `"worker": {"address": "127.0.0.1:50051"},`).WorkerOptions(known)
	if err != nil || !enabled || len(opts.Command) != 0 {
		t.Fatalf("enabled %v, err %v, opts %+v", enabled, err, opts)
	}
	if opts.StartupTimeout != DefaultWorkerStartupTimeout {
		t.Errorf("StartupTimeout = %v, atteso il default", opts.StartupTimeout)
	}
}

func TestWorkerOptionsErrors(t *testing.T) {
	cases := []struct {
		name, worker, field string
	}{
		{"address mancante", `{"command": ["w"], "slots": 1, "threads": 1}`, "address"},
		{"slots zero", `{"command": ["w"], "address": "a:1", "slots": 0, "threads": 1}`, "slots"},
		{"threads zero", `{"command": ["w"], "address": "a:1", "slots": 1, "threads": 0}`, "threads"},
		{"startup timeout zero", `{"address": "a:1", "startup_timeout_ms": 0}`, "startup_timeout_ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := loadSections(t, `"worker": `+tc.worker+`,`).WorkerOptions(known)
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Errorf("err = %v, atteso un errore che nomina %q", err, tc.field)
			}
		})
	}
}

func TestStreamConfig(t *testing.T) {
	cfg, err := loadSections(t, "").StreamConfig()
	if err != nil || cfg != stream.DefaultConfig() {
		t.Errorf("senza sezione: %+v, %v", cfg, err)
	}

	f := loadSections(t, `"stream": {"max_inflight": 5, "default_deadline_ms": 250, "max_message_bytes": 2048},`)
	cfg, err = f.StreamConfig()
	if err != nil || cfg.MaxInflight != 5 || cfg.DefaultDeadline != 250*time.Millisecond || cfg.MaxMessageBytes != 2048 {
		t.Errorf("cfg = %+v, err %v", cfg, err)
	}
	// Il probe di default si adatta a un messaggio piccolo.
	if cfg.ProbeBytes != 2048-probeHeaderRoom {
		t.Errorf("probe_bytes = %d, atteso %d", cfg.ProbeBytes, 2048-probeHeaderRoom)
	}

	f = loadSections(t, `"stream": {"probe_interval_ms": 0, "probe_bytes": 131072},`)
	cfg, err = f.StreamConfig()
	if err != nil || cfg.ProbeInterval != 0 || cfg.ProbeBytes != 131072 {
		t.Errorf("cfg = %+v, err %v", cfg, err)
	}
}

func TestStreamConfigErrors(t *testing.T) {
	cases := []struct{ name, stream, field string }{
		{"max_inflight zero", `{"max_inflight": 0}`, "max_inflight"},
		{"deadline zero", `{"default_deadline_ms": 0}`, "default_deadline_ms"},
		{"messaggio troppo piccolo", `{"max_message_bytes": 100}`, "max_message_bytes"},
		{"ping troppo frequente", `{"ping_interval_ms": 10}`, "ping_interval_ms"},
		{"probe troppo frequente", `{"probe_interval_ms": 10}`, "probe_interval_ms"},
		{"tau della banda troppo corto", `{"bandwidth_tau_ms": 10}`, "bandwidth_tau_ms"},
		{"probe troppo piccolo", `{"probe_bytes": 1000}`, "probe_bytes"},
		{"probe oltre il messaggio", `{"max_message_bytes": 1048576, "probe_bytes": 1048576}`, "probe_bytes"},
		{"messaggio oltre il limite gRPC", `{"max_message_bytes": 1073741824}`, "max_message_bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadSections(t, `"stream": `+tc.stream+`,`).StreamConfig()
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Errorf("err = %v, atteso un errore che nomina %q", err, tc.field)
			}
		})
	}
}

func TestExampleConfigLoads(t *testing.T) {
	f, err := Load("../../config.example.json")
	if err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
	if _, err := f.StreamConfig(); err != nil {
		t.Errorf("stream: %v", err)
	}
	if _, enabled, err := f.WorkerOptions(nil); err != nil || !enabled {
		t.Errorf("worker: enabled %v, err %v", enabled, err)
	}
}

func TestRecordDir(t *testing.T) {
	if _, enabled, err := loadSections(t, "").RecordDir(); enabled || err != nil {
		t.Errorf("senza sezione: enabled %v, err %v", enabled, err)
	}
	dir, enabled, err := loadSections(t, `"record": {"dir": "/tmp/records"},`).RecordDir()
	if !enabled || err != nil || dir != "/tmp/records" {
		t.Errorf("dir %q, enabled %v, err %v", dir, enabled, err)
	}
	home, _ := os.UserHomeDir()
	if dir, _, _ := loadSections(t, `"record": {"dir": "~/sciot-records"},`).RecordDir(); dir != filepath.Join(home, "sciot-records") {
		t.Errorf("~ non espansa: %q", dir)
	}
	if _, _, err := loadSections(t, `"record": {},`).RecordDir(); err == nil || !strings.Contains(err.Error(), "record.dir") {
		t.Errorf("dir mancante: err = %v", err)
	}
}

func TestTauOptions(t *testing.T) {
	f := loadSections(t, `"dispatch": {"service_tau_ms": 5000}, "stream": {"bandwidth_tau_ms": 120000},`)
	d, err := f.DispatchConfig()
	if err != nil || d.ServiceTau != 5*time.Second {
		t.Errorf("dispatch = %+v, %v", d, err)
	}
	st, err := f.StreamConfig()
	if err != nil || st.BandwidthTau != 2*time.Minute {
		t.Errorf("stream = %+v, %v", st, err)
	}
	if tau, err := loadSections(t, "").DeviceSegmentTau(); err != nil || tau != device.DefaultSegmentTau {
		t.Errorf("default device_segment_tau = %v, %v", tau, err)
	}
	if _, err := loadSections(t, `"dispatch": {"service_tau_ms": 4000000},`).DispatchConfig(); err == nil {
		t.Error("service_tau_ms oltre l'ora: atteso un errore")
	}
}
