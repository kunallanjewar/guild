package daemon

import (
	"context"
	"sync"
	"testing"
	"time"
)

// captureRecorder is a test DecisionRecorder that stores every recorded
// decision for assertions. Safe for concurrent use (the loops record off
// their own goroutines).
type captureRecorder struct {
	mu   sync.Mutex
	recs []Decision
}

func (c *captureRecorder) Record(d Decision) {
	c.mu.Lock()
	c.recs = append(c.recs, d)
	c.mu.Unlock()
}

func (c *captureRecorder) all() []Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Decision, len(c.recs))
	copy(out, c.recs)
	return out
}

func (c *captureRecorder) byKind(k DecisionKind) []Decision {
	var out []Decision
	for _, d := range c.all() {
		if d.Kind == k {
			out = append(out, d)
		}
	}
	return out
}

// installRecorder sets the package sink to r and restores nil at test end, so
// each test is isolated and the default (no recorder) is restored.
func installRecorder(t *testing.T, r DecisionRecorder) {
	t.Helper()
	SetDecisionRecorder(r)
	t.Cleanup(func() { SetDecisionRecorder(nil) })
}

// TestRecordDecision_NoRecorder_IsNoop verifies the default state (no
// recorder installed) is a safe no-op: this is the disabled-observability
// path the parity bar requires.
func TestRecordDecision_NoRecorder_IsNoop(t *testing.T) {
	SetDecisionRecorder(nil)
	// Must not panic and must record nothing observable.
	recordDecision(Decision{Kind: DecisionAutopass, Allow: true, Reason: "fire"})
}

// TestAutopassDecision_CapturesBooleans checks the autopass value object
// captures every input boolean and the right reason for each short-circuit,
// WITHOUT the recorded Allow diverging from canFire's bool.
func TestAutopassDecision_CapturesBooleans(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		cfg        SchedulerConfig
		setup      func(s *Scheduler)
		wantAllow  bool
		wantReason string
	}{
		{
			name:       "disabled",
			cfg:        SchedulerConfig{Enabled: false},
			wantAllow:  false,
			wantReason: "disabled",
		},
		{
			name: "pass_in_flight",
			cfg:  SchedulerConfig{Enabled: true, Idle: time.Minute, Pass: noopPass},
			setup: func(s *Scheduler) {
				s.running = true
				s.lastActivity = now.Add(-2 * time.Minute)
			},
			wantAllow:  false,
			wantReason: "pass_in_flight",
		},
		{
			name: "not_idle",
			cfg:  SchedulerConfig{Enabled: true, Idle: time.Minute, Pass: noopPass},
			setup: func(s *Scheduler) {
				s.lastActivity = now // just touched
			},
			wantAllow:  false,
			wantReason: "not_idle",
		},
		{
			name: "gap_not_elapsed",
			cfg:  SchedulerConfig{Enabled: true, Idle: time.Minute, Pass: noopPass},
			setup: func(s *Scheduler) {
				s.lastActivity = now.Add(-2 * time.Minute)
				s.lastPassEnded = now.Add(-30 * time.Second) // inside the gap
			},
			wantAllow:  false,
			wantReason: "gap_not_elapsed",
		},
		{
			name: "fire",
			cfg:  SchedulerConfig{Enabled: true, Idle: time.Minute, Pass: noopPass},
			setup: func(s *Scheduler) {
				s.lastActivity = now.Add(-2 * time.Minute)
			},
			wantAllow:  true,
			wantReason: "fire",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Logger = quietLogger()
			s := NewScheduler(tc.cfg)
			s.lastActivity = now // default seed; setup overrides
			if tc.setup != nil {
				tc.setup(s)
			}
			d := s.autopassDecision(now)
			if d.Allow != tc.wantAllow {
				t.Errorf("Allow = %v, want %v", d.Allow, tc.wantAllow)
			}
			if d.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", d.Reason, tc.wantReason)
			}
			// The value object's Allow must match canFire's bool: parity.
			if got := s.canFire(now); got != d.Allow {
				t.Errorf("canFire=%v but decision.Allow=%v: must agree", got, d.Allow)
			}
			// Every boolean input must be present.
			for _, k := range []string{"armed", "not_running", "idle_elapsed", "gap_elapsed"} {
				if _, ok := d.Inputs[k]; !ok {
					t.Errorf("missing input boolean %q", k)
				}
			}
		})
	}
}

