package cli

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/i18n"
)

func TestTUIDiagnosticsKeepProcessAndPluginLogsOffTerminal(t *testing.T) {
	var terminal bytes.Buffer
	beforeTest := slog.Default()
	terminalLogger := slog.New(slog.NewTextHandler(&terminal, nil))
	slog.SetDefault(terminalLogger)
	t.Cleanup(func() { slog.SetDefault(beforeTest) })

	d := startTUIDiagnostics(t.TempDir())
	t.Cleanup(d.Close)
	slog.Warn("controller: snapshot conflict", "path", "private-session.jsonl")
	fmt.Fprintln(d.Writer(), "plugin diagnostic")
	if got := terminal.String(); got != "" {
		t.Fatalf("terminal received diagnostics while TUI owned it: %q", got)
	}

	logPath := d.path
	d.Close()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read TUI diagnostic log: %v", err)
	}
	got := string(data)
	for _, want := range []string{"controller: snapshot conflict", "private-session.jsonl", "plugin diagnostic"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostic log = %q, want %q", got, want)
		}
	}

	slog.Warn("after TUI")
	if got := terminal.String(); !strings.Contains(got, "after TUI") {
		t.Fatalf("previous logger was not restored after TUI close: %q", got)
	}
}

func TestTUIDiagnosticsFallBackToDiscardWithoutLeakingToTerminal(t *testing.T) {
	var terminal bytes.Buffer
	beforeTest := slog.Default()
	terminalLogger := slog.New(slog.NewTextHandler(&terminal, nil))
	slog.SetDefault(terminalLogger)
	t.Cleanup(func() { slog.SetDefault(beforeTest) })

	blockedHome := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedHome, []byte("file"), 0o600); err != nil {
		t.Fatalf("seed blocked home: %v", err)
	}
	d := startTUIDiagnostics(blockedHome)
	defer d.Close()

	slog.Warn("must stay off terminal")
	fmt.Fprintln(d.Writer(), "plugin must stay off terminal")
	if got := terminal.String(); got != "" {
		t.Fatalf("fallback leaked diagnostics to terminal: %q", got)
	}
	if d.path != "" {
		t.Fatalf("fallback diagnostic path = %q, want empty", d.path)
	}
}

func TestBoundedDiagnosticWriterStopsAtLimit(t *testing.T) {
	var dst bytes.Buffer
	w := &boundedDiagnosticWriter{dst: &dst, remaining: 8}
	payload := strings.Repeat("x", 32)
	n, err := io.WriteString(w, payload)
	if err != nil || n != len(payload) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(payload))
	}
	if !strings.HasPrefix(dst.String(), strings.Repeat("x", 8)) {
		t.Fatalf("bounded output = %q, want eight payload bytes first", dst.String())
	}
	if !strings.Contains(dst.String(), "diagnostic log limit reached") {
		t.Fatalf("bounded output = %q, want truncation marker", dst.String())
	}
	before := dst.Len()
	if _, err := io.WriteString(w, "more"); err != nil {
		t.Fatalf("discard after cap: %v", err)
	}
	if dst.Len() != before {
		t.Fatalf("writer grew after cap: before=%d after=%d", before, dst.Len())
	}
}

func TestCLIProfileBuildOptionsPropagateInteractiveOwners(t *testing.T) {
	var diagnostic bytes.Buffer
	recovered := false
	onRecovered := func(control.SessionRecoveryInfo) error {
		recovered = true
		return nil
	}
	opts := cliProfileBuildOptions("provider/model", 9, false, event.Discard, "delivery", cliBuildOverrides{
		WorkspaceRoot:        "/workspace",
		HeadlessApprovalMode: control.ToolApprovalAuto,
		Stderr:               &diagnostic,
		OnSessionRecovered:   onRecovered,
	})

	if opts.Stderr != &diagnostic {
		t.Fatalf("Stderr = %T, want caller-owned diagnostic writer", opts.Stderr)
	}
	if opts.OnSessionRecovered == nil {
		t.Fatal("OnSessionRecovered was dropped from CLI build options")
	}
	if err := opts.OnSessionRecovered(control.SessionRecoveryInfo{RecoveryPath: "recovery.jsonl"}); err != nil {
		t.Fatalf("OnSessionRecovered: %v", err)
	}
	if !recovered {
		t.Fatal("propagated recovery callback was not invoked")
	}
	if opts.HeadlessApprovalMode != control.ToolApprovalAuto {
		t.Fatalf("HeadlessApprovalMode = %q, want %q", opts.HeadlessApprovalMode, control.ToolApprovalAuto)
	}
}

