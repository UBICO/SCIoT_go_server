package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBuild è lo script al posto di sciot-model build: stampa i suoi
// argomenti, crea --output e scrive model_hash, oppure fa altro secondo
// FAKE_BUILD (l'ambiente del server passa al processo).
const fakeBuild = `#!/bin/sh
out=""; prev=""
for a in "$@"; do [ "$prev" = "--output" ] && out="$a"; prev="$a"; done
echo "args: $*"
echo "token: [$SCIOT_ADMIN_TOKEN]"
echo "warning on stderr" >&2
case "$FAKE_BUILD" in
  fail) echo "error: boom" >&2; exit 2;;
  slow) trap 'echo "got TERM"; exit 143' TERM; i=0; while [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done; exit 0;;
  nohash) exit 0;;
  partial) printf "no newline"; echo; mkdir -p "$out"; echo "model_hash 0123456789abcdef0123456789abcdef"; printf "tail without newline"; exit 0;;
  many) seq 1 10005; mkdir -p "$out"; echo "model_hash 0123456789abcdef0123456789abcdef"; exit 0;;
esac
mkdir -p "$out"
printf 'model_hash %s\n' 0123456789abcdef0123456789abcdef
`

const fakeHash = "0123456789abcdef0123456789abcdef"

// buildServer: un server con models_dir temporanea, il registro, e lo
// script finto come comando di build.
func buildServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	s, _, _ := testServer(t)
	dir := t.TempDir()
	models := filepath.Join(dir, "models")
	if err := os.Mkdir(models, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "fake-build.sh")
	if err := os.WriteFile(script, []byte(fakeBuild), 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	s.SetAudit(a)
	s.SetModelBuilds(models)
	s.builds.command = []string{"sh", script}
	t.Cleanup(s.Close) // nessun processo resta dopo il test
	return s, s.AdminRoutes(testAdminToken), dir
}

// form è un upload: campi di testo e file (nome → contenuto).
type form struct {
	fields map[string][]string
	files  map[string][][2]string // campo → [nome file, contenuto]
}

func validForm(key string) form {
	return form{
		fields: map[string][]string{"model_key": {key}, "max_segments": {"3"}, "cut": {"model/conv_1/Relu;x"}},
		files: map[string][][2]string{
			"savedmodel": {{"ei-savedmodel.41.zip", "SM"}},
			"cpp_export": {{"ei-cpp.zip", "CPP"}},
			"sample":     {{"stop.png", "P"}, {"../../bike.JPG", "J"}, {"x.exe", "E"}},
		},
	}
}

func postBuild(t *testing.T, h http.Handler, f form) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, vs := range f.fields {
		for _, v := range vs {
			mw.WriteField(k, v)
		}
	}
	for k, fs := range f.files {
		for _, nf := range fs {
			fw, err := mw.CreateFormFile(k, nf[0])
			if err != nil {
				t.Fatal(err)
			}
			fw.Write([]byte(nf[1]))
		}
	}
	mw.Close()
	rec := adminReq(h, http.MethodPost, "/api/v1/admin/models/builds", body.String(), bearer(testAdminToken), func(r *http.Request) {
		r.Header.Set("Content-Type", mw.FormDataContentType())
	})
	var m map[string]any
	json.NewDecoder(rec.Body).Decode(&m)
	return rec.Code, m
}

