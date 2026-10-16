// Package scheduler provides a daily auto-checkin scheduler that runs
// billing.RunPoolCheckin for both domestic and global pool sites at a
// configured local time.
//
// Configuration is persisted to a JSON file alongside the accounts / usage
// files (proxy-autocheckin.json) and can be updated at runtime via the
// admin API.  The scheduler uses time.NewTimer and recomputes the next
// trigger time after each run, so it is accurate to the second.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wnddd839/codebuddy-proxy/internal/atomicwrite"
	"github.com/wnddd839/codebuddy-proxy/internal/billing"
)

// Config controls the scheduler behaviour.  Time is in local time, "HH:MM".
type Config struct {
	Enabled bool   `json:"enabled"`
	Time    string `json:"time"` // "HH:MM", 24-hour
}

// Validate returns a human-readable error if c is invalid.
func (c Config) Validate() error {
	if c.Time != "" {
		if _, _, err := parseHM(c.Time); err != nil {
			return fmt.Errorf("invalid time %q: %w", c.Time, err)
		}
	}
	return nil
}

// DefaultConfig is used when no JSON file exists yet.
func DefaultConfig() Config {
	return Config{Enabled: false, Time: "08:00"}
}

// ParseConfigFromEnv allows operators to bootstrap via env without touching
// the admin UI — useful for headless / docker deployments where accounts
// are pre-configured.  Empty strings are ignored (JSON file wins).
func ParseConfigFromEnv() Config {
	cfg := Config{}
	if v := strings.TrimSpace(os.Getenv("CODEBUDDY_AUTO_CHECKIN_ENABLED")); v != "" {
		cfg.Enabled = OptionalBool(v, false)
	}
	if v := strings.TrimSpace(os.Getenv("CODEBUDDY_AUTO_CHECKIN_TIME")); v != "" {
		if _, _, err := parseHM(v); err == nil {
			cfg.Time = v
		}
	}
	return cfg
}

// OptionalBool is a tiny helper used instead of importing config just for
// one function — the scheduler wants to avoid a circular dependency with
// config.Load.
func OptionalBool(raw string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// parseHM parses "HH:MM" into hour (0-23) and minute (0-59).
func parseHM(s string) (hour, minute int, err error) {
	s = strings.TrimSpace(s)
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, 0, fmt.Errorf("expected HH:MM")
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("hour/minute out of range: %d:%d", h, m)
	}
	return h, m, nil
}

// configFile is the on-disk snapshot — kept versioned so we can evolve it.
type configFile struct {
	Version      int         `json:"version"`
	Config       Config      `json:"config"`
	LastRun      time.Time   `json:"lastRun,omitempty"`
	LastDay      string      `json:"lastDay,omitempty"` // "2006-01-02"
	LastDomestic *lastResult `json:"lastDomestic,omitempty"`
	LastGlobal   *lastResult `json:"lastGlobal,omitempty"`
	LastResult   *lastResult `json:"lastResult,omitempty"` // deprecated: kept for backward compat, equals LastGlobal
}

type lastResult struct {
	At       time.Time              `json:"at"`
	PoolSite string                 `json:"poolSite"`
	Summary  billing.CheckinSummary `json:"summary"`
	OK       bool                   `json:"ok"`
	Note     string                 `json:"note,omitempty"`
}

// Service is the scheduler.  Safe for concurrent use — config writes go
// through the exported Update method which reloads the timer.
type Service struct {
	path string
	log  *slog.Logger

	mu    sync.Mutex
	cfg   Config
	state configFile

	running atomic.Bool // true while a checkin is in flight
	stopCh  chan struct{}
	doneCh  chan struct{} // closed when loop() has exited
	timer   *time.Timer
	wakeCh  chan struct{}
	// RunFunc is injected by main for tests — real impl uses gateway.
	RunFunc func(ctx context.Context, site string) billing.CheckinBatchResult
}

// DefaultPath returns proxy-autocheckin.json sibling to accountsPath.
func DefaultPath(accountsPath string) string {
	accountsPath = strings.TrimSpace(accountsPath)
	if accountsPath == "" {
		accountsPath = filepath.Join(".", "proxy-accounts.json")
	}
	return filepath.Join(filepath.Dir(accountsPath), "proxy-autocheckin.json")
}

// Open loads or creates a scheduler.  runFunc runs checkin for one pool
// site (typically billing.RunPoolCheckin bound to the gateway service).
func Open(accountsPath string, log *slog.Logger, runFunc func(ctx context.Context, site string) billing.CheckinBatchResult) (*Service, error) {
	if runFunc == nil {
		return nil, errors.New("scheduler: RunFunc must not be nil")
	}
	path := DefaultPath(accountsPath)
	s := &Service{
		path:    path,
		log:     log,
		RunFunc: runFunc,
		wakeCh:  make(chan struct{}, 1),
	}
	loaded := true
	if err := s.load(); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("load auto-checkin config: %w", err)
		}
		loaded = false
	}
	if s.state.Version == 0 {
		s.state.Version = 1
	}
	if s.state.Config.Time == "" {
		s.state.Config = DefaultConfig()
	}
	// Bootstrap from env only when no JSON file exists yet — on-disk config
	// always wins once the UI (or an operator) has persisted it.
	if !loaded {
		env := ParseConfigFromEnv()
		if env.Time != "" {
			s.state.Config.Time = env.Time
		}
		if env.Enabled {
			s.state.Config.Enabled = true
		}
	}
	s.cfg = s.state.Config
	return s, nil
}

func (s *Service) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var snap configFile
	if err := json.Unmarshal(raw, &snap); err != nil {
		return err
	}
	s.state = snap
	s.cfg = snap.Config
	return nil
}

