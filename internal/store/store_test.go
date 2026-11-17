package store

// These tests run the real SQL against Postgres. They are skipped unless
// TEST_DATABASE_URL points at a server where the user may create databases,
// for example with the compose stack running:
//
//	TEST_DATABASE_URL='postgres://board:board@localhost:54329/board?sslmode=disable' go test ./internal/store/
//
// Each test gets a fresh database that is dropped afterwards.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"listingboard/internal/board"
)

func TestJobIsScopedToItsOwner(t *testing.T) {
	st, ctx := openTestStore(t)
	ada, grace := user(t, st, "ada"), user(t, st, "grace")
	job := sheet(t, st, ada)

	if _, err := st.Job(ctx, ada.ID, job.ID); err != nil {
		t.Fatalf("owner: %v", err)
	}
	if _, err := st.Job(ctx, grace.ID, job.ID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("other account: %v, want ErrNotFound", err)
	}
	if _, err := st.AttachFile(ctx, grace.ID, job.ID, "x.pdf", time.Now()); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("other account attach: %v, want ErrNotFound", err)
	}
	if _, err := st.StartJob(ctx, grace.ID, job.ID, board.NewID(), time.Now()); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("other account start: %v, want ErrNotFound", err)
	}
}

func TestAttachFileOnlyOnce(t *testing.T) {
	st, ctx := openTestStore(t)
	ada := user(t, st, "ada")
	job := sheet(t, st, ada)

	got, err := st.AttachFile(ctx, ada.ID, job.ID, "../../escaped.txt", time.Now())
	if err != nil || got.Filename != "../../escaped.txt" {
		t.Fatalf("first attach: %q, %v", got.Filename, err)
	}
	if _, err := st.AttachFile(ctx, ada.ID, job.ID, "second.pdf", time.Now()); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("second attach: %v, want ErrConflict", err)
	}
}

func TestStartJob(t *testing.T) {
	st, ctx := openTestStore(t)
	ada := user(t, st, "ada")

	bare := sheet(t, st, ada)
	if _, err := st.StartJob(ctx, ada.ID, bare.ID, board.NewID(), time.Now()); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("no brochure: %v, want ErrConflict", err)
	}

	first := attached(t, st, ada)
	runID := board.NewID()
	got, err := st.StartJob(ctx, ada.ID, first.ID, runID, time.Now())
	if err != nil || got.Status != board.StatusRunning || got.RunID != runID {
		t.Fatalf("start: %+v, %v", got, err)
	}
	if _, err := st.StartJob(ctx, ada.ID, first.ID, board.NewID(), time.Now()); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("start again: %v, want ErrConflict (the handler answers from the stored sheet)", err)
	}

	second := attached(t, st, ada)
	if _, err := st.StartJob(ctx, ada.ID, second.ID, board.NewID(), time.Now()); !errors.Is(err, board.ErrBusy) {
		t.Fatalf("second running sheet: %v, want ErrBusy", err)
	}

	// The limit is per account.
	grace := user(t, st, "grace")
	if _, err := st.StartJob(ctx, grace.ID, attached(t, st, grace).ID, board.NewID(), time.Now()); err != nil {
		t.Fatalf("other account: %v", err)
	}
}

// The unique index, not the code path, is what stops racing starts.
func TestConcurrentStartsRunOneSheet(t *testing.T) {
	st, ctx := openTestStore(t)
	ada := user(t, st, "ada")
	var ids []string
	for range 4 {
		ids = append(ids, attached(t, st, ada).ID)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	started, busy := 0, 0
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.StartJob(ctx, ada.ID, ids[i%len(ids)], board.NewID(), time.Now())
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				started++
			case errors.Is(err, board.ErrBusy), errors.Is(err, board.ErrConflict):
				busy++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	running, err := st.RunningJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if started != 1 || len(running) != 1 {
		t.Fatalf("%d starts succeeded and %d sheets are running, want 1 and 1", started, len(running))
	}
}

func TestSaveProgressIsConditional(t *testing.T) {
	st, ctx := openTestStore(t)
	ada := user(t, st, "ada")
	job := attached(t, st, ada)
	job, err := st.StartJob(ctx, ada.ID, job.ID, board.NewID(), time.Now())
	if err != nil {
		t.Fatal(err)
	}

	step := withStep(job, 0)
	if err := st.SaveProgress(ctx, job, step, time.Now()); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// The same step saved from the stale copy: the checklist no longer matches.
	if err := st.SaveProgress(ctx, job, step, time.Now()); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("stale save: %v, want ErrConflict", err)
	}
	wrongRun := step
	wrongRun.RunID = board.NewID()
	if err := st.SaveProgress(ctx, wrongRun, withStep(step, 1), time.Now()); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("other runId: %v, want ErrConflict", err)
	}

	failed := step
	failed.Status, failed.Error = board.StatusFailed, "Brochure is missing"
	if err := st.SaveProgress(ctx, step, failed, time.Now()); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if err := st.SaveProgress(ctx, failed, withStep(failed, 1), time.Now()); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("save on a failed sheet: %v, want ErrConflict", err)
	}
	got, err := st.Job(ctx, ada.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != board.StatusFailed || got.Error != "Brochure is missing" || got.Checklist[0].State != board.StepDone || got.Checklist[1].State != board.StepPending {
		t.Fatalf("stored: %+v", got)
	}
	if running, _ := st.RunningJobs(ctx); len(running) != 0 {
		t.Fatalf("%d running sheets, want 0", len(running))
	}
}

// The API and the worker migrate at the same time on startup.
func TestConcurrentMigrations(t *testing.T) {
	dsn := testDatabase(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := Open(dsn)
			if err == nil {
				err = st.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Open: %v", err)
		}
	}
}

// --- helpers ---

func openTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	st, err := Open(testDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, context.Background()
}

// testDatabase creates an empty database for one test and drops it after.
func testDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := "board_test_" + board.NewID()[:12]
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func user(t *testing.T, st *Store, name string) board.User {
	t.Helper()
	u, err := st.CreateUser(context.Background(), fmt.Sprintf("%s-%s@example.com", name, board.NewID()[:6]), "hash")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func sheet(t *testing.T, st *Store, owner board.User) board.Job {
	t.Helper()
	now := time.Now().UTC()
	job := board.Job{
		ID: board.NewID(), UserID: owner.ID, Title: "Market loft", Address: "10 Market Street",
		Status: board.StatusDraft, Checklist: board.NewChecklist(), CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

func attached(t *testing.T, st *Store, owner board.User) board.Job {
	t.Helper()
	job, err := st.AttachFile(context.Background(), owner.ID, sheet(t, st, owner).ID, "brochure.pdf", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func withStep(job board.Job, i int) board.Job {
	job.Checklist = append([]board.Step(nil), job.Checklist...)
	job.Checklist[i].State = board.StepDone
	return job
}
