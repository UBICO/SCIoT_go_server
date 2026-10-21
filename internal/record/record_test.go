package record

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
)

// lines legge il file e restituisce ogni riga decodificata.
func lines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("riga non JSON: %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func kinds(ls []map[string]any) []string {
	var k []string
	for _, l := range ls {
		k = append(k, l["kind"].(string))
	}
	return k
}

func open(t *testing.T, opts Options) *Recorder {
	t.Helper()
	r, err := Open(t.TempDir(), Start{HeartbeatIntervalMS: 1000, Models: []api.ModelEntry{}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecordAndClose(t *testing.T) {
	r := open(t, DefaultOptions())
	if !strings.HasPrefix(filepath.Base(r.Stats().File), "sciot-") || filepath.Ext(r.Stats().File) != ".jsonl" {
		t.Errorf("file = %s", r.Stats().File)
	}
	r.Record(&Session{DeviceID: "d1", SessionID: "s1", ModelKey: "yolo"})
	n := 3
	r.Record(&Frame{DeviceID: "d1", SequenceID: 7, Outcome: OutcomeResult, Detections: &n,
		Admission: &Admission{PredictedWaitMS: 1.5, ServiceMS: 8, Known: true}})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	ls := lines(t, r.Stats().File)
	if got := strings.Join(kinds(ls), ","); got != "start,session,frame,end" {
		t.Fatalf("righe = %s", got)
	}
	if ls[0]["schema"] != Schema {
		t.Errorf("start = %v", ls[0])
	}
	// Header incorporato: kind, ts e t_ms allo stesso livello degli altri campi.
	frame := ls[2]
	if frame["device_id"] != "d1" || frame["sequence_id"] != 7.0 || frame["detections"] != 3.0 {
		t.Errorf("frame = %v", frame)
	}
	if adm := frame["admission"].(map[string]any); adm["predicted_wait_ms"] != 1.5 || adm["known"] != true {
		t.Errorf("admission = %v", adm)
	}
	if _, ok := frame["timing"]; ok {
		t.Error("timing assente deve essere omesso")
	}
	if ts, err := time.Parse(time.RFC3339Nano, frame["ts"].(string)); err != nil || ts.Location() != time.UTC {
		t.Errorf("ts = %v (%v)", frame["ts"], err)
	}
	// t_ms cresce; la riga end conta le righe scritte prima di sé.
	for i := 1; i < len(ls); i++ {
		if ls[i]["t_ms"].(float64) < ls[i-1]["t_ms"].(float64) {
			t.Errorf("t_ms decresce alla riga %d", i)
		}
	}
	if end := ls[3]; end["written"] != 3.0 || end["dropped"] != 0.0 {
		t.Errorf("end = %v", end)
	}
}

func TestTimestampIsWhenRecordedNotWhenWritten(t *testing.T) {
	r := open(t, DefaultOptions())
	at := time.Date(2026, 10, 21, 10, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	r.now = func() time.Time { return at }
	r.Record(&Policy{DeviceID: "d1", Reason: "admin"})
	r.now = time.Now
	r.Close()
	if ts := lines(t, r.Stats().File)[1]["ts"]; ts != "2026-10-21T08:00:00Z" {
		t.Errorf("ts = %v: l'ora è quella di Record, in UTC", ts)
	}
}

func TestDropWhenBufferIsFull(t *testing.T) {
	// Un Recorder senza la goroutine di scrittura: il buffer non si svuota.
	path := filepath.Join(t.TempDir(), "r.jsonl")
	file, _ := os.Create(path)
	r := &Recorder{path: path, start: time.Now(), now: time.Now, file: file, w: bufio.NewWriter(file),
		opts: Options{Buffer: 2, FlushInterval: time.Hour}, ch: make(chan Event, 2), done: make(chan struct{})}

	start := time.Now()
	for range 5 {
		r.Record(&Policy{DeviceID: "d"})
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Error("Record ha bloccato: con il buffer pieno deve scartare")
	}
	if s := r.Stats(); s.Dropped != 3 {
		t.Errorf("Dropped = %d, attesi 3", s.Dropped)
	}

	go r.run() // ora i 2 eventi accettati vengono scritti
	r.Close()
	ls := lines(t, path)
	if got := strings.Join(kinds(ls), ","); got != "policy,policy,end" {
		t.Errorf("righe = %s: gli eventi accettati non vanno persi", got)
	}
	if end := ls[2]; end["dropped"] != 3.0 {
		t.Errorf("end = %v", end)
	}
}

func TestPeriodicFlush(t *testing.T) {
	r := open(t, Options{Buffer: 16, FlushInterval: 10 * time.Millisecond})
	defer r.Close()
	r.Record(&Policy{DeviceID: "d"})
	deadline := time.Now().Add(5 * time.Second)
	for len(lines(t, r.Stats().File)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("l'evento non è arrivato sul disco senza Close")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNilRecorder(t *testing.T) {
	var r *Recorder
	r.Record(&Policy{})
	if err := r.Close(); err != nil {
		t.Error(err)
	}
	if r.Stats() != (Stats{}) {
		t.Error("Stats di un Recorder nil")
	}
}

func TestRecordAfterCloseAndCloseTwice(t *testing.T) {
	r := open(t, DefaultOptions())
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r.Record(&Policy{}) // niente panic: il channel è chiuso
	if err := r.Close(); err != nil {
		t.Errorf("seconda Close: %v", err)
	}
	if got := strings.Join(kinds(lines(t, r.Stats().File)), ","); got != "start,end" {
		t.Errorf("righe = %s", got)
	}
}

func TestConcurrentRecordAndClose(t *testing.T) {
	// Per -race: molte goroutine registrano mentre un'altra chiude.
	r := open(t, Options{Buffer: 64, FlushInterval: time.Millisecond})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				r.Record(&Policy{DeviceID: "d"})
			}
		})
	}
	time.Sleep(time.Millisecond)
	r.Close()
	wg.Wait()

	ls := lines(t, r.Stats().File)
	end := ls[len(ls)-1]
	if end["kind"] != "end" {
		t.Fatalf("ultima riga = %v", end)
	}
	// Ogni evento è scritto, scartato o arrivato dopo Close (ignorato).
	if written := int(end["written"].(float64)); written != len(ls)-1 {
		t.Errorf("end.written = %d, righe prima di end = %d", written, len(ls)-1)
	}
}
