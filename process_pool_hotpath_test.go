package subp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// buildEchoWorker compiles a worker that answers every command at once. When
// the command carries "big": N it answers with an N-byte "app" string — the
// shape of an SSR render (one big HTML field in a single JSON line).
func buildEchoWorker(tb testing.TB) (string, func()) {
	tb.Helper()
	tmpDir, err := os.MkdirTemp("", "subp_hotpath")
	if err != nil {
		tb.Fatal(err)
	}
	src := `package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

func main() {
	out := json.NewEncoder(os.Stdout)
	out.SetEscapeHTML(false) // like node's JSON.stringify: '<' stays '<'
	out.Encode(map[string]interface{}{"type": "ready"})
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var cmd map[string]interface{}
		if json.Unmarshal(sc.Bytes(), &cmd) != nil {
			continue
		}
		resp := map[string]interface{}{"type": "success", "id": cmd["id"]}
		if n, ok := cmd["big"].(float64); ok && n > 0 {
			resp["output"] = map[string]interface{}{"app": strings.Repeat("<p>ssr</p>", int(n)/10)}
		}
		out.Encode(resp)
	}
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "w.go"), []byte(src), 0o644); err != nil {
		tb.Fatal(err)
	}
	bin := filepath.Join(tmpDir, "w")
	if out, err := exec.Command("go", "build", "-o", bin, filepath.Join(tmpDir, "w.go")).CombinedOutput(); err != nil {
		tb.Fatalf("build worker: %v\n%s", err, out)
	}
	return bin, func() { os.RemoveAll(tmpDir) }
}

func newEchoPool(tb testing.TB, bin string, size int) *ProcessPool {
	tb.Helper()
	logger := zerolog.New(zerolog.Nop())
	pool := NewProcessPool("hotpath", size, &logger, filepath.Dir(bin), bin, nil,
		5*time.Second, 5*time.Second, 5*time.Second)
	if err := pool.WaitForReadyAllProcess(); err != nil {
		tb.Fatal(err)
	}
	return pool
}

// The dispatcher is one goroutine that never returns. A `defer timer.Stop()`
// inside its loop is never run, so every dispatched command left a deferred
// record + its *time.Timer on the heap for the life of the process. This test
// pins that the dispatcher's heap does not grow with the number of commands.
func TestDispatcherDoesNotRetainPerCommandState(t *testing.T) {
	bin, cleanup := buildEchoWorker(t)
	defer cleanup()
	pool := newEchoPool(t, bin, 2)
	defer pool.StopAll()

	run := func(n int) {
		for i := 0; i < n; i++ {
			if _, err := pool.SendCommand(map[string]interface{}{"data": "x"}); err != nil {
				t.Fatalf("command %d: %v", i, err)
			}
		}
	}
	var heapBytes uint64
	heapObjects := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		heapBytes = m.HeapAlloc
		return m.HeapObjects
	}

	run(500) // warm up maps/channels
	before := heapObjects()
	bytesBefore := heapBytes
	const n = 20000
	run(n)
	after := heapObjects()
	t.Logf("heap bytes: growth=%d (%.0f B/command)", int64(heapBytes)-int64(bytesBefore), float64(int64(heapBytes)-int64(bytesBefore))/n)

	growth := int64(after) - int64(before)
	t.Logf("heap objects: before=%d after=%d growth=%d over %d commands (%.3f/command)",
		before, after, growth, n, float64(growth)/n)
	if growth > n/10 {
		t.Fatalf("heap grew by %d objects over %d commands — per-command state is being retained", growth, n)
	}
}

// BenchmarkSendCommandSSRSized: one ~200 KB response line per command, the
// size class of a rendered page coming back from the SSR worker.
func BenchmarkSendCommandSSRSized(b *testing.B) {
	bin, cleanup := buildEchoWorker(b)
	defer cleanup()
	pool := newEchoPool(b, bin, 1)
	defer pool.StopAll()
	input := strings.Repeat("{\"k\":\"v\"}", 2000) // ~18 KB input, like fwData JSON
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := pool.SendCommand(map[string]interface{}{"input": input, "big": 200000}); err != nil {
			b.Fatal(err)
		}
	}
}