func TestCLIProfileBuildOptionsUseResolvedLocaleForAutoPricing(t *testing.T) {
	defer i18n.DetectLanguage("en")
	for _, tt := range []struct {
		language string
		want     string
	}{
		{language: "en", want: "USD"},
		{language: "zh", want: "CNY"},
		{language: "zh-TW", want: "CNY"},
	} {
		i18n.DetectLanguage(tt.language)
		opts := cliProfileBuildOptions("provider/model", 0, false, event.Discard, "balanced", cliBuildOverrides{})
		if opts.AutoPricingCurrency != tt.want {
			t.Errorf("language %q auto pricing currency = %q, want %q", tt.language, opts.AutoPricingCurrency, tt.want)
		}
	}
}

func TestTUIDiagnosticsMilestoneFlushesNonEmptyLog(t *testing.T) {
	home := t.TempDir()
	d := startTUIDiagnostics(home)
	t.Cleanup(d.Close)
	d.Milestone("config_load_begin")
	d.Milestone("controller_build_done")
	if d.Path() == "" {
		t.Fatal("expected diagnostic log path")
	}
	body, err := os.ReadFile(d.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Fatal("diagnostic log must not be empty after milestones")
	}
	for _, want := range []string{"diagnostics_started", "config_load_begin", "controller_build_done"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("log missing %q:\n%s", want, body)
		}
	}
}

// fakeWatchClock drives the stall watchdog without real sleeps.
type fakeWatchClock struct {
	now time.Time
}

func newWatchdogForTest(t *testing.T, clock *fakeWatchClock) *tuiDiagnostics {
	t.Helper()
	d := &tuiDiagnostics{
		writer:    io.Discard,
		stopWatch: make(chan struct{}),
		phase:     watchdogBooting,
		nowFn:     func() time.Time { return clock.now },
		dumpFn:    func(string) {},
		killFn:    func() {},
		logFn:     func(string, ...any) {},
	}
	d.lastHeartbeat = clock.now
	d.lastHeartbeatSource = "test_start"
	t.Cleanup(d.Close)
	return d
}

func TestWatchdogIdleNeverEscalates(t *testing.T) {
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	d.NoteBooted()
	if d.phaseForTest() != watchdogIdle {
		t.Fatalf("phase = %s, want idle", d.phaseForTest())
	}
	// Idle for well over the stall threshold.
	for range 30 {
		clock.now = clock.now.Add(time.Second)
		d.onTick(clock.now)
	}
	if got := d.dumpCalls.Load(); got != 0 {
		t.Fatalf("idle dumpCalls = %d, want 0", got)
	}
	if got := d.cancelCalls.Load(); got != 0 {
		t.Fatalf("idle cancelCalls = %d, want 0", got)
	}
	if got := d.killCalls.Load(); got != 0 {
		t.Fatalf("idle killCalls = %d, want 0", got)
	}
}

func TestWatchdogBootStallDumpsAndKills(t *testing.T) {
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	// Stay in booting; no NoteBooted.
	clock.now = clock.now.Add(tuiWatchdogStall)
	d.onTick(clock.now)
	if d.dumpCalls.Load() != 1 {
		t.Fatalf("boot dumpCalls = %d, want 1", d.dumpCalls.Load())
	}
	if d.killCalls.Load() != 1 {
		t.Fatalf("boot killCalls = %d, want 1", d.killCalls.Load())
	}
	if d.cancelCalls.Load() != 0 {
		t.Fatalf("boot cancelCalls = %d, want 0 (no controller)", d.cancelCalls.Load())
	}
	// Repeat ticks must not re-kill.
	clock.now = clock.now.Add(time.Second)
	d.onTick(clock.now)
	if d.killCalls.Load() != 1 {
		t.Fatalf("boot re-kill = %d, want 1", d.killCalls.Load())
	}
}

func TestWatchdogRunningElapsedHeartbeatPreventsKill(t *testing.T) {
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	d.NoteBooted()
	d.NoteRunning(func() {})
	// Simulate a long turn with a heartbeat every second.
	for range 60 {
		clock.now = clock.now.Add(time.Second)
		d.NoteActiveHeartbeat("elapsed_tick")
		d.onTick(clock.now)
	}
	if d.dumpCalls.Load() != 0 || d.cancelCalls.Load() != 0 || d.killCalls.Load() != 0 {
		t.Fatalf("healthy running escalated: dump=%d cancel=%d kill=%d",
			d.dumpCalls.Load(), d.cancelCalls.Load(), d.killCalls.Load())
	}
}

