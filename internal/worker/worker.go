// Package worker walks running sheets through their checklist.
//
// All progress lives in the database: the worker keeps no state of its own
// beyond which sheets it is busy with. Killing the process leaves a sheet
// running, and a new process continues from its first pending step with the
// same runId.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"listingboard/internal/board"
)

// MissingBrochure is the error a sheet fails with when its brochure file
// cannot be read. The README fixes the exact text.
const MissingBrochure = "Brochure is missing"

// DefaultPause is the wait between steps when STEP_PAUSE is not set.
const DefaultPause = 2 * time.Second

// Store is the persistence the worker needs.
type Store interface {
	RunningJobs(ctx context.Context) ([]board.Job, error)
	// SaveProgress writes to's status, error and checklist only if the
	// sheet still matches from (running, same runId, same checklist), and
	// returns board.ErrConflict otherwise.
	SaveProgress(ctx context.Context, from, to board.Job, at time.Time) error
}

type Worker struct {
	Store   Store
	DataDir string
	// Pause is the wait after a step is saved, before the next one.
	Pause time.Duration
	// Poll is how often the database is checked for running sheets.
	Poll time.Duration

	// sleep is time.Sleep that stops early when ctx ends. Tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
}

// ParsePause reads STEP_PAUSE: a Go duration such as 2s or 500ms. Empty
// means DefaultPause.
func ParsePause(value string) (time.Duration, error) {
	if value == "" {
		return DefaultPause, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("STEP_PAUSE: %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf("STEP_PAUSE must not be negative, got %s", value)
	}
	return d, nil
}

// Run polls for running sheets and works on each in its own goroutine until
// ctx ends. It returns once every goroutine has stopped.
func (w *Worker) Run(ctx context.Context) error {
	var (
		mu     sync.Mutex
		active = map[string]bool{}
		wg     sync.WaitGroup
	)
	defer wg.Wait()

	poll := w.Poll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	for {
		jobs, err := w.Store.RunningJobs(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("worker: list running sheets: %v", err)
		}
		for _, job := range jobs {
			mu.Lock()
			busy := active[job.ID]
			active[job.ID] = true
			mu.Unlock()
			if busy {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(active, job.ID)
					mu.Unlock()
				}()
				w.Process(ctx, job)
			}()
		}
		if err := w.wait(ctx, poll); err != nil {
			return nil
		}
	}
}

// Process takes one running sheet to success or failed, one step at a time,
// pausing after each saved step. It returns early when ctx ends, when the
// sheet changes under it, or on a database error; the sheet is then still
// running and the next poll picks it up again from the saved state.
func (w *Worker) Process(ctx context.Context, job board.Job) {
	for job.Status == board.StatusRunning {
		next, err := w.advance(ctx, job)
		if err != nil {
			if !errors.Is(err, board.ErrConflict) && ctx.Err() == nil {
				log.Printf("worker: sheet %s: %v", job.ID, err)
			}
			return
		}
		stepped := pendingCount(next) < pendingCount(job)
		job = next
		if stepped && job.Status == board.StatusRunning && pendingCount(job) > 0 {
			if err := w.wait(ctx, w.Pause); err != nil {
				return
			}
		}
	}
}

// advance makes one change to the sheet and saves it:
//
//   - brochure unreadable → failed with MissingBrochure
//   - a step pending      → the first pending step done
//   - nothing pending     → success
//
// A crash between two saves is safe: the next call starts from whatever
// was saved, so a done step is never done again.
func (w *Worker) advance(ctx context.Context, job board.Job) (board.Job, error) {
	next := job
	next.Checklist = append([]board.Step(nil), job.Checklist...)

	i := firstPending(job)
	switch {
	case i >= 0 && !w.brochureReadable(job):
		next.Status = board.StatusFailed
		next.Error = MissingBrochure
	case i >= 0:
		next.Checklist[i].State = board.StepDone
	default:
		next.Status = board.StatusSuccess
	}

	// Save even if ctx was cancelled mid-step, so finished work is kept.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := w.Store.SaveProgress(saveCtx, job, next, w.clock()); err != nil {
		return job, err
	}
	return next, nil
}

func (w *Worker) brochureReadable(job board.Job) bool {
	f, err := os.Open(board.BrochurePath(w.DataDir, job.UserID, job.ID))
	if err != nil {
		return false
	}
	defer f.Close()
	// Opening is not enough: a directory, or a file the process may not
	// read, opens but cannot be read.
	_, err = f.Read(make([]byte, 1))
	return err == nil || errors.Is(err, io.EOF)
}

func (w *Worker) wait(ctx context.Context, d time.Duration) error {
	if w.sleep != nil {
		return w.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (w *Worker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now().UTC()
}

func firstPending(job board.Job) int {
	for i, step := range job.Checklist {
		if step.State == board.StepPending {
			return i
		}
	}
	return -1
}

func pendingCount(job board.Job) int {
	n := 0
	for _, step := range job.Checklist {
		if step.State == board.StepPending {
			n++
		}
	}
	return n
}
