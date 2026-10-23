package stream

import (
	"context"
	"testing"
	"time"
)

func TestFrameIsMeasured(t *testing.T) {
	h := newHarness(t, &FakeExecutor{}, testConfig())
	if _, ok := h.handler.Network("web_96"); ok {
		t.Fatal("Network prima di uno stream: atteso false")
	}
	ws := h.open(t)
	sendFrame(t, ws, frame(1), make([]byte, split4Bytes))
	res, _ := readResult(t, ws)

	if res.Timing.ReceiveMS == nil || *res.Timing.ReceiveMS < 0 {
		t.Errorf("receive_ms = %v", res.Timing.ReceiveMS)
	}
	n, ok := h.handler.Network("web_96")
	if !ok || n.Valid+n.Discarded != 1 {
		t.Errorf("Network = %+v, %v: atteso 1 campione", n, ok)
	}
}

func TestNetworkSurvivesTheStream(t *testing.T) {
	h := newHarness(t, &FakeExecutor{}, testConfig())
	ws := h.open(t)
	sendFrame(t, ws, frame(1), make([]byte, split4Bytes))
	readResult(t, ws)
	ws.CloseNow()

	// Lo stato è del device: resta dopo la chiusura e il nuovo stream lo continua.
	ws = h.open(t)
	sendFrame(t, ws, frame(2), make([]byte, split4Bytes))
	readResult(t, ws)
	if n, _ := h.handler.Network("web_96"); n.Valid+n.Discarded != 2 {
		t.Errorf("Network = %+v: attesi 2 campioni", n)
	}
}

func TestPingMeasuresLatency(t *testing.T) {
	cfg := testConfig()
	cfg.PingInterval = 20 * time.Millisecond
	h := newHarness(t, &FakeExecutor{}, cfg)
	ws := h.open(t)
	// Il client risponde ai ping solo mentre legge: CloseRead legge in
	// background (e chiude se arriva un messaggio, qui non ne arrivano).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws.CloseRead(ctx)

	eventually(t, func() bool {
		n, _ := h.handler.Network("web_96")
		return n.LatencyMS != nil
	})
}

func TestPingDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.PingInterval = 0
	h := newHarness(t, &FakeExecutor{}, cfg)
	ws := h.open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws.CloseRead(ctx)

	time.Sleep(100 * time.Millisecond)
	if n, _ := h.handler.Network("web_96"); n.LatencyMS != nil {
		t.Errorf("ping disattivato ma latenza %v", *n.LatencyMS)
	}
}
