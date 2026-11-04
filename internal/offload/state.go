package offload

import (
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

// StateFrom costruisce lo stato di un device da un suo heartbeat registrato:
// la stessa funzione serve al server dal vivo (che costruisce l'heartbeat
// come per la registrazione) e al replay, così le due strade non possono
// divergere. policyAge è da quanto la policy attuale è in vigore.
func StateFrom(hb record.Heartbeat, policyAge time.Duration) State {
	st := State{
		DeviceID:     hb.DeviceID,
		ModelKey:     hb.ModelKey,
		Last:         hb.NumSegments - 1,
		Policy:       hb.ServerPolicy,
		PolicyAge:    policyAge,
		FramesPerSec: FramesPerSec(hb.Telemetry.InferenceCount, hb.SinceLastSeenMS),
		MaxWindow:    hb.MaxInflight,
	}
	in := costmodel.Inputs{
		DeviceMS:   make([]*float64, len(hb.DeviceSegments)),
		Network:    hb.Network,
		InputBytes: map[int]int{},
		ServiceMS:  map[int]float64{},
	}
	for i, e := range hb.DeviceSegments {
		if e != nil {
			m := e.Mean
			in.DeviceMS[i] = &m
		}
	}
	for _, s := range hb.InputBytes {
		in.InputBytes[s.SplitPoint] = s.InputBytes
	}
	// La finestra della policy attuale: PredictAll la cambia per ogni
	// candidato.
	in.MaxInflight = st.Window()
	split := hb.Applied.Mode == api.ModeSplit
	var upload float64
	uploadOK := false
	if bytes, ok := in.InputBytes[st.Split()]; ok && hb.Network != nil {
		upload, uploadOK = hb.Network.UploadMS(bytes)
	}
	if a, ok := CycleMS(hb.Telemetry, hb.SinceLastSeenMS, split, st.Last, upload, uploadOK); ok {
		in.CycleMS = &a
	}
	var cs, weight float64
	for _, s := range hb.Service {
		in.ServiceMS[s.SplitPoint] = s.MeanMS
		if s.MeanMS > 0 {
			cs += s.StdMS / s.MeanMS * float64(s.Samples)
			weight += float64(s.Samples)
		}
	}
	if q := hb.Queue; q != nil {
		in.Queue.Slots = q.Slots
		if weight > 0 {
			in.Queue.CS = cs / weight // cs medio, pesato sui campioni
		}
		if q.InterarrivalMS != nil && q.InterarrivalStdMS != nil && *q.InterarrivalMS > 0 {
			in.Queue.CA = *q.InterarrivalStdMS / *q.InterarrivalMS
		}
		rho := 0.0
		if q.Utilization != nil {
			rho = *q.Utilization
		}
		split := st.Split()
		inSplit := split < st.Last
		in.Queue.OthersLambda, in.Queue.OthersErlangs = Others(
			q.AdmittedPerSec, rho, q.Slots, st.FramesPerSec, in.ServiceMS[split], inSplit)
	}
	st.Inputs = in
	return st
}
