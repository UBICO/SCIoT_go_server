package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig scrive content in un config.json temporaneo e ne restituisce il path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	path := writeConfig(t, `{
		"server": {"heartbeat_interval_ms": 500},
		"models": [{
			"model_key": "fomo_96x96",
			"tflite_dir": "/models/fomo_96x96"
		}]
	}`)

	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Server.HeartbeatIntervalMS == nil || *f.Server.HeartbeatIntervalMS != 500 {
		t.Errorf("heartbeat_interval_ms = %v", f.Server.HeartbeatIntervalMS)
	}
	if rc := f.RegistryConfig(); len(rc.Models) != 1 || rc.Models[0].TFLiteDir != "/models/fomo_96x96" {
		t.Errorf("RegistryConfig = %+v", rc)
	}
}

func TestLoadWithoutServerSection(t *testing.T) {
	// Il config.json della fase precedente (solo "models") resta valido.
	f, err := Load(writeConfig(t, `{"models": []}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Server.HeartbeatIntervalMS != nil {
		t.Errorf("heartbeat_interval_ms deve essere nil se assente")
	}
}

func TestLoadUnknownField(t *testing.T) {
	path := writeConfig(t, `{"server": {"heartbeat_interval": 500}, "models": []}`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load deve rifiutare la chiave sconosciuta \"heartbeat_interval\"")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "non-esiste.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want os.ErrNotExist avvolto", err)
	}
}

func ms(v int) *int { return &v }

func TestServerConfig(t *testing.T) {
	cases := []struct {
		name    string
		ms      *int
		want    time.Duration
		wantErr bool
	}{
		{"assente → default 1s", nil, time.Second, false},
		{"500 ms", ms(500), 500 * time.Millisecond, false},
		{"minimo", ms(100), 100 * time.Millisecond, false},
		{"massimo", ms(60_000), time.Minute, false},
		{"zero", ms(0), 0, true},
		{"negativo", ms(-1), 0, true},
		{"sotto il minimo", ms(99), 0, true},
		{"sopra il massimo", ms(60_001), 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := File{Server: Server{HeartbeatIntervalMS: tc.ms}}
			cfg, err := f.ServerConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("errore atteso, got cfg %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("ServerConfig: %v", err)
			}
			if cfg.HeartbeatInterval != tc.want {
				t.Errorf("HeartbeatInterval = %v, want %v", cfg.HeartbeatInterval, tc.want)
			}
			// Gli altri campi devono restare ai default.
			if cfg.MaxBodyBytes == 0 {
				t.Error("MaxBodyBytes azzerato: parti da server.DefaultConfig()")
			}
		})
	}
}

func TestServerConfigVerbose(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name string
		v    *bool
		want bool
	}{{"assente → false", nil, false}, {"true", &on, true}, {"false", &off, false}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := File{Server: Server{Verbose: tc.v}}.ServerConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Verbose != tc.want {
				t.Errorf("Verbose = %v, want %v", cfg.Verbose, tc.want)
			}
		})
	}
}

func TestServerConfigAdmin(t *testing.T) {
	on := true
	cfg, err := File{Server: Server{Admin: &on}}.ServerConfig()
	if err != nil || !cfg.Admin {
		t.Errorf("Admin = %v, err %v", cfg.Admin, err)
	}
	if cfg, _ := (File{}).ServerConfig(); cfg.Admin {
		t.Error("Admin deve essere false di default")
	}
}

func TestLoadMonitorConfig(t *testing.T) {
	alpha := func(v float64) *float64 { return &v }
	cases := []struct {
		name         string
		ms           *int
		alpha        *float64
		wantInterval time.Duration
		wantAlpha    float64
		wantErr      bool
	}{
		{"assenti → default", nil, nil, time.Second, 0.35, false},
		{"250 ms", ms(250), nil, 250 * time.Millisecond, 0.35, false},
		{"alpha 1", nil, alpha(1), time.Second, 1, false},
		{"intervallo sotto il minimo", ms(99), nil, 0, 0, true},
		{"intervallo sopra il massimo", ms(60_001), nil, 0, 0, true},
		{"alpha zero", nil, alpha(0), 0, 0, true},
		{"alpha negativo", nil, alpha(-0.1), 0, 0, true},
		{"alpha sopra 1", nil, alpha(1.5), 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := File{Server: Server{LoadSampleIntervalMS: tc.ms, LoadCPUEWMAAlpha: tc.alpha}}
			cfg, err := f.LoadConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("errore atteso, got %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Interval != tc.wantInterval || cfg.CPUEWMAAlpha != tc.wantAlpha {
				t.Errorf("cfg = %+v, want %v / %v", cfg, tc.wantInterval, tc.wantAlpha)
			}
		})
	}
}

func TestLoadAcceptsLoadKeys(t *testing.T) {
	f, err := Load(writeConfig(t, `{"server": {"load_sample_interval_ms": 500, "load_cpu_ewma_alpha": 0.5}, "models": []}`))
	if err != nil {
		t.Fatal(err)
	}
	if *f.Server.LoadSampleIntervalMS != 500 || *f.Server.LoadCPUEWMAAlpha != 0.5 {
		t.Errorf("server = %+v", f.Server)
	}
}

func TestLoadRemovedModelKeysExplainWhy(t *testing.T) {
	// Un config.json di prima del manifest: l'errore spiega dove sono finiti i valori.
	for key, want := range map[string]string{
		"last_offloading_layer": "number of segments",
		"input_size_bytes":      "manifest.json",
	} {
		path := writeConfig(t, `{"models": [{"model_key": "m", "tflite_dir": "/d", "`+key+`": 1}]}`)
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "MODELS.md") {
			t.Errorf("%s: err = %v", key, err)
		}
	}
}