// getBuild legge un job, con il log da since.
func getBuild(t *testing.T, h http.Handler, id string, since int) buildView {
	t.Helper()
	rec := adminReq(h, http.MethodGet, "/api/v1/admin/models/builds/"+id+"?since="+itoa(since), "", bearer(testAdminToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET build %s: %d %s", id, rec.Code, rec.Body)
	}
	var v buildView
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// waitBuild aspetta che il job finisca.
func waitBuild(t *testing.T, h http.Handler, id string) buildView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if v := getBuild(t, h, id, 0); v.State != buildRunning {
			return v
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("build %s ancora in corso", id)
	return buildView{}
}

func startBuild(t *testing.T, h http.Handler, key string) string {
	t.Helper()
	code, m := postBuild(t, h, validForm(key))
	if code != http.StatusAccepted {
		t.Fatalf("POST build: %d %v", code, m)
	}
	return m["id"].(string)
}

func TestBuildsDisabled(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)
	var resp adminBuildsResponse
	if code := getJSON(t, h, "/api/v1/admin/models/builds", &resp); code != http.StatusOK || resp.Enabled || resp.Builds == nil {
		t.Fatalf("senza models_dir: %d %+v", code, resp)
	}
	if code, _ := postBuild(t, h, validForm("m")); code != http.StatusNotFound {
		t.Errorf("POST senza models_dir: %d", code)
	}
}

func TestBuildSucceeds(t *testing.T) {
	t.Setenv("SCIOT_ADMIN_TOKEN", "secret-token-1234567890")
	_, h, dir := buildServer(t)
	code, m := postBuild(t, h, validForm("yolo_small_320"))
	if code != http.StatusAccepted || m["state"] != "running" || m["who"] != actorToken || m["output"] != filepath.Join(dir, "models", "yolo_small_320") {
		t.Fatalf("POST: %d %v", code, m)
	}
	v := waitBuild(t, h, m["id"].(string))
	if v.State != buildSucceeded || v.ModelHash != fakeHash || v.ExitCode == nil || *v.ExitCode != 0 || v.FinishedAt == nil {
		t.Fatalf("job: %+v", v)
	}
	if _, err := os.Stat(v.Output); err != nil {
		t.Errorf("cartella del modello: %v", err)
	}

	// Gli argomenti: nomi fissi per i file, mai quelli del browser; solo
	// estensioni di immagine note.
	args := strings.TrimPrefix(v.Log[0], "args: ")
	parts := strings.Fields(args)
	workDir := filepath.Dir(parts[0])
	want := []string{
		filepath.Join(workDir, "savedmodel.zip"),
		"--ei-cpp-export", filepath.Join(workDir, "cpp_export.zip"),
		"--sample", filepath.Join(workDir, "sample-1.png"),
		"--sample", filepath.Join(workDir, "sample-2.jpg"),
		"--sample", filepath.Join(workDir, "sample-3.img"),
		"--cut", "model/conv_1/Relu;x",
		"--max-segments", "3",
		"--output", v.Output,
	}
	if strings.Join(parts, " ") != strings.Join(want, " ") {
		t.Errorf("argomenti:\n%s\nvoglio:\n%s", strings.Join(parts, " "), strings.Join(want, " "))
	}
	if _, err := os.Stat(workDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("gli upload devono essere cancellati a fine job: %v", err)
	}
	if !strings.Contains(strings.Join(v.Log, "\n"), "token: []") {
		t.Errorf("SCIOT_ADMIN_TOKEN non deve arrivare al build: %v", v.Log[1])
	}
	if !strings.Contains(strings.Join(v.Log, "\n"), "warning on stderr") {
		t.Errorf("stderr nel log: %v", v.Log)
	}

	// since: solo le righe nuove.
	tail := getBuild(t, h, v.ID, v.LogLines-1)
	if len(tail.Log) != 1 || tail.Next != v.LogLines {
		t.Errorf("since: %+v", tail)
	}
	// Elenco e registro.
	var list adminBuildsResponse
	getJSON(t, h, "/api/v1/admin/models/builds", &list)
	if !list.Enabled || len(list.Builds) != 1 || list.Builds[0].ID != v.ID || list.Builds[0].Log != nil {
		t.Errorf("elenco: %+v", list)
	}
	lines := readAudit(t, dir)
	b, _ := lines[0]["build"].(map[string]any)
	if len(lines) != 1 || lines[0]["action"] != "model_build" || b["model_key"] != "yolo_small_320" {
		t.Errorf("registro: %v", lines)
	}
}

func TestBuildValidation(t *testing.T) {
	_, h, _ := buildServer(t)
	cases := map[string]func(f *form){
		"key with slash":    func(f *form) { f.fields["model_key"] = []string{"../x"} },
		"key uppercase":     func(f *form) { f.fields["model_key"] = []string{"Yolo"} },
		"no key":            func(f *form) { delete(f.fields, "model_key") },
		"no savedmodel":     func(f *form) { delete(f.files, "savedmodel") },
		"two savedmodels":   func(f *form) { f.files["savedmodel"] = append(f.files["savedmodel"], [2]string{"b.zip", "x"}) },
		"no cpp export":     func(f *form) { delete(f.files, "cpp_export") },
		"too many samples":  func(f *form) { f.files["sample"] = make([][2]string, maxBuildSamples+1) },
		"max_segments big":  func(f *form) { f.fields["max_segments"] = []string{"65"} },
		"max_segments neg":  func(f *form) { f.fields["max_segments"] = []string{"-1"} },
		"max_segments text": func(f *form) { f.fields["max_segments"] = []string{"abc"} },
		"bad cut":           func(f *form) { f.fields["cut"] = []string{"a b"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := validForm("m")
			mutate(&f)
			for i := range f.files["sample"] {
				if f.files["sample"][i][0] == "" {
					f.files["sample"][i] = [2]string{"s.png", "x"}
				}
			}
			if code, m := postBuild(t, h, f); code != http.StatusBadRequest || errCode(m) != "invalid_request" {
				t.Fatalf("%d %v, voglio 400", code, m)
			}
		})
	}
}

func TestBuildModelExists(t *testing.T) {
	s, h, _ := buildServer(t)
	if err := os.Mkdir(filepath.Join(s.builds.dir, "m"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, m := postBuild(t, h, validForm("m")); code != http.StatusConflict || errCode(m) != "model_exists" {
		t.Fatalf("%d %v, voglio 409 model_exists", code, m)
	}
}

func TestBuildOneAtATimeAndCancel(t *testing.T) {
	t.Setenv("FAKE_BUILD", "slow")
	_, h, dir := buildServer(t)
	id := startBuild(t, h, "a")
	if code, m := postBuild(t, h, validForm("b")); code != http.StatusConflict || errCode(m) != "build_running" {
		t.Fatalf("secondo build: %d %v", code, m)
	}
	rec := adminReq(h, http.MethodDelete, "/api/v1/admin/models/builds/"+id, "", bearer(testAdminToken))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	v := waitBuild(t, h, id)
	if v.State != buildCancelled {
		t.Fatalf("dopo il cancel: %+v", v)
	}
	if rec := adminReq(h, http.MethodDelete, "/api/v1/admin/models/builds/"+id, "", bearer(testAdminToken)); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_running") {
		t.Errorf("cancel di un job finito: %d %s", rec.Code, rec.Body)
	}
	actions := []string{}
	for _, l := range readAudit(t, dir) {
		actions = append(actions, l["action"].(string))
	}
	if strings.Join(actions, ",") != "model_build,model_build_cancel" {
		t.Errorf("registro: %v", actions)
	}
	// Libero di nuovo.
	t.Setenv("FAKE_BUILD", "")
	waitBuild(t, h, startBuild(t, h, "b"))
}

func TestBuildFailures(t *testing.T) {
	for mode, wantExit := range map[string]int{"fail": 2, "nohash": 0} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("FAKE_BUILD", mode)
			_, h, _ := buildServer(t)
			v := waitBuild(t, h, startBuild(t, h, "m"))
			if v.State != buildFailed || v.ExitCode == nil || *v.ExitCode != wantExit {
				t.Fatalf("%+v", v)
			}
			if mode == "fail" && !strings.Contains(strings.Join(v.Log, "\n"), "error: boom") {
				t.Errorf("log: %v", v.Log)
			}
		})
	}
}

