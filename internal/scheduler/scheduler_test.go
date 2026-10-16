package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"

	"github.com/wnddd839/codebuddy-proxy/internal/billing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func tempDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	// create a fake accounts.json so DefaultPath has a sensible dir
	_ = os.WriteFile(filepath.Join(d, "proxy-accounts.json"), []byte("[]"), 0o644)
	return d
}

func TestDefaultConfigPath(t *testing.T) {
	d := tempDir(t)
	got := DefaultPath(filepath.Join(d, "proxy-accounts.json"))
	want := filepath.Join(d, "proxy-autocheckin.json")
	if got != want {
		t.Fatalf("DefaultPath = %s, want %s", got, want)
	}
}

func TestParseHM(t *testing.T) {
	cases := []struct {
		in      string
		wantH   int
		wantM   int
		wantErr bool
	}{
		{"08:00", 8, 0, false},
		{"23:59", 23, 59, false},
		{"00:00", 0, 0, false},
		{" 12:30 ", 12, 30, false},
		{"24:00", 0, 0, true},
		{"-1:00", 0, 0, true},
		{"12:60", 0, 0, true},
		{"abc", 0, 0, true},
		{"", 0, 0, true},
	}
	for _, c := range cases {
		h, m, err := parseHM(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseHM(%q) expected error, got (%d,%d)", c.in, h, m)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseHM(%q) unexpected error: %v", c.in, err)
			continue
		}
		if h != c.wantH || m != c.wantM {
			t.Errorf("parseHM(%q) = (%d,%d), want (%d,%d)", c.in, h, m, c.wantH, c.wantM)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	c := Config{Enabled: true, Time: "08:00"}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	c.Time = "bad"
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for bad time")
	}
}

func TestOpenCreatesDefault(t *testing.T) {
	d := tempDir(t)
	called := atomic.Int32{}
	svc, err := Open(filepath.Join(d, "proxy-accounts.json"), testLogger(), func(ctx context.Context, site string) billing.CheckinBatchResult {
		called.Add(1)
		return billing.CheckinBatchResult{PoolSite: site, OK: true}
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cfg := svc.Config()
	if cfg.Enabled {
		t.Error("default should be disabled")
	}
	if cfg.Time == "" {
		t.Error("default time should be set")
	}
	// File should exist on disk after a save.
	if err := svc.Update(Config{Enabled: true, Time: "08:30"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(d, "proxy-autocheckin.json"))
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	var snap configFile
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if !snap.Config.Enabled || snap.Config.Time != "08:30" {
		t.Errorf("saved config wrong: %+v", snap.Config)
	}
}

func TestReloadFromDisk(t *testing.T) {
	d := tempDir(t)
	path := filepath.Join(d, "proxy-autocheckin.json")
	snap := configFile{Version: 1, Config: Config{Enabled: true, Time: "09:15"}}
	raw, _ := json.MarshalIndent(snap, "", "  ")
	_ = os.WriteFile(path, raw, 0o644)

	called := atomic.Int32{}
	svc, err := Open(filepath.Join(d, "proxy-accounts.json"), testLogger(), func(ctx context.Context, site string) billing.CheckinBatchResult {
		called.Add(1)
		return billing.CheckinBatchResult{PoolSite: site, OK: true}
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cfg := svc.Config()
	if !cfg.Enabled || cfg.Time != "09:15" {
		t.Errorf("did not reload from disk: %+v", cfg)
	}
}

func TestRunOnce(t *testing.T) {
	d := tempDir(t)
	var called atomic.Int32
	var mu sync.Mutex
	var sitesSeen []string
	svc, err := Open(filepath.Join(d, "proxy-accounts.json"), testLogger(), func(ctx context.Context, site string) billing.CheckinBatchResult {
		mu.Lock()
		sitesSeen = append(sitesSeen, site)
		mu.Unlock()
		called.Add(1)
		return billing.CheckinBatchResult{PoolSite: site, OK: true, Summary: billing.CheckinSummary{CheckedIn: 1}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok := svc.RunOnce(); !ok {
		t.Fatal("RunOnce returned false immediately")
	}
	// Wait for both sites AND for the goroutine to finish persisting state.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if called.Load() == 2 && !svc.Running() {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if called.Load() != 2 {
		t.Fatalf("RunOnce called %d times, want 2 (domestic + global)", called.Load())
	}
	if svc.Running() {
		t.Fatal("RunOnce goroutine still running after deadline")
	}
	// Both sites covered.
	mu.Lock()
	sites := strings.Join(sitesSeen, ",")
	mu.Unlock()
	if !strings.Contains(sites, "domestic") || !strings.Contains(sites, "global") {
		t.Errorf("missing pool site calls: %v", sites)
	}
	// Result was persisted to state.
	state := svc.State()
	if state.LastDomestic == nil || state.LastGlobal == nil {
		t.Errorf("expected both site results persisted, got domestic=%v global=%v", state.LastDomestic, state.LastGlobal)
	}
}

func TestScheduledTimerFires(t *testing.T) {
	d := tempDir(t)
	var called atomic.Int32
	svc, err := Open(filepath.Join(d, "proxy-accounts.json"), testLogger(), func(ctx context.Context, site string) billing.CheckinBatchResult {
		called.Add(1)
		return billing.CheckinBatchResult{PoolSite: site, OK: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	// Set the schedule to 1 second from now.
	h, m, _ := time.Now().Add(2 * time.Second).Clock()
	_ = svc.Update(Config{Enabled: true, Time: pad2(h) + ":" + pad2(m)})

	svc.Start()
	defer svc.Stop()

	// Wait up to 10s for both sites.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if called.Load() >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if called.Load() < 2 {
		t.Fatalf("scheduler fired only %d times, want >=2", called.Load())
	}
}

// TestStartWithConfigEnabledFromDisk verifies the scheduler fires even when
// Start() is called without a prior Update() — the classic loop() hang bug.
func TestStartWithConfigEnabledFromDisk(t *testing.T) {
	d := tempDir(t)
	h, m, _ := time.Now().Add(2 * time.Second).Clock()
	soonStr := pad2(h) + ":" + pad2(m)
	// Pre-populate config JSON with enabled=true and a soon time.
	path := filepath.Join(d, "proxy-autocheckin.json")
	snap := configFile{Version: 1, Config: Config{Enabled: true, Time: soonStr}}
	raw, _ := json.MarshalIndent(snap, "", "  ")
	_ = os.WriteFile(path, raw, 0o644)

	var called atomic.Int32
	svc, err := Open(filepath.Join(d, "proxy-accounts.json"), testLogger(), func(ctx context.Context, site string) billing.CheckinBatchResult {
		called.Add(1)
		return billing.CheckinBatchResult{PoolSite: site, OK: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg := svc.Config(); !cfg.Enabled || cfg.Time != soonStr {
		t.Fatalf("did not pre-load config from disk: %+v", cfg)
	}

	// CRITICAL: no Update() call here — Start() must work off the disk config.
	svc.Start()
	defer svc.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if called.Load() >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if called.Load() < 2 {
		t.Fatalf("scheduler fired only %d times, want >=2 — loop() may have hung on first select", called.Load())
	}
}

func pad2(n int) string {
	return fmt.Sprintf("%02d", n)
}