func (s *Service) save() error {
	s.state.Config = s.cfg
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	return atomicwrite.Write(s.path, raw, 0o644)
}

// Config returns the current snapshot (for the admin UI).
func (s *Service) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// State returns the full persisted state including last run info.
func (s *Service) State() configFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Update changes config, persists it, and reschedules the timer if running.
func (s *Service) Update(newCfg Config) error {
	if err := newCfg.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = newCfg
	if err := s.save(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	// Wake the loop so it recomputes the schedule immediately.
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
	return nil
}

// Start begins the scheduling loop.  Not safe to call twice without Stop.
func (s *Service) Start() {
	s.mu.Lock()
	if s.stopCh != nil {
		s.mu.Unlock()
		return
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	s.stopCh = stopCh
	s.doneCh = doneCh
	s.mu.Unlock()
	go func() {
		defer close(doneCh)
		s.loop(stopCh)
	}()
}

// Stop terminates the scheduling loop and blocks until it has exited.
// Safe to call multiple times.
func (s *Service) Stop() {
	s.mu.Lock()
	ch := s.stopCh
	done := s.doneCh
	t := s.timer
	s.stopCh = nil
	s.doneCh = nil
	s.timer = nil
	s.mu.Unlock()
	if ch == nil {
		return
	}
	close(ch)
	if t != nil {
		t.Stop()
	}
	<-done
}

func (s *Service) loop(stopCh <-chan struct{}) {
	// On startup: if today's target time has passed and we haven't run today,
	// do an immediate catch-up run.  This runs in a goroutine so it does not
	// block Stop() responsiveness.
	go s.maybeCatchup(stopCh)
	for {
		d := s.nextDelay()
		s.mu.Lock()
		if s.timer != nil {
			s.timer.Stop()
		}
		t := time.NewTimer(d)
		s.timer = t
		s.mu.Unlock()
		select {
		case <-stopCh:
			return
		case <-s.wakeCh:
			// config changed — recompute next trigger on next iteration
			continue
		case <-t.C:
			s.runNow()
		}
	}
}

// nextDelay returns how long until the next configured local time.
// If disabled or time unset it returns a long grace period so the loop
// stays alive but idle (wakeCh will re-evaluate on config change).
func (s *Service) nextDelay() time.Duration {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	if !cfg.Enabled || cfg.Time == "" {
		// Re-check every 30s when disabled so toggling takes effect quickly.
		return 30 * time.Second
	}
	h, m, err := parseHM(cfg.Time)
	if err != nil {
		return 30 * time.Second
	}
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	d := time.Until(next)
	if d <= 0 {
		d = time.Second
	}
	return d
}

// maybeCatchup runs checkin immediately if the schedule was already due
// today but we haven't executed it yet (first boot after deploy / restart).
func (s *Service) maybeCatchup(stopCh <-chan struct{}) {
	s.mu.Lock()
	cfg := s.cfg
	lastDay := s.state.LastDay
	s.mu.Unlock()
	if !cfg.Enabled || cfg.Time == "" {
		return
	}
	h, m, err := parseHM(cfg.Time)
	if err != nil {
		return
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	// Already ran today — nothing to do.
	if lastDay == today {
		return
	}
	targetToday := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	// If we're past today's target OR within 1 min of it, run now.
	if !now.Before(targetToday.Add(-time.Minute)) {
		// Never start a catchup after Stop has been requested.
		select {
		case <-stopCh:
			return
		default:
		}
		s.log.Info("auto-checkin catchup: today's scheduled time already passed, running now", "time", cfg.Time)
		s.runNow()
	}
}

// runNow executes checkin for both pool sites and records both results.
// Idempotent at the per-day level: if we're already running we skip.
func (s *Service) runNow() {
	if !s.running.CompareAndSwap(false, true) {
		s.log.Info("auto-checkin already in flight, skipping")
		return
	}
	defer s.running.Store(false)
	s.executeCheckin()
}

// executeCheckin is the body of one checkin run — both pool sites + persist.
// Assumes s.running has already been flipped to true by the caller.
func (s *Service) executeCheckin() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	began := time.Now()
	domestic := s.runSite(ctx, "domestic")
	global := s.runSite(ctx, "global")

	s.mu.Lock()
	s.state.LastRun = began
	s.state.LastDay = began.Format("2006-01-02")
	s.state.LastDomestic = domestic
	s.state.LastGlobal = global
	s.state.LastResult = global // backward compat
	_ = s.save()
	s.mu.Unlock()
}

func (s *Service) runSite(ctx context.Context, site string) *lastResult {
	res := s.RunFunc(ctx, site)
	s.log.Info("auto-checkin completed",
		"poolSite", site,
		"ok", res.OK,
		"checkedIn", res.Summary.CheckedIn,
		"alreadyDone", res.Summary.AlreadyDone,
		"failed", res.Summary.Failed,
		"skipped", res.Summary.Skipped,
		"unsupported", res.Summary.Unsupported,
	)
	return &lastResult{
		At:       time.Now(),
		PoolSite: site,
		Summary:  res.Summary,
		OK:       res.OK,
		Note:     res.Note,
	}
}

// RunOnce forces a checkin now (used by admin UI "run now" button).
// Returns false if a checkin is already running.  Spawns a goroutine and
// returns immediately; state is persisted when the goroutine finishes.
func (s *Service) RunOnce() bool {
	if !s.running.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer s.running.Store(false)
		s.executeCheckin()
	}()
	return true
}

// Running returns whether a checkin is currently in flight.
func (s *Service) Running() bool { return s.running.Load() }
