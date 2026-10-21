package registry

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/registry/registrytest"
)

// writeFiles crea in dir un file per ogni coppia nome → contenuto.
// t.Helper() fa sì che, in caso di errore, il test riporti la riga del
// chiamante invece di quella dentro l'helper.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// md5Hex è il riferimento: MD5 della concatenazione delle parti, in ordine.
func md5Hex(parts ...string) string {
	h := md5.New()
	for _, p := range parts {
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestHashTFLiteDir(t *testing.T) {
	// t.TempDir crea una cartella temporanea, cancellata a fine test.
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"submodel_1.tflite": "BBB",
		"submodel_0.tflite": "AAA",
		"notes.txt":         "da ignorare",
	})

	got, err := hashTFLiteDir(dir)
	if err != nil {
		t.Fatalf("hashTFLiteDir: %v", err)
	}
	// Ordine per nome: submodel_0 poi submodel_1. notes.txt escluso.
	if want := md5Hex("AAA", "BBB"); got != want {
		t.Errorf("hash = %s, want %s", got, want)
	}
}

func TestHashTFLiteDirEmpty(t *testing.T) {
	_, err := hashTFLiteDir(t.TempDir())
	// errors.Is "scava" dentro gli errori avvolti con %w.
	if !errors.Is(err, errNoTFLiteFiles) {
		t.Errorf("err = %v, want errNoTFLiteFiles", err)
	}
}

func TestNewAndLookup(t *testing.T) {
	dir := t.TempDir()
	hash := registrytest.Write(t, dir, registrytest.Default())

	reg, err := New(Config{Models: []ModelConfig{{ModelKey: "yolo_small_320", TFLiteDir: dir}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("Len = %d, want 1", reg.Len())
	}

	info, ok := reg.Lookup(hash)
	if !ok {
		t.Fatal("Lookup: hash non trovato")
	}
	// Dal manifest: 8 segmenti → num_layers 8, ultimo indice 7.
	if info.ModelKey != "yolo_small_320" || info.NumLayers != 8 || info.LastOffloadingLayer != 7 {
		t.Errorf("ModelInfo errato: %+v", info)
	}
	if !slices.Equal(info.Input.Shape, []int{1, 320, 320, 3}) || info.Input.DType != "float32" {
		t.Errorf("Input = %+v", info.Input)
	}
	pp := info.Postprocess
	if pp.Decoder != "yolo_pro" || !slices.Equal(pp.Labels, []string{"bicycle", "stop sign"}) ||
		pp.ScoreThreshold != 0.5 || pp.IoUThreshold == nil || *pp.IoUThreshold != 0.2 || pp.MaxDetections != 200 {
		t.Errorf("Postprocess = %+v", pp)
	}

	if _, ok := reg.Lookup("sconosciuto"); ok {
		t.Error("Lookup di un hash sconosciuto deve restituire false")
	}
}

func TestNewDuplicateHash(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	registrytest.Write(t, a, registrytest.Default())
	registrytest.Write(t, b, registrytest.Default())

	_, err := New(Config{Models: []ModelConfig{
		{ModelKey: "a", TFLiteDir: a},
		{ModelKey: "b", TFLiteDir: b},
	}})
	if err == nil || !strings.Contains(err.Error(), "same segments") {
		t.Fatalf("err = %v: due modelli dallo stesso hash vanno rifiutati", err)
	}
}

func TestModelsSortedByKey(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	registrytest.Write(t, a, registrytest.Default())
	fomo := registrytest.Default()
	fomo.Segments = []string{"F0", "F1", "F2"}
	hashB := registrytest.Write(t, b, fomo)

	reg, err := New(Config{Models: []ModelConfig{
		{ModelKey: "yolo_small", TFLiteDir: a},
		{ModelKey: "fomo_96x96", TFLiteDir: b},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Ripetuto: l'ordine di una map è casuale, quello di Models no.
	for range 20 {
		models := reg.Models()
		if len(models) != 2 || models[0].ModelKey != "fomo_96x96" || models[1].ModelKey != "yolo_small" {
			t.Fatalf("ordine errato: %+v", models)
		}
		if models[0].ModelHash != hashB || models[0].NumLayers != 3 || models[1].NumLayers != 8 {
			t.Fatalf("contenuto errato: %+v", models)
		}
	}
}

// --- manifest -----------------------------------------------------------------

func TestManifestErrors(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(m *registrytest.Model)
		write func(t *testing.T, dir string) // dopo Write: per togliere o cambiare file
		want  string
	}{
		{name: "manifest mancante",
			write: func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, ManifestFile)) },
			want:  "sciot-model build"},
		{name: "versione del formato",
			edit: func(m *registrytest.Model) { m.Manifest = map[string]any{"format_version": 1} },
			want: "1"},
		{name: "modello quantizzato",
			edit: func(m *registrytest.Model) {
				m.Manifest = map[string]any{"quantization": map[string]any{"weights": "int8"}}
			},
			want: "quantized"},
		{name: "indici dei segmenti fuori ordine",
			edit: func(m *registrytest.Model) {
				m.Manifest = map[string]any{"segments": []map[string]any{
					{"index": 1, "file": "submodel_0.tflite"}, {"index": 0, "file": "submodel_1.tflite"}}}
				m.Segments = m.Segments[:2]
			},
			want: "index"},
		{name: "stesso file elencato due volte",
			edit: func(m *registrytest.Model) {
				m.Manifest = map[string]any{"segments": []map[string]any{
					{"index": 0, "file": "submodel_0.tflite"}, {"index": 1, "file": "submodel_0.tflite"}}}
				m.Segments = m.Segments[:1]
			},
			want: "twice"},
		{name: "segmento in più nella cartella",
			edit: func(m *registrytest.Model) { m.ExtraFiles = map[string]string{"submodel_8.tflite": "OLD"} },
			want: "submodel_8.tflite"},
		{name: "segmento mancante",
			write: func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "submodel_3.tflite")) },
			want:  "submodel_3.tflite"},
		{name: "file di un altro build",
			write: func(t *testing.T, dir string) {
				os.WriteFile(filepath.Join(dir, "submodel_2.tflite"), []byte("OTHER BUILD"), 0o644)
			},
			want: "different builds"},
		{name: "postprocess vuoto",
			edit: func(m *registrytest.Model) { m.Manifest = map[string]any{"postprocess": nil} },
			want: "postprocess"},
		{name: "input mancante",
			edit: func(m *registrytest.Model) { m.Manifest = map[string]any{"input": nil} },
			want: "input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			m := registrytest.Default()
			if tc.edit != nil {
				tc.edit(&m)
			}
			registrytest.Write(t, dir, m)
			if tc.write != nil {
				tc.write(t, dir)
			}
			_, err := New(Config{Models: []ModelConfig{{ModelKey: "yolo", TFLiteDir: dir}}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, atteso un errore che contenga %q", err, tc.want)
			}
		})
	}
}

