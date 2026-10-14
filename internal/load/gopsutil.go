package load

import (
	"github.com/shirou/gopsutil/v4/cpu"
	psload "github.com/shirou/gopsutil/v4/load" // alias: il nostro package si chiama già "load"
	"github.com/shirou/gopsutil/v4/mem"
)

// HostReader legge le metriche della macchina con gopsutil (Linux e macOS).
//
// È una struct vuota: non ha stato proprio. gopsutil tiene internamente il
// campione precedente della CPU, quindi CPUPercent restituisce l'uso medio
// dall'ultima chiamata, come psutil.cpu_percent(interval=None) in Python.
type HostReader struct{}

// Il "var _ Reader = HostReader{}" è un controllo a tempo di compilazione:
// se HostReader smettesse di soddisfare l'interfaccia Reader, il programma
// non compilerebbe. In Go le interfacce sono soddisfatte implicitamente,
// quindi questa riga è il modo idiomatico di dichiararlo.
var _ Reader = HostReader{}

func (HostReader) CPUPercent() (float64, error) {
	// interval 0: non blocca, confronta con la chiamata precedente.
	// percpu false: un solo valore per tutta la macchina.
	values, err := cpu.Percent(0, false)
	if err != nil {
		return 0, err
	}
	if len(values) == 0 {
		return 0, errNoValue
	}
	return values[0], nil
}

func (HostReader) CPUCount() (int, error) {
	return cpu.Counts(true) // true: core logici, come psutil.cpu_count()
}

func (HostReader) LoadAvg1() (float64, error) {
	avg, err := psload.Avg()
	if err != nil {
		return 0, err
	}
	return avg.Load1, nil
}

func (HostReader) Memory() (total, available uint64, err error) {
	vm, err := mem.VirtualMemory()
	if err != nil {
		return 0, 0, err
	}
	return vm.Total, vm.Available, nil
}