func TestWatchdogRunningStallEscalatesDumpCancelThenKill(t *testing.T) {
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	d.NoteBooted()
	cancelCh := make(chan struct{}, 1)
	d.NoteRunning(func() {
		select {
		case cancelCh <- struct{}{}:
		default:
		}
	})

	// Stall for 10s with no heartbeat.
	clock.now = clock.now.Add(tuiWatchdogStall)
	d.onTick(clock.now)
	if d.dumpCalls.Load() != 1 {
		t.Fatalf("stall dumpCalls = %d, want 1", d.dumpCalls.Load())
	}
	// Cancel is invoked from a goroutine; wait briefly via channel without sleep-loops
	// that depend on wall clock beyond a generous select timeout for scheduling.
	select {
	case <-cancelCh:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel was not invoked after stall dump")
	}
	if d.cancelCalls.Load() != 1 {
		t.Fatalf("cancelCalls = %d, want 1", d.cancelCalls.Load())
	}
	if d.killCalls.Load() != 0 {
		t.Fatalf("killCalls = %d before grace, want 0", d.killCalls.Load())
	}

	// Still within grace window — no kill.
	clock.now = clock.now.Add(tuiWatchdogCancelGrace - 100*time.Millisecond)
	d.onTick(clock.now)
	if d.killCalls.Load() != 0 {
		t.Fatalf("killCalls during grace = %d, want 0", d.killCalls.Load())
	}

	// Grace expires, still no heartbeat → hard-kill once.
	clock.now = clock.now.Add(200 * time.Millisecond)
	d.onTick(clock.now)
	if d.killCalls.Load() != 1 {
		t.Fatalf("killCalls after grace = %d, want 1", d.killCalls.Load())
	}
	// Repeat tick does not re-kill.
	clock.now = clock.now.Add(time.Second)
	d.onTick(clock.now)
	if d.killCalls.Load() != 1 || d.cancelCalls.Load() != 1 {
		t.Fatalf("duplicate escalation: cancel=%d kill=%d", d.cancelCalls.Load(), d.killCalls.Load())
	}
}

func TestWatchdogGraceHeartbeatAbortsKill(t *testing.T) {
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	d.NoteBooted()
	d.NoteRunning(func() {})

	clock.now = clock.now.Add(tuiWatchdogStall)
	d.onTick(clock.now)
	if d.dumpCalls.Load() != 1 {
		t.Fatalf("dumpCalls = %d, want 1", d.dumpCalls.Load())
	}

	// Heartbeat during grace aborts hard-kill.
	clock.now = clock.now.Add(time.Second)
	d.NoteActiveHeartbeat("elapsed_tick")
	clock.now = clock.now.Add(tuiWatchdogCancelGrace)
	d.onTick(clock.now)
	if d.killCalls.Load() != 0 {
		t.Fatalf("killCalls after heartbeat = %d, want 0", d.killCalls.Load())
	}
}

// TestWatchdogCancelOncePerGeneration pins "one Cancel per Turn": after a grace
// abort via heartbeat, a later stall on the same generation may dump/kill but
// must not invoke Cancel() again.
func TestWatchdogCancelOncePerGeneration(t *testing.T) {
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	d.NoteBooted()
	// cancelCalls is incremented before the hook body, so assertions below do not
	// need to wait for a separate scheduler turn.
	d.NoteRunning(func() {})

	// First stall → cancel once.
	clock.now = clock.now.Add(tuiWatchdogStall)
	d.onTick(clock.now)
	if d.cancelCalls.Load() != 1 {
		t.Fatalf("first cancelCalls = %d, want 1", d.cancelCalls.Load())
	}
	// Heartbeat aborts grace (cancelIssued stays sticky).
	clock.now = clock.now.Add(time.Second)
	d.NoteActiveHeartbeat("elapsed_tick")
	// Second stall on the same generation.
	clock.now = clock.now.Add(tuiWatchdogStall)
	d.onTick(clock.now)
	if d.cancelCalls.Load() != 1 {
		t.Fatalf("second stall re-canceled: cancelCalls=%d, want 1", d.cancelCalls.Load())
	}
	if d.dumpCalls.Load() != 2 {
		t.Fatalf("second stall dumpCalls = %d, want 2 (re-dump allowed)", d.dumpCalls.Load())
	}
	// Grace after second escalation still hard-kills once.
	clock.now = clock.now.Add(tuiWatchdogCancelGrace)
	d.onTick(clock.now)
	if d.killCalls.Load() != 1 {
		t.Fatalf("killCalls after second grace = %d, want 1", d.killCalls.Load())
	}
}

