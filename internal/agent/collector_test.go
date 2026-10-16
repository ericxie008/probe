package agent

import (
	"runtime"
	"testing"
	"time"
)

func TestCollector_NonNilProcs(t *testing.T) {
	c := NewCollector()
	if c.cachedProcs == nil {
		t.Fatal("expected cachedProcs to be initialized non-nil")
	}

	s := c.Collect("agent-1", "test-host")
	if s.Processes == nil {
		t.Fatal("expected s.Processes to never be nil")
	}
}

func TestCollector_Throttle(t *testing.T) {
	c := NewCollector()

	// 第一次调用触发首次采样
	start1 := time.Now()
	s1 := c.Collect("agent-1", "test-host")
	dur1 := time.Since(start1)

	// 第二次调用紧随其后（模拟 3s 间隔的下一次 tick）
	start2 := time.Now()
	s2 := c.Collect("agent-1", "test-host")
	dur2 := time.Since(start2)

	t.Logf("First collect duration: %v, second collect duration: %v", dur1, dur2)

	if s1.Processes == nil || s2.Processes == nil {
		t.Fatal("Processes slice must not be nil")
	}

	// 第二次采集应当完全走缓存，耗时应该非常低（毫秒级）
	if dur2 > 500*time.Millisecond {
		t.Errorf("second collect took too long: %v, throttling might be broken", dur2)
	}
}

func TestCollector_DisableProcs(t *testing.T) {
	c := NewCollector()
	c.DisableProcs = true

	start := time.Now()
	s := c.Collect("agent-1", "test-host")
	dur := time.Since(start)

	if s.Processes == nil {
		t.Fatal("expected s.Processes to be non-nil empty slice when disabled")
	}
	if len(s.Processes) != 0 {
		t.Errorf("expected 0 processes when DisableProcs=true, got %d", len(s.Processes))
	}
	t.Logf("Disabled procs collect duration: %v", dur)
}

func TestFastDarwinProcesses(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("skipping darwin-specific test on non-darwin OS")
	}
	c := NewCollector()
	start := time.Now()
	procs, ok := c.fastDarwinProcesses()
	dur := time.Since(start)

	if !ok {
		t.Fatal("fastDarwinProcesses failed")
	}
	t.Logf("fastDarwinProcesses returned %d procs in %v", len(procs), dur)
	if dur > 1*time.Second {
		t.Errorf("fastDarwinProcesses took too long: %v", dur)
	}
	for i, p := range procs {
		t.Logf("[%d] PID=%d Name=%s CPU=%.1f%% Mem=%d", i, p.PID, p.Name, p.CPU, p.Memory)
	}
}