func noopPass(context.Context, time.Duration) (PassOutcome, error) {
	return PassOutcome{}, nil
}

// TestScheduler_RecordsAutopass_OnFireAndSkip verifies the scheduler records
// both a skipped tick (not due) and a fired tick, and that recording does not
// change firing behavior.
func TestScheduler_RecordsAutopass_OnFireAndSkip(t *testing.T) {
	rec := &captureRecorder{}
	installRecorder(t, rec)

	fired := make(chan struct{}, 1)
	clk := newFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	s := NewScheduler(SchedulerConfig{
		Enabled: true,
		Idle:    time.Minute,
		Budget:  time.Second,
		clock:   clk,
		Logger:  quietLogger(),
		Pass: func(context.Context, time.Duration) (PassOutcome, error) {
			fired <- struct{}{}
			return PassOutcome{Steps: 1}, nil
		},
	})
	_, stop := runScheduler(t, s)
	defer stop()

	// Not idle yet: a tick records a skip and does not fire.
	clk.tick()
	clk.tick() // barrier: the first tick was consumed

	skips := rec.byKind(DecisionAutopass)
	if len(skips) == 0 {
		t.Fatal("expected at least one recorded autopass decision after a skip tick")
	}
	for _, d := range skips {
		if d.Allow {
			t.Fatalf("expected skip (not idle), got Allow=true reason=%q", d.Reason)
		}
	}

	// Advance past idle so the next tick fires.
	clk.advance(2 * time.Minute)
	clk.tick()
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("pass did not fire after idle elapsed")
	}

	// A fire decision (Allow=true) must have been recorded.
	var sawFire bool
	for _, d := range rec.byKind(DecisionAutopass) {
		if d.Allow && d.Reason == "fire" {
			sawFire = true
		}
	}
	if !sawFire {
		t.Error("expected a recorded autopass decision with Allow=true reason=fire")
	}
}

// TestReaper_RecordsSweepDecision verifies the reaper records the sweep
// outcome with the right booleans and metrics, and that recording does not
// change the forfeited tally.
func TestReaper_RecordsSweepDecision(t *testing.T) {
	rec := &captureRecorder{}
	installRecorder(t, rec)

	r, clk := newTestReaper(t, ReaperConfig{
		Interval: time.Minute,
		Reap: func(context.Context) (ReapOutcome, error) {
			return ReapOutcome{Scanned: 3, Forfeited: 2, SkippedLive: 1}, nil
		},
	})
	stop := runReaper(t, r)
	defer stop()

	clk.tick()
	clk.tick() // barrier

	recs := rec.byKind(DecisionLeaseReap)
	if len(recs) == 0 {
		t.Fatal("expected a recorded lease_reap decision")
	}
	d := recs[0]
	if !d.Allow {
		t.Errorf("Allow = false, want true (forfeited 2)")
	}
	if d.Reason != "forfeited_zombie_claims" {
		t.Errorf("Reason = %q, want forfeited_zombie_claims", d.Reason)
	}
	if d.Metrics["forfeited"] != 2 {
		t.Errorf("forfeited metric = %d, want 2", d.Metrics["forfeited"])
	}
	if d.Inputs["errored"] {
		t.Error("errored input should be false on a clean sweep")
	}
	// Parity: the reaper tallies exactly 2 forfeits per sweep regardless of
	// recording. Each fake-clock tick drives one sweep, so the running total
	// is 2 * (number of sweeps that produced a record). Recording does not
	// inflate it: the totalForfeited add lives in sweep(), untouched by the
	// recordDecision call.
	if got, want := r.TotalForfeited(), int64(2*len(recs)); got != want {
		t.Errorf("TotalForfeited = %d, want %d (2 per recorded sweep; recording must not change it)", got, want)
	}
}

// TestSetDecisionRecorder_Removable verifies installing then removing the
// recorder restores the no-op path: a decision after removal is not recorded.
func TestSetDecisionRecorder_Removable(t *testing.T) {
	rec := &captureRecorder{}
	SetDecisionRecorder(rec)
	recordDecision(Decision{Kind: DecisionAutopass, Allow: true})
	if len(rec.all()) != 1 {
		t.Fatalf("expected 1 record while installed, got %d", len(rec.all()))
	}
	SetDecisionRecorder(nil)
	recordDecision(Decision{Kind: DecisionAutopass, Allow: false})
	if len(rec.all()) != 1 {
		t.Errorf("expected still 1 record after removal, got %d", len(rec.all()))
	}
}