func TestManifestIgnoresOtherFiles(t *testing.T) {
	dir := t.TempDir()
	m := registrytest.Default()
	m.ExtraFiles = map[string]string{"sizes.json": "{}", "README.txt": "note"}
	registrytest.Write(t, dir, m)
	if _, err := New(Config{Models: []ModelConfig{{ModelKey: "yolo", TFLiteDir: dir}}}); err != nil {
		t.Errorf("file non .tflite non devono contare: %v", err)
	}
}

// --- override del post-processing ------------------------------------------------

func f64(v float64) *float64 { return &v }
func intp(v int) *int        { return &v }

func TestPostprocessOverride(t *testing.T) {
	dir := t.TempDir()
	hash := registrytest.Write(t, dir, registrytest.Default())
	override := &PostprocessOverride{ScoreThreshold: f64(0.7), IoUThreshold: f64(0.4), MaxDetections: intp(3)}

	reg, err := New(Config{Models: []ModelConfig{{ModelKey: "yolo", TFLiteDir: dir, Postprocess: override}}})
	if err != nil {
		t.Fatal(err)
	}
	pp, _ := reg.Lookup(hash)
	if pp.Postprocess.ScoreThreshold != 0.7 || *pp.Postprocess.IoUThreshold != 0.4 || pp.Postprocess.MaxDetections != 3 {
		t.Errorf("Postprocess = %+v", pp.Postprocess)
	}
	if pp.Postprocess.Decoder != "yolo_pro" || len(pp.Postprocess.Labels) != 2 {
		t.Errorf("decoder e label devono restare quelli del manifest: %+v", pp.Postprocess)
	}
	// L'override non deve condividere memoria con il risultato.
	*override.IoUThreshold = 0.9
	if *pp.Postprocess.IoUThreshold != 0.4 {
		t.Error("IoUThreshold condivide la memoria con il config")
	}
}

func TestPostprocessOverrideErrors(t *testing.T) {
	base := api.Postprocess{Decoder: "yolo_pro", Labels: []string{"a"}, ScoreThreshold: 0.5,
		IoUThreshold: f64(0.2), MaxDetections: 10}
	fomo := api.Postprocess{Decoder: "fomo", Labels: []string{"a"}, ScoreThreshold: 0.8, MaxDetections: 10}
	cases := []struct {
		name string
		o    PostprocessOverride
		p    api.Postprocess
		want string
	}{
		{"score zero", PostprocessOverride{ScoreThreshold: f64(0)}, base, "score_threshold"},
		{"score oltre 1", PostprocessOverride{ScoreThreshold: f64(1.5)}, base, "score_threshold"},
		{"iou zero", PostprocessOverride{IoUThreshold: f64(0)}, base, "iou_threshold"},
		{"iou senza NMS", PostprocessOverride{IoUThreshold: f64(0.3)}, fomo, "NMS"},
		{"max zero", PostprocessOverride{MaxDetections: intp(0)}, base, "max_detections"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.o.apply(tc.p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, atteso un errore che contenga %q", err, tc.want)
			}
		})
	}
	var none *PostprocessOverride
	if got, err := none.apply(base); err != nil || got.ScoreThreshold != 0.5 {
		t.Errorf("override nil: %+v, %v", got, err)
	}
}
