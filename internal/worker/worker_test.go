package worker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"listingboard/internal/board"
)

func TestWorkerRunsTheChecklistInOrderThenSucceeds(t *testing.T) {
	w, st := newTestWorker(t)
	job := st.add(t, runningSheet(t, w.DataDir, "u1"))

	w.Process(context.Background(), job)

	// One save per step, then success. The pause comes after each saved
	// step that has another after it, never before the first or after the last.
	st.assertSaves(t,
		saved{board.StatusRunning, "", "done pending pending"},
		saved{board.StatusRunning, "", "done done pending"},
		saved{board.StatusRunning, "", "done done done"},
		saved{board.StatusSuccess, "", "done done done"},
	)
	if got := st.sheet(job.ID).RunID; got != job.RunID {
		t.Fatalf("runId changed from %q to %q", job.RunID, got)
	}
	assertSleeps(t, w, w.Pause, w.Pause)
}

// A new worker process after a kill: it continues from the first pending
// step, keeps the runId, and never redoes a step that was saved.
func TestWorkerResumesFromTheFirstPendingStep(t *testing.T) {
	w, st := newTestWorker(t)
	job := runningSheet(t, w.DataDir, "u1")
	job.Checklist[0].State = board.StepDone
	job = st.add(t, job)

	w.Process(context.Background(), job)

	st.assertSaves(t,
		saved{board.StatusRunning, "", "done done pending"},
		saved{board.StatusRunning, "", "done done done"},
		saved{board.StatusSuccess, "", "done done done"},
	)
	if got := st.sheet(job.ID).RunID; got != job.RunID {
		t.Fatalf("runId changed from %q to %q", job.RunID, got)
	}
}

// Killed after the third step was saved but before success: the restart
// only marks it success.
func TestWorkerFinishesASheetWithEveryStepDone(t *testing.T) {
	w, st := newTestWorker(t)
	job := runningSheet(t, w.DataDir, "u1")
	for i := range job.Checklist {
		job.Checklist[i].State = board.StepDone
	}
	job = st.add(t, job)

	w.Process(context.Background(), job)

	st.assertSaves(t, saved{board.StatusSuccess, "", "done done done"})
	assertSleeps(t, w)
}

// Cancelling mid-run (SIGTERM) leaves the sheet running with the steps done
// so far, and a second worker finishes it with the same runId.
func TestWorkerStoppedMidRunLeavesTheSheetRunning(t *testing.T) {
	w, st := newTestWorker(t)
	job := st.add(t, runningSheet(t, w.DataDir, "u1"))

	ctx, cancel := context.WithCancel(context.Background())
	w.sleep = func(context.Context, time.Duration) error {
		cancel() // stop during the first pause
		return context.Canceled
	}
	w.Process(ctx, job)

	got := st.sheet(job.ID)
	if got.Status != board.StatusRunning || states(got) != "done pending pending" {
		t.Fatalf("after stop: %s %q, want running with the first step done", got.Status, states(got))
	}

	next, _ := newTestWorker(t)
	next.DataDir, next.Store = w.DataDir, st
	next.Process(context.Background(), got)
	final := st.sheet(job.ID)
	if final.Status != board.StatusSuccess || final.RunID != job.RunID {
		t.Fatalf("after restart: %s with runId %q, want success with %q", final.Status, final.RunID, job.RunID)
	}
}

func TestWorkerFailsWhenTheBrochureIsMissing(t *testing.T) {
	w, st := newTestWorker(t)
	job := runningSheet(t, w.DataDir, "u1")
	if err := os.Remove(board.BrochurePath(w.DataDir, job.UserID, job.ID)); err != nil {
		t.Fatal(err)
	}
	job = st.add(t, job)

	w.Process(context.Background(), job)

	st.assertSaves(t, saved{board.StatusFailed, MissingBrochure, "pending pending pending"})
	if got := st.sheet(job.ID); got.Error != "Brochure is missing" {
		t.Fatalf("error %q, want exactly %q", got.Error, "Brochure is missing")
	}
}

// The file vanishing between steps fails the sheet at the next step and
// keeps the steps already done.
func TestWorkerFailsWhenTheBrochureDisappearsMidRun(t *testing.T) {
	w, st := newTestWorker(t)
	job := st.add(t, runningSheet(t, w.DataDir, "u1"))
	w.sleep = func(context.Context, time.Duration) error {
		return os.Remove(board.BrochurePath(w.DataDir, job.UserID, job.ID))
	}

	w.Process(context.Background(), job)

	st.assertSaves(t,
		saved{board.StatusRunning, "", "done pending pending"},
		saved{board.StatusFailed, MissingBrochure, "done pending pending"},
	)
}

// A brochure path that exists but can't be read as a file also counts as missing.
func TestWorkerFailsWhenTheBrochureIsNotAFile(t *testing.T) {
	w, st := newTestWorker(t)
	job := runningSheet(t, w.DataDir, "u1")
	path := board.BrochurePath(w.DataDir, job.UserID, job.ID)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	job = st.add(t, job)

	w.Process(context.Background(), job)

	st.assertSaves(t, saved{board.StatusFailed, MissingBrochure, "pending pending pending"})
}

