package stream

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

// recordedFrames chiude il Recorder e restituisce le righe "frame".
func recordedFrames(t *testing.T, rec *record.Recorder) []record.Frame {
	t.Helper()
	rec.Close()
	f, err := os.Open(rec.Stats().File)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var frames []record.Frame
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var fr record.Frame
		if err := json.Unmarshal(sc.Bytes(), &fr); err != nil {
			t.Fatal(err)
		}
		if fr.Kind == "frame" {
			frames = append(frames, fr)
		}
	}
	return frames
}

func recorded(t *testing.T, exec *FakeExecutor) (*harness, *record.Recorder) {
	t.Helper()
	h := newHarness(t, exec, testConfig())
	rec, err := record.Open(t.TempDir(), record.Start{}, record.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	h.handler.SetRecorder(rec)
	return h, rec
}

func TestFrameResultIsRecorded(t *testing.T) {
	exec := &FakeExecutor{
		Detections: []api.Detection{{Label: "stop sign", Score: 0.9}},
		Admission:  &Admission{PredictedWait: 1500 * time.Microsecond, Service: 8 * time.Millisecond, Known: true},
	}
	h, rec := recorded(t, exec)
	ws := h.open(t)
	hdr := frame(42)
	deadline := 45.0
	hdr.DeadlineMS = &deadline
	sendFrame(t, ws, hdr, make([]byte, split4Bytes))
	readResult(t, ws)

	frames := recordedFrames(t, rec)
	if len(frames) != 1 {
		t.Fatalf("frames = %+v", frames)
	}
	f := frames[0]
	if f.DeviceID != "web_96" || f.SessionID != "S1" || f.SequenceID != 42 || f.PolicyVersion != 2 ||
		f.SplitPoint != 4 || f.RequestBytes != split4Bytes || f.DeadlineMS != 45 || f.Outcome != record.OutcomeResult {
		t.Errorf("frame = %+v", f)
	}
	if f.Admission == nil || f.Admission.PredictedWaitMS != 1.5 || f.Admission.ServiceMS != 8 || !f.Admission.Known {
		t.Errorf("admission = %+v", f.Admission)
	}
	if f.Timing == nil || len(f.Timing.LayerMS) != 2 || f.Detections == nil || *f.Detections != 1 {
		t.Errorf("timing %+v, detections %v", f.Timing, f.Detections)
	}
}

func TestRejectedFrameIsRecordedWithItsPrediction(t *testing.T) {
	exec := &FakeExecutor{Err: &JobError{
		Admission: Admission{PredictedWait: 61 * time.Millisecond, Service: 8 * time.Millisecond, Known: true},
		Err:       errors.Join(ErrBusy, errors.New("predicted wait 61 ms + service 8 ms > deadline 45 ms")),
	}}
	h, rec := recorded(t, exec)
	ws := h.open(t)
	sendFrame(t, ws, frame(1), make([]byte, split4Bytes))
	var e api.StreamError
	readText(t, ws, &e)
	if e.Code != api.ErrCodeServerBusy {
		t.Fatalf("errore = %+v: JobError deve restare un ErrBusy", e)
	}

	f := recordedFrames(t, rec)[0]
	if f.Outcome != record.OutcomeServerBusy || f.Admission == nil || f.Admission.PredictedWaitMS != 61 {
		t.Errorf("frame = %+v, admission %+v", f, f.Admission)
	}
	if f.Timing != nil || f.Detections != nil {
		t.Error("un frame rifiutato non ha tempi né detection")
	}
}

func TestFrameWithoutPredictionIsRecorded(t *testing.T) {
	h, rec := recorded(t, &FakeExecutor{Err: ErrWorkerUnavailable})
	ws := h.open(t)
	sendFrame(t, ws, frame(1), make([]byte, split4Bytes))
	var e api.StreamError
	readText(t, ws, &e)

	f := recordedFrames(t, rec)[0]
	if f.Outcome != record.OutcomeWorkerUnavailable || f.Admission != nil {
		t.Errorf("frame = %+v", f)
	}
}
