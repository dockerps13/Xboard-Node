package monitor

import (
	"sync"
	"testing"
)

func TestCollectSharesHostMetrics(t *testing.T) {
	var wg sync.WaitGroup
	results := make([]Status, 16)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = Collect()
		}(i)
	}
	wg.Wait()
	for _, got := range results[1:] {
		if got.CPU != results[0].CPU || got.NumGC != results[0].NumGC || got.MemUsed != results[0].MemUsed {
			t.Fatal("concurrent reports should share one host sample")
		}
	}
	if len(results[0].CPUPerCore) > 0 {
		want := results[1].CPUPerCore[0]
		results[0].CPUPerCore[0] = -123
		if results[1].CPUPerCore[0] != want {
			t.Fatal("sample returned shared mutable CPU slice")
		}
	}
}