// If someone else saved the sheet first (another worker, say), the worker
// stops instead of writing over it.
func TestWorkerStopsWhenTheSheetChangedUnderIt(t *testing.T) {
	w, st := newTestWorker(t)
	job := st.add(t, runningSheet(t, w.DataDir, "u1"))
	stale := job
	stale.Checklist = append([]board.Step(nil), job.Checklist...)
	st.mu.Lock()
	st.jobs[job.ID].Checklist[0].State = board.StepDone // done elsewhere
	st.mu.Unlock()

	w.Process(context.Background(), stale)

	st.assertSaves(t)
}

func TestRunWorksOnEveryRunningSheetAndStopsOnCancel(t *testing.T) {
	w, st := newTestWorker(t)
	w.sleep = nil // real timers, short ones
	w.Pause = time.Millisecond
	w.Poll = time.Millisecond
	a := st.add(t, runningSheet(t, w.DataDir, "u1"))
	b := st.add(t, runningSheet(t, w.DataDir, "u2"))
	draft := runningSheet(t, w.DataDir, "u3")
	draft.Status = board.StatusDraft
	draft = st.add(t, draft)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for st.sheet(a.ID).Status != board.StatusSuccess || st.sheet(b.ID).Status != board.StatusSuccess {
		if time.Now().After(deadline) {
			t.Fatalf("sheets did not finish: %s, %s", st.sheet(a.ID).Status, st.sheet(b.ID).Status)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if got := st.sheet(draft.ID); got.Status != board.StatusDraft || states(got) != "pending pending pending" {
		t.Fatalf("draft was touched: %s %q", got.Status, states(got))
	}
	// Each sheet got exactly its four saves, however many polls saw it.
	if n := st.saveCount(a.ID); n != 4 {
		t.Fatalf("sheet a saved %d times, want 4", n)
	}
	if n := st.saveCount(b.ID); n != 4 {
		t.Fatalf("sheet b saved %d times, want 4", n)
	}
}

func TestParsePause(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 2 * time.Second, false},
		{"2s", 2 * time.Second, false},
		{"150ms", 150 * time.Millisecond, false},
		{"0s", 0, false},
		{"2", 0, true},
		{"soon", 0, true},
		{"-1s", 0, true},
	}
	for _, tc := range cases {
		got, err := ParsePause(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("ParsePause(%q) = %s, %v; want %s, error %v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

// --- helpers ---

type saved struct {
	status, err, states string
}

// testWorker is a Worker whose pauses are recorded instead of waited out.
type testWorker struct {
	*Worker
	slept []time.Duration
}

func newTestWorker(t *testing.T) (*testWorker, *fakeStore) {
	t.Helper()
	st := &fakeStore{jobs: map[string]*board.Job{}}
	w := &testWorker{Worker: &Worker{Store: st, DataDir: t.TempDir(), Pause: 2 * time.Second}}
	w.sleep = func(_ context.Context, d time.Duration) error {
		w.slept = append(w.slept, d)
		return nil
	}
	return w, st
}

// runningSheet returns a started sheet whose brochure exists on disk.
func runningSheet(t *testing.T, dataDir, userID string) board.Job {
	t.Helper()
	job := board.Job{
		ID:        board.NewID(),
		UserID:    userID,
		Title:     "Market loft",
		Address:   "10 Market Street",
		Status:    board.StatusRunning,
		RunID:     board.NewID(),
		Filename:  "brochure.pdf",
		Checklist: board.NewChecklist(),
	}
	path := board.BrochurePath(dataDir, userID, job.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("%PDF-1.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return job
}

func assertSleeps(t *testing.T, w *testWorker, want ...time.Duration) {
	t.Helper()
	if !slices.Equal(w.slept, want) {
		t.Fatalf("paused %v, want %v", w.slept, want)
	}
}

func states(job board.Job) string {
	var s []string
	for _, step := range job.Checklist {
		s = append(s, step.State)
	}
	return strings.Join(s, " ")
}

// fakeStore keeps sheets in memory with the same conditional-write rule as
// the Postgres store, and records every successful save.
type fakeStore struct {
	mu    sync.Mutex
	jobs  map[string]*board.Job
	saves []struct {
		id string
		saved
	}
}

func (f *fakeStore) add(t *testing.T, job board.Job) board.Job {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := clone(job)
	f.jobs[job.ID] = &stored
	return clone(job)
}

func (f *fakeStore) sheet(id string) board.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return clone(*f.jobs[id])
}

func (f *fakeStore) saveCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.saves {
		if s.id == id {
			n++
		}
	}
	return n
}

func (f *fakeStore) assertSaves(t *testing.T, want ...saved) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var got []saved
	for _, s := range f.saves {
		got = append(got, s.saved)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("saves:\n  got  %v\n  want %v", got, want)
	}
}

func (f *fakeStore) RunningJobs(context.Context) ([]board.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []board.Job
	for _, job := range f.jobs {
		if job.Status == board.StatusRunning {
			out = append(out, clone(*job))
		}
	}
	return out, nil
}

func (f *fakeStore) SaveProgress(_ context.Context, from, to board.Job, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.jobs[from.ID]
	if !ok || cur.Status != board.StatusRunning || cur.RunID != from.RunID || states(*cur) != states(from) {
		return board.ErrConflict
	}
	cur.Status, cur.Error = to.Status, to.Error
	cur.Checklist = append([]board.Step(nil), to.Checklist...)
	f.saves = append(f.saves, struct {
		id string
		saved
	}{from.ID, saved{to.Status, to.Error, states(to)}})
	return nil
}

func clone(job board.Job) board.Job {
	job.Checklist = append([]board.Step(nil), job.Checklist...)
	return job
}
