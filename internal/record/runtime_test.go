package record

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSampleRuntime(t *testing.T) {
	r := SampleRuntime()
	if r.Goroutines < 1 {
		t.Errorf("goroutines = %d, want at least 1 (this test)", r.Goroutines)
	}
	if r.HeapBytes == 0 {
		t.Error("heap_bytes = 0: a running program always has objects on the heap")
	}
	if r.GoMemoryBytes < r.HeapBytes {
		t.Errorf("go_memory_bytes %d < heap_bytes %d: the total includes the heap", r.GoMemoryBytes, r.HeapBytes)
	}
	if r.OpenFiles == nil {
		t.Fatal("open_files = nil on macOS/Linux: /dev/fd should be readable")
	}
	// stdin, stdout, stderr at least.
	if *r.OpenFiles < 3 {
		t.Errorf("open_files = %d, want at least 3", *r.OpenFiles)
	}
	if r.Devices != 0 {
		t.Errorf("devices = %d: the caller sets it, SampleRuntime must not", r.Devices)
	}
}

func TestSampleRuntimeCountsAnOpenFile(t *testing.T) {
	// The first file opened may also start Go's poller (one more descriptor,
	// kept for good): open one before measuring.
	warm, err := os.Create(filepath.Join(t.TempDir(), "warm"))
	if err != nil {
		t.Fatal(err)
	}
	warm.Close()
	before := SampleRuntime().OpenFiles
	if before == nil {
		t.Fatal("open_files = nil")
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "x"))
	if err != nil {
		t.Fatal(err)
	}
	after := SampleRuntime().OpenFiles
	f.Close()
	// Exactly one more: the directory opened to count must not be counted.
	if after == nil || *after != *before+1 {
		t.Errorf("open_files before %d, after opening one file %v: want %d", *before, after, *before+1)
	}
	closed := SampleRuntime().OpenFiles
	if closed == nil || *closed != *before {
		t.Errorf("open_files after closing it = %v, want %d", closed, *before)
	}
}