func TestBuildLogLines(t *testing.T) {
	t.Setenv("FAKE_BUILD", "partial")
	_, h, _ := buildServer(t)
	v := waitBuild(t, h, startBuild(t, h, "m"))
	log := strings.Join(v.Log, "\n")
	if v.State != buildSucceeded || !strings.Contains(log, "no newline") || v.Log[len(v.Log)-1] != "tail without newline" {
		t.Fatalf("%+v", v)
	}
}

func TestBuildLogTruncated(t *testing.T) {
	t.Setenv("FAKE_BUILD", "many")
	_, h, _ := buildServer(t)
	v := waitBuild(t, h, startBuild(t, h, "m"))
	if v.LogLines != maxBuildLogLines+1 || v.Log[len(v.Log)-1] != buildLogTruncated {
		t.Fatalf("righe %d, ultima %q", v.LogLines, v.Log[len(v.Log)-1])
	}
	if v.State != buildSucceeded || v.ModelHash != fakeHash {
		t.Errorf("l'hash si legge anche dopo il taglio del log: %+v", v.State)
	}
}

func TestBuildCommandNotFound(t *testing.T) {
	s, h, dir := buildServer(t)
	s.builds.command = []string{filepath.Join(t.TempDir(), "missing")}
	if code, _ := postBuild(t, h, validForm("m")); code != http.StatusInternalServerError {
		t.Fatalf("comando inesistente: %d, voglio 500", code)
	}
	var list adminBuildsResponse
	getJSON(t, h, "/api/v1/admin/models/builds", &list)
	if len(list.Builds) != 1 || list.Builds[0].State != buildFailed {
		t.Fatalf("elenco: %+v", list)
	}
	s.builds.command = []string{"sh", filepath.Join(dir, "fake-build.sh")}
	if code, m := postBuild(t, h, validForm("n")); code != http.StatusAccepted {
		t.Fatalf("dopo un avvio fallito il build deve essere libero: %d %v", code, m)
	}
}

func TestBuildTooLarge(t *testing.T) {
	old := maxBuildUpload
	maxBuildUpload = 1024
	t.Cleanup(func() { maxBuildUpload = old })
	_, h, _ := buildServer(t)
	f := validForm("m")
	f.files["savedmodel"] = [][2]string{{"big.zip", strings.Repeat("x", 4096)}}
	if code, m := postBuild(t, h, f); code != http.StatusRequestEntityTooLarge || errCode(m) != "too_large" {
		t.Fatalf("%d %v, voglio 413", code, m)
	}
}

// Allo spegnimento del server il build in corso si ferma.
func TestBuildStopsWithTheServer(t *testing.T) {
	t.Setenv("FAKE_BUILD", "slow")
	s, h, _ := buildServer(t)
	id := startBuild(t, h, "m")
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(buildKillGrace + 2*time.Second):
		t.Fatal("Close non ha fermato il build")
	}
	if v := getBuild(t, h, id, 0); v.State != buildCancelled {
		t.Errorf("dopo Close: %s", v.State)
	}
}

func TestBuildJobWrite(t *testing.T) {
	j := &buildJob{}
	for _, chunk := range []string{"ab", "c\nd", "\r\n", "model_hash " + fakeHash + "\n", "e"} {
		if n, err := j.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write: %d %v", n, err)
		}
	}
	if strings.Join(j.log, "|") != "abc|d|model_hash "+fakeHash || string(j.partial) != "e" || j.modelHash != fakeHash {
		t.Fatalf("log %q partial %q hash %q", j.log, j.partial, j.modelHash)
	}
}
