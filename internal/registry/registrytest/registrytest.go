// Package registrytest scrive cartelle di modello per i test: segmenti
// finti e un manifest.json valido, con il model_hash giusto.
//
// È un package a parte (come net/http/httptest) perché lo usano i test di
// più package (registry, server) e non deve finire nel binario del server.
package registrytest

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Model descrive la cartella da scrivere; Default ne dà una completa.
//
// Manifest sostituisce chiavi del manifest generato. Un valore nil toglie la
// chiave, tranne per "quantization", dove nil è il valore JSON null.
type Model struct {
	Segments   []string          // contenuto di submodel_i.tflite, in ordine
	Input      []int             // shape dell'input
	Labels     []string          // label del post-processing
	Manifest   map[string]any    // chiavi da sostituire (vedi sopra)
	ExtraFiles map[string]string // altri file nella cartella (nome → contenuto)
}

// Default è un modello con 8 segmenti come yolo-small.
func Default() Model {
	return Model{
		Segments: []string{"S0", "S1", "S2", "S3", "S4", "S5", "S6", "S7"},
		Input:    []int{1, 320, 320, 3},
		Labels:   []string{"bicycle", "stop sign"},
	}
}

// Write scrive m in dir e restituisce il model_hash.
func Write(t testing.TB, dir string, m Model) string {
	t.Helper()
	files := map[string]string{}
	segments := []map[string]any{}
	for i, content := range m.Segments {
		name := fmt.Sprintf("submodel_%d.tflite", i)
		files[name] = content
		segments = append(segments, map[string]any{"index": i, "file": name})
	}
	for name, content := range m.ExtraFiles {
		files[name] = content
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hash := Hash(files)

	manifest := map[string]any{
		"format_version": 2,
		"quantization":   nil,
		"input":          map[string]any{"shape": m.Input, "dtype": "float32"},
		"output":         map[string]any{"shape": []int{1, 2100, 4 + len(m.Labels)}, "dtype": "float32"},
		"segments":       segments,
		"postprocess": map[string]any{
			"decoder": "yolo_pro", "labels": m.Labels,
			"score_threshold": 0.5, "iou_threshold": 0.2, "max_detections": 200,
		},
		"model_hash": hash,
	}
	for k, v := range m.Manifest {
		if v == nil && k != "quantization" {
			delete(manifest, k)
			continue
		}
		manifest[k] = v
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return hash
}

// Hash è il model_hash dei file *.tflite tra files (nome → contenuto):
// MD5 dei contenuti in ordine di nome, la stessa regola del registry.
func Hash(files map[string]string) string {
	var names []string
	for name := range files {
		if filepath.Ext(name) == ".tflite" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	h := md5.New()
	for _, name := range names {
		h.Write([]byte(files[name]))
	}
	return hex.EncodeToString(h.Sum(nil))
}
