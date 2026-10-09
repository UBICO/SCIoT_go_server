package registry

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
	writeFiles(t, dir, map[string]string{"submodel_0.tflite": "AAA"})

	reg, err := New(Config{Models: []ModelConfig{{
		ModelKey:            "fomo_96x96",
		TFLiteDir:           dir,
		LastOffloadingLayer: 58,
		InputSizeBytes:      18432,
	}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("Len = %d, want 1", reg.Len())
	}

	info, ok := reg.Lookup(md5Hex("AAA"))
	if !ok {
		t.Fatal("Lookup: hash non trovato")
	}
	if info.ModelKey != "fomo_96x96" || info.NumLayers != 59 ||
		info.LastOffloadingLayer != 58 || info.InputSizeBytes != 18432 {
		t.Errorf("ModelInfo errato: %+v", info)
	}

	if _, ok := reg.Lookup("sconosciuto"); ok {
		t.Error("Lookup di un hash sconosciuto deve restituire false")
	}
}

func TestNewDuplicateHash(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeFiles(t, a, map[string]string{"s.tflite": "SAME"})
	writeFiles(t, b, map[string]string{"s.tflite": "SAME"})

	_, err := New(Config{Models: []ModelConfig{
		{ModelKey: "a", TFLiteDir: a},
		{ModelKey: "b", TFLiteDir: b},
	}})
	if err == nil {
		t.Fatal("New deve fallire con due modelli dallo stesso hash")
	}
}