func TestWatchdogStaleCancelCannotAffectNewGeneration(t *testing.T) {
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	d.NoteBooted()
	var oldCancelCalls atomic.Int32
	d.NoteRunning(func() { oldCancelCalls.Add(1) })
	oldGeneration := d.generationForTest()
	d.NoteIdle()
	d.NoteRunning(func() {})

	d.cancelCurrentGeneration(oldGeneration, func() { oldCancelCalls.Add(1) })
	if got := oldCancelCalls.Load(); got != 0 {
		t.Fatalf("stale cancellation invoked old callback %d times, want 0", got)
	}
}

func TestWatchdogTurnDoneDuringGraceAbortsKill(t *testing.T) {
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	d.NoteBooted()
	d.NoteRunning(func() {})

	clock.now = clock.now.Add(tuiWatchdogStall)
	d.onTick(clock.now)
	// TurnDone → idle before grace expires.
	d.NoteIdle()
	if d.phaseForTest() != watchdogIdle {
		t.Fatalf("phase = %s, want idle", d.phaseForTest())
	}
	clock.now = clock.now.Add(tuiWatchdogCancelGrace + time.Second)
	d.onTick(clock.now)
	if d.killCalls.Load() != 0 {
		t.Fatalf("kill after TurnDone = %d, want 0", d.killCalls.Load())
	}
}

func TestWatchdogClosedStopsAllActions(t *testing.T) {
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	d.NoteBooted()
	d.NoteRunning(func() {})
	d.Close()
	if d.phaseForTest() != watchdogClosed {
		t.Fatalf("phase = %s, want closed", d.phaseForTest())
	}
	clock.now = clock.now.Add(tuiWatchdogStall + time.Second)
	d.onTick(clock.now)
	if d.dumpCalls.Load() != 0 || d.cancelCalls.Load() != 0 || d.killCalls.Load() != 0 {
		t.Fatalf("closed watchdog still acted: dump=%d cancel=%d kill=%d",
			d.dumpCalls.Load(), d.cancelCalls.Load(), d.killCalls.Load())
	}
}

func TestWatchdogStaleGenerationCannotKillNewTurn(t *testing.T) {
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	d.NoteBooted()
	d.NoteRunning(func() {}) // gen 1
	clock.now = clock.now.Add(tuiWatchdogStall)
	d.onTick(clock.now) // escalate gen 1
	if d.cancelCalls.Load() != 1 {
		t.Fatalf("cancelCalls = %d, want 1", d.cancelCalls.Load())
	}

	// New turn starts (generation bumps); old grace must not kill it.
	d.NoteIdle()
	d.NoteRunning(func() {}) // gen 2
	clock.now = clock.now.Add(tuiWatchdogCancelGrace + time.Second)
	// Heartbeat keeps gen 2 healthy.
	d.NoteActiveHeartbeat("elapsed_tick")
	d.onTick(clock.now)
	if d.killCalls.Load() != 0 {
		t.Fatalf("stale kill hit new generation: killCalls=%d", d.killCalls.Load())
	}
	if d.generationForTest() != 2 {
		t.Fatalf("generation = %d, want 2", d.generationForTest())
	}
}

func TestWatchdogUserActivityDoesNotCountAsActiveHeartbeat(t *testing.T) {
	// NoteBooted / NoteIdle paths are the only non-active transitions; keyboard
	// never calls NoteActiveHeartbeat. Prove that without it, a running stall
	// still escalates even if "time passes" via booted-style idle marks.
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	d.NoteBooted()
	d.NoteRunning(func() {})
	// Simulate only user-facing updates that do not call NoteActiveHeartbeat.
	clock.now = clock.now.Add(tuiWatchdogStall)
	d.onTick(clock.now)
	if d.dumpCalls.Load() != 1 {
		t.Fatalf("stall without active heartbeat dumpCalls = %d, want 1", d.dumpCalls.Load())
	}
}

func TestChatTUIWatchdogHelpersAreNilSafe(t *testing.T) {
	var m chatTUI
	m.noteWatchdogRunning()
	m.noteWatchdogIdle()
	m.noteWatchdogHeartbeat("elapsed_tick")
}

func TestChatTUIWatchdogLifecycleHelpers(t *testing.T) {
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	m := chatTUI{diagnostics: d}

	// Boot confirmation (first Update path).
	m.diagnostics.NoteBooted()
	if d.phaseForTest() != watchdogIdle {
		t.Fatalf("after NoteBooted phase = %s, want idle", d.phaseForTest())
	}

	// Shell / controller turn entry.
	m.noteWatchdogRunning()
	if d.phaseForTest() != watchdogRunning {
		t.Fatalf("phase = %s, want running", d.phaseForTest())
	}
	m.noteWatchdogHeartbeat("elapsed_tick")
	// TurnDone / shell completion.
	m.noteWatchdogIdle()
	if d.phaseForTest() != watchdogIdle {
		t.Fatalf("phase after idle = %s, want idle", d.phaseForTest())
	}
}

// TestWatchdogTickGapSkipsSuspendedProcess verifies the tick-gap heuristic that
// tells a real OS suspension (Android backgrounding, screen off) from a healthy
// 1s ticker: the watchdog's own ticker stalls for far longer than its interval
// while the process is frozen, so the heartbeat age read at resume is inflated
// by the pause and must not be read as a wedged event loop.
func TestWatchdogTickGapSkipsSuspendedProcess(t *testing.T) {
	now := time.Now()
	// A normal 1s ticker gap is never a suspension.
	if watchdogTickGap(now.Add(-tuiWatchdogInterval), now) {
		t.Fatal("normal 1s tick gap must not count as process suspension")
	}
	// A gap beyond suspendAllowance (e.g. a 38s background freeze) is.
	if !watchdogTickGap(now.Add(-38*time.Second), now) {
		t.Fatal("38s tick gap must count as process suspension")
	}
	// A gap at the boundary must not trigger (stall threshold is 10s, so a
	// suspended process is also a healthy margin away from a real stall).
	if watchdogTickGap(now.Add(-suspendAllowance), now) {
		t.Fatalf("tick gap of exactly %s must not count as suspension", suspendAllowance)
	}
	// First tick after startup has no previous tick: never a suspension.
	if watchdogTickGap(time.Time{}, now) {
		t.Fatal("zero previous tick must not count as process suspension")
	}
}

// TestWatchdogSuspendDoesNotKill is the regression test for the stall watchdog
// killing a healthy TUI after an OS suspension (Android lock/screen-off): the
// process freezes, the watchdog ticker goes silent, and the heartbeat age
// measured at resume is inflated by the whole pause. noteSuspended must reset
// the heartbeat to the resume moment so a healthy-but-asleep TUI is not killed.
func TestWatchdogSuspendDoesNotKill(t *testing.T) {
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	d.NoteBooted()
	d.NoteRunning(func() {})

	// Device sleeps for 5 minutes: no ticks, no heartbeats.
	clock.now = clock.now.Add(5 * time.Minute)
	// Resume: the first tick sees the huge gap and refreshes liveness.
	d.noteSuspended(clock.now, 5*time.Minute)

	// One healthy second after resume must not escalate.
	clock.now = clock.now.Add(time.Second)
	d.onTick(clock.now)
	if got := d.dumpCalls.Load(); got != 0 {
		t.Fatalf("after suspend refresh dumpCalls = %d, want 0", got)
	}
	if got := d.cancelCalls.Load(); got != 0 {
		t.Fatalf("after suspend refresh cancelCalls = %d, want 0", got)
	}
	if got := d.killCalls.Load(); got != 0 {
		t.Fatalf("after suspend refresh killCalls = %d, want 0", got)
	}
}

// TestWatchdogSuspendRefreshDoesNotSwallowRealStall guards the opposite
// direction: refreshing the heartbeat on resume must not mask a genuinely
// wedged event loop afterwards. A real stall after the refresh still escalates
// through dump, cancel, grace, and hard-kill.
func TestWatchdogSuspendRefreshDoesNotSwallowRealStall(t *testing.T) {
	clock := &fakeWatchClock{now: time.Unix(1_700_000_000, 0)}
	d := newWatchdogForTest(t, clock)
	d.NoteBooted()
	d.NoteRunning(func() {})

	// Resume from a suspension refreshes the reference point.
	d.noteSuspended(clock.now, 5*time.Minute)

	// The event loop then genuinely wedges: stall threshold passes with no
	// heartbeat, exactly as a frozen loop would.
	clock.now = clock.now.Add(tuiWatchdogStall)
	d.onTick(clock.now)
	if got := d.dumpCalls.Load(); got != 1 {
		t.Fatalf("stall after suspend dumpCalls = %d, want 1", got)
	}
	if got := d.cancelCalls.Load(); got != 1 {
		t.Fatalf("stall after suspend cancelCalls = %d, want 1", got)
	}
	if got := d.killCalls.Load(); got != 0 {
		t.Fatalf("killCalls before grace expiry = %d, want 0", got)
	}

	// Grace expires with no recovery: hard-kill fires.
	clock.now = clock.now.Add(tuiWatchdogCancelGrace + time.Second)
	d.onTick(clock.now)
	if got := d.killCalls.Load(); got != 1 {
		t.Fatalf("killCalls after grace expiry = %d, want 1", got)
	}
}
