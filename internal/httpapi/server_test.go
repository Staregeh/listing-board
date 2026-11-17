package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"listingboard/internal/board"
)

// POST /start only hands the sheet to the worker: it becomes running with a
// fresh runId and every step still pending. Success and failure are the
// worker's to set, never the handler's.
func TestStartMarksTheJobSuccessful(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")

	res := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	got := decodeJob(t, res)
	if got.Status != board.StatusRunning {
		t.Fatalf("status %s, want running", got.Status)
	}
	if got.RunID == "" {
		t.Fatal("runId is empty")
	}
	if got.Error != "" {
		t.Fatalf("error %q, want empty", got.Error)
	}
	assertChecklist(t, got, board.StepPending, board.StepPending, board.StepPending)

	stored := getSheet(t, srv, cookie, job.ID)
	if stored.Status != board.StatusRunning || stored.RunID != got.RunID {
		t.Fatalf("stored sheet is %s with runId %q, response said running with %q", stored.Status, stored.RunID, got.RunID)
	}
	assertChecklist(t, stored, board.StepPending, board.StepPending, board.StepPending)
}

func TestStartAgainWhileRunningKeepsTheRunID(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := readySheet(t, srv, cookie, "Market loft")
	first := start(t, srv, cookie, job.ID)

	// Progress made by the worker must survive a repeated start too.
	memOf(srv).edit(t, job.ID, func(j *board.Job) { j.Checklist[0].State = board.StepDone })

	res := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", res.Code, res.Body.String())
	}
	again := decodeJob(t, res)
	if again.Status != board.StatusRunning || again.RunID != first.RunID {
		t.Fatalf("second start: %s with runId %q, want running with %q", again.Status, again.RunID, first.RunID)
	}
	assertChecklist(t, again, board.StepDone, board.StepPending, board.StepPending)
}

func TestStartWithoutABrochure(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

	res := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	assertError(t, res, http.StatusBadRequest, "Attach a brochure first")
	if got := getSheet(t, srv, cookie, job.ID); got.Status != board.StatusDraft || got.RunID != "" {
		t.Fatalf("sheet is %s with runId %q, want an untouched draft", got.Status, got.RunID)
	}
}

func TestStartRefusesASecondRunningSheet(t *testing.T) {
	srv, _ := newTestServer(t)
	ada := signUp(t, srv, "ada@example.com")
	running := readySheet(t, srv, ada, "Market loft")
	start(t, srv, ada, running.ID)
	next := readySheet(t, srv, ada, "Harbour flat")

	res := call(srv, http.MethodPost, "/api/jobs/"+next.ID+"/start", ada, nil, "")
	if res.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", res.Code, res.Body.String())
	}
	assertJSONError(t, res)
	if got := getSheet(t, srv, ada, next.ID); got.Status != board.StatusDraft || got.RunID != "" {
		t.Fatalf("refused sheet is %s with runId %q, want an untouched draft", got.Status, got.RunID)
	}

	// The limit is per account: someone else can still start theirs.
	grace := signUp(t, srv, "grace@example.com")
	theirs := readySheet(t, srv, grace, "Garden house")
	start(t, srv, grace, theirs.ID)

	// Once the running sheet finishes, the next one can start.
	memOf(srv).edit(t, running.ID, func(j *board.Job) { j.Status = board.StatusSuccess })
	start(t, srv, ada, next.ID)
}

// A draft missing its brochure gets 400 even while another sheet runs: what
// is wrong with this sheet is the more useful answer than the conflict.
func TestStartWithoutABrochureWhileAnotherRuns(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	start(t, srv, cookie, readySheet(t, srv, cookie, "Market loft").ID)
	bare := createSheet(t, srv, cookie, "Harbour flat", "2 Harbour Road")

	res := call(srv, http.MethodPost, "/api/jobs/"+bare.ID+"/start", cookie, nil, "")
	assertError(t, res, http.StatusBadRequest, "Attach a brochure first")
}

func TestFinishedSheetsStayFinished(t *testing.T) {
	for _, status := range []string{board.StatusFailed, board.StatusSuccess} {
		t.Run(status, func(t *testing.T) {
			srv, _ := newTestServer(t)
			cookie := signUp(t, srv, "ada@example.com")
			job := readySheet(t, srv, cookie, "Market loft")
			first := start(t, srv, cookie, job.ID)
			memOf(srv).edit(t, job.ID, func(j *board.Job) {
				j.Status = status
				if status == board.StatusFailed {
					j.Error = "Brochure is missing"
				}
			})

			res := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
			if res.Code != http.StatusConflict {
				t.Fatalf("status %d, want 409: %s", res.Code, res.Body.String())
			}
			assertJSONError(t, res)
			got := getSheet(t, srv, cookie, job.ID)
			if got.Status != status || got.RunID != first.RunID {
				t.Fatalf("sheet is %s with runId %q, want it still %s with %q", got.Status, got.RunID, status, first.RunID)
			}
		})
	}
}

// Many starts of the same sheet at once: all succeed and agree on one runId.
func TestConcurrentStartsShareOneRunID(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := readySheet(t, srv, cookie, "Market loft")

	results := parallel(16, func(int) *httptest.ResponseRecorder {
		return call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	})
	runIDs := map[string]bool{}
	for _, res := range results {
		if res.Code != http.StatusOK {
			t.Fatalf("status %d, want 200: %s", res.Code, res.Body.String())
		}
		runIDs[decodeJob(t, res).RunID] = true
	}
	if len(runIDs) != 1 {
		t.Fatalf("got %d different runIds, want 1: %v", len(runIDs), runIDs)
	}
}

// Starts of two different sheets at once: exactly one ends up running.
func TestConcurrentStartsOfTwoSheetsRunOnlyOne(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	ids := []string{readySheet(t, srv, cookie, "Market loft").ID, readySheet(t, srv, cookie, "Harbour flat").ID}

	parallel(16, func(i int) *httptest.ResponseRecorder {
		return call(srv, http.MethodPost, "/api/jobs/"+ids[i%2]+"/start", cookie, nil, "")
	})
	running := 0
	for _, id := range ids {
		if getSheet(t, srv, cookie, id).Status == board.StatusRunning {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("%d sheets running, want exactly 1", running)
	}
}

// The client's filename is kept only in the sheet's filename field. The bytes
// always land at {DATA_DIR}/{userID}/{jobID}/body, and nothing else is written.
func TestUploadKeepsTheClientFilename(t *testing.T) {
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	userID := accountID(t, srv, cookie)
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	pdf := []byte("%PDF-1.4\n")

	res := upload(t, srv, cookie, job.ID, "brochure.pdf", pdf, "application/pdf")
	if res.Code != http.StatusOK {
		t.Fatalf("upload %d: %s", res.Code, res.Body.String())
	}
	if got := decodeJob(t, res).Filename; got != "brochure.pdf" {
		t.Fatalf("filename %q, want brochure.pdf", got)
	}

	want := filepath.Join(dir, userID, job.ID, "body")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("brochure was not stored at %s: %v", want, err)
	}
	if !bytes.Equal(got, pdf) {
		t.Fatalf("stored bytes %q, want %q", got, pdf)
	}
	assertFiles(t, filepath.Dir(dir), want)
}

func TestUploadCannotEscapeTheSheetDirectory(t *testing.T) {
	for _, name := range []string{
		"../../escaped.txt",
		"../../../../../../tmp/escaped.txt",
		"/etc/escaped.txt",
		`..\..\escaped.txt`,
		"body",
	} {
		t.Run(name, func(t *testing.T) {
			srv, dir := newTestServer(t)
			cookie := signUp(t, srv, "ada@example.com")
			userID := accountID(t, srv, cookie)
			job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

			res := upload(t, srv, cookie, job.ID, name, []byte("Two bedrooms"), "text/plain")
			if res.Code != http.StatusOK {
				t.Fatalf("upload %d: %s", res.Code, res.Body.String())
			}
			if got := decodeJob(t, res).Filename; got != name {
				t.Fatalf("filename %q, want the original %q", got, name)
			}
			assertFiles(t, filepath.Dir(dir), filepath.Join(dir, userID, job.ID, "body"))
		})
	}
}

func TestUploadSizeLimit(t *testing.T) {
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	userID := accountID(t, srv, cookie)

	atLimit := createSheet(t, srv, cookie, "At the limit", "1 Market Street")
	res := upload(t, srv, cookie, atLimit.ID, "big.txt", bytes.Repeat([]byte("a"), board.MaxBrochureSize), "text/plain")
	if res.Code != http.StatusOK {
		t.Fatalf("exactly 1 MiB: status %d, want 200: %s", res.Code, res.Body.String())
	}

	for _, size := range []int{board.MaxBrochureSize + 1, 3 * board.MaxBrochureSize} {
		over := createSheet(t, srv, cookie, "Over the limit", "2 Market Street")
		res := upload(t, srv, cookie, over.ID, "big.txt", bytes.Repeat([]byte("a"), size), "text/plain")
		if res.Code != http.StatusBadRequest {
			t.Fatalf("%d bytes: status %d, want 400: %s", size, res.Code, res.Body.String())
		}
		assertJSONError(t, res)
		if got := getSheet(t, srv, cookie, over.ID).Filename; got != "" {
			t.Fatalf("%d bytes: rejected upload set filename %q", size, got)
		}
	}
	// Only the accepted file is on disk: no partial or temporary files.
	assertFiles(t, filepath.Dir(dir), filepath.Join(dir, userID, atLimit.ID, "body"))
}

// The type comes from the bytes. The multipart Content-Type and the
// extension are deliberately chosen to lie in every case.
func TestUploadDecidesTheTypeFromTheBytes(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		ctype    string
		data     []byte
		wantOK   bool
	}{
		{"pdf named as text", "notes.txt", "text/plain", []byte("%PDF-1.7\n\x00\xff binary"), true},
		{"text named as pdf", "brochure.pdf", "application/pdf", []byte("Two bedrooms, south facing"), true},
		{"utf-8 text with no extension", "brochure", "application/octet-stream", []byte("Квартира біля ринку"), true},
		{"empty file", "brochure.pdf", "application/pdf", nil, false},
		{"nul byte", "brochure.txt", "text/plain", []byte("Two\x00bedrooms"), false},
		{"invalid utf-8", "brochure.txt", "text/plain", []byte("caf\xe9"), false},
		{"png named as pdf", "brochure.pdf", "application/pdf", []byte("\x89PNG\r\n\x1a\n\x00\x00"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, dir := newTestServer(t)
			cookie := signUp(t, srv, "ada@example.com")
			job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

			res := upload(t, srv, cookie, job.ID, tc.filename, tc.data, tc.ctype)
			if tc.wantOK {
				if res.Code != http.StatusOK {
					t.Fatalf("status %d, want 200: %s", res.Code, res.Body.String())
				}
				return
			}
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", res.Code, res.Body.String())
			}
			assertJSONError(t, res)
			assertFiles(t, filepath.Dir(dir))
		})
	}
}

func TestSecondUploadIsRejected(t *testing.T) {
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	userID := accountID(t, srv, cookie)
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	first := []byte("%PDF-1.4\nfirst")
	uploadFile(t, srv, cookie, job.ID, "first.pdf", first, "application/pdf")

	res := upload(t, srv, cookie, job.ID, "second.txt", []byte("second"), "text/plain")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.Code, res.Body.String())
	}
	assertJSONError(t, res)

	if got := getSheet(t, srv, cookie, job.ID).Filename; got != "first.pdf" {
		t.Fatalf("filename %q, want first.pdf", got)
	}
	path := filepath.Join(dir, userID, job.ID, "body")
	if got, _ := os.ReadFile(path); !bytes.Equal(got, first) {
		t.Fatalf("stored bytes %q, want the first upload", got)
	}
	assertFiles(t, filepath.Dir(dir), path)
}

// Two uploads racing for the same sheet: exactly one wins, and the stored
// bytes belong to the winner.
func TestConcurrentUploadsOnlyOneWins(t *testing.T) {
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	userID := accountID(t, srv, cookie)
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

	results := parallel(8, func(i int) *httptest.ResponseRecorder {
		return upload(t, srv, cookie, job.ID, fmt.Sprintf("b%d.txt", i), []byte(fmt.Sprintf("brochure %d", i)), "text/plain")
	})
	winners := 0
	for _, res := range results {
		switch res.Code {
		case http.StatusOK:
			winners++
		case http.StatusBadRequest:
		default:
			t.Fatalf("unexpected status %d: %s", res.Code, res.Body.String())
		}
	}
	if winners != 1 {
		t.Fatalf("%d uploads won, want exactly 1", winners)
	}
	name := getSheet(t, srv, cookie, job.ID).Filename
	var i int
	if _, err := fmt.Sscanf(name, "b%d.txt", &i); err != nil {
		t.Fatalf("filename %q: %v", name, err)
	}
	path := filepath.Join(dir, userID, job.ID, "body")
	if got, _ := os.ReadFile(path); string(got) != fmt.Sprintf("brochure %d", i) {
		t.Fatalf("stored %q, but the sheet says %s won", got, name)
	}
	assertFiles(t, filepath.Dir(dir), path)
}

func TestUploadNeedsAFilePart(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

	res := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/file", cookie, strings.NewReader(`{}`), "application/json")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("not multipart: status %d, want 400", res.Code)
	}
	assertJSONError(t, res)

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("brochure", "wrong field name")
	_ = writer.Close()
	res = call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/file", cookie, &buf, writer.FormDataContentType())
	if res.Code != http.StatusBadRequest {
		t.Fatalf("no file part: status %d, want 400", res.Code)
	}
	assertJSONError(t, res)
}

func TestDownloadReturnsTheBrochure(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

	res := call(srv, http.MethodGet, "/api/jobs/"+job.ID+"/file", cookie, nil, "")
	if res.Code != http.StatusNotFound {
		t.Fatalf("before upload: status %d, want 404", res.Code)
	}

	text := []byte("Two bedrooms <script>alert(1)</script>")
	// Quotes arrive backslash-escaped inside the quoted filename parameter.
	uploadFile(t, srv, cookie, job.ID, `../my \"loft\".pdf`, text, "application/pdf")
	res = call(srv, http.MethodGet, "/api/jobs/"+job.ID+"/file", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if !bytes.Equal(res.Body.Bytes(), text) {
		t.Fatalf("body %q, want %q", res.Body.Bytes(), text)
	}
	if got := res.Header().Get("Content-Type"); got != board.ContentTypeText {
		t.Fatalf("Content-Type %q, want %q: decided from the bytes", got, board.ContentTypeText)
	}
	if got := res.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options %q, want nosniff", got)
	}
	_, params, err := mime.ParseMediaType(res.Header().Get("Content-Disposition"))
	if err != nil || params["filename"] != `my "loft".pdf` {
		t.Fatalf("Content-Disposition %q: filename %q, err %v", res.Header().Get("Content-Disposition"), params["filename"], err)
	}
}

func TestListHidesOtherUsersJobs(t *testing.T) {
	srv, _ := newTestServer(t)
	ada := signUp(t, srv, "ada@example.com")
	grace := signUp(t, srv, "grace@example.com")
	job := createSheet(t, srv, ada, "Market loft", "10 Market Street")

	res := call(srv, http.MethodGet, "/api/jobs", grace, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	var jobs []board.Job
	if err := json.Unmarshal(res.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	for _, item := range jobs {
		if item.ID == job.ID {
			t.Fatal("another account can see this sheet in the list")
		}
	}
}

// Every route that takes an id answers 404 for someone else's sheet, the
// same as for an unknown one, so ids can't be probed. Nothing the other
// account sends may change the sheet either.
func TestSheetRoutesHideOtherUsersSheets(t *testing.T) {
	srv, _ := newTestServer(t)
	ada := signUp(t, srv, "ada@example.com")
	grace := signUp(t, srv, "grace@example.com")
	job := createSheet(t, srv, ada, "Market loft", "10 Market Street")
	uploadFile(t, srv, ada, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")

	for _, id := range []string{job.ID, board.NewID()} {
		upload, contentType := multipartBody(t, "other.txt", []byte("not yours"), "text/plain")
		routes := []struct {
			method, path string
			body         io.Reader
			contentType  string
		}{
			{http.MethodGet, "/api/jobs/" + id, nil, ""},
			{http.MethodPost, "/api/jobs/" + id + "/file", upload, contentType},
			{http.MethodPost, "/api/jobs/" + id + "/start", nil, ""},
			{http.MethodGet, "/api/jobs/" + id + "/file", nil, ""},
		}
		for _, route := range routes {
			res := call(srv, route.method, route.path, grace, route.body, route.contentType)
			if res.Code != http.StatusNotFound {
				t.Errorf("%s %s as another account: status %d, want 404: %s", route.method, route.path, res.Code, res.Body.String())
			}
			assertJSONError(t, res)
		}
	}

	res := call(srv, http.MethodGet, "/api/jobs/"+job.ID, ada, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("owner read: status %d: %s", res.Code, res.Body.String())
	}
	got := decodeJob(t, res)
	if got.Status != "draft" || got.RunID != "" || got.Filename != "brochure.pdf" {
		t.Fatalf("another account changed the sheet: status %q, runId %q, filename %q", got.Status, got.RunID, got.Filename)
	}
}

func TestStartRequiresASession(t *testing.T) {
	srv, _ := newTestServer(t)
	res := call(srv, http.MethodPost, "/api/jobs/"+board.NewID()+"/start", "", nil, "")
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", res.Code)
	}
}

// newTestServer returns a server and its DATA_DIR. The data directory sits one
// level inside a fresh temp root, so a file that escapes it lands somewhere
// assertFiles can still see.
func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return New(newMem(), dir, ""), dir
}

func memOf(srv *Server) *mem {
	return srv.store.(*mem)
}

// readySheet creates a draft with a brochure attached, ready to start.
func readySheet(t *testing.T, srv *Server, cookie, title string) board.Job {
	t.Helper()
	job := createSheet(t, srv, cookie, title, "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")
	return job
}

func start(t *testing.T, srv *Server, cookie, id string) board.Job {
	t.Helper()
	res := call(srv, http.MethodPost, "/api/jobs/"+id+"/start", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("start %d: %s", res.Code, res.Body.String())
	}
	return decodeJob(t, res)
}

// parallel runs fn n times at once and returns the responses in order.
func parallel(n int, fn func(i int) *httptest.ResponseRecorder) []*httptest.ResponseRecorder {
	results := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = fn(i)
		}()
	}
	wg.Wait()
	return results
}

func assertChecklist(t *testing.T, job board.Job, states ...string) {
	t.Helper()
	labels := []string{"Reading the brochure", "Checking the address", "Publishing the sheet"}
	if len(job.Checklist) != len(labels) {
		t.Fatalf("checklist has %d steps, want %d", len(job.Checklist), len(labels))
	}
	for i, step := range job.Checklist {
		if step.Label != labels[i] || step.State != states[i] {
			t.Fatalf("step %d is %q/%s, want %q/%s", i, step.Label, step.State, labels[i], states[i])
		}
	}
}

// assertError checks the status and the exact error text the README names.
func assertError(t *testing.T, res *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	if res.Code != status {
		t.Fatalf("status %d, want %d: %s", res.Code, status, res.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil || body.Error != message {
		t.Fatalf("body %q, want {\"error\":%q}", res.Body.String(), message)
	}
}

func accountID(t *testing.T, srv *Server, cookie string) string {
	t.Helper()
	res := call(srv, http.MethodGet, "/api/me", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("me %d: %s", res.Code, res.Body.String())
	}
	var me account
	if err := json.Unmarshal(res.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	return me.ID
}

func getSheet(t *testing.T, srv *Server, cookie, id string) board.Job {
	t.Helper()
	res := call(srv, http.MethodGet, "/api/jobs/"+id, cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("get %d: %s", res.Code, res.Body.String())
	}
	return decodeJob(t, res)
}

func upload(t *testing.T, srv *Server, cookie, id, name string, data []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	body, formType := multipartBody(t, name, data, contentType)
	return call(srv, http.MethodPost, "/api/jobs/"+id+"/file", cookie, body, formType)
}

// assertFiles fails unless the regular files under root are exactly want.
func assertFiles(t *testing.T, root string, want ...string) {
	t.Helper()
	var got []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			got = append(got, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("files on disk:\n  got  %q\n  want %q", got, want)
	}
}

func signUp(t *testing.T, srv *Server, email string) string {
	t.Helper()
	body := strings.NewReader(`{"email":"` + email + `","password":"password123"}`)
	res := call(srv, http.MethodPost, "/api/register", "", body, "application/json")
	if res.Code != http.StatusCreated {
		t.Fatalf("register %d: %s", res.Code, res.Body.String())
	}
	for _, cookie := range res.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie.Value
		}
	}
	t.Fatal("register did not set a session")
	return ""
}

func createSheet(t *testing.T, srv *Server, cookie, title, address string) board.Job {
	t.Helper()
	body := strings.NewReader(`{"title":"` + title + `","address":"` + address + `"}`)
	res := call(srv, http.MethodPost, "/api/jobs", cookie, body, "application/json")
	if res.Code != http.StatusCreated {
		t.Fatalf("create %d: %s", res.Code, res.Body.String())
	}
	return decodeJob(t, res)
}

func uploadFile(t *testing.T, srv *Server, cookie, id, name string, data []byte, contentType string) {
	t.Helper()
	body, formType := multipartBody(t, name, data, contentType)
	res := call(srv, http.MethodPost, "/api/jobs/"+id+"/file", cookie, body, formType)
	if res.Code != http.StatusOK {
		t.Fatalf("upload %d: %s", res.Code, res.Body.String())
	}
}

// multipartBody builds a form with one "file" part. The filename goes into
// the header as given, without the escaping mime/multipart would apply, so
// tests can send names like ../../escaped.txt exactly as a hostile client would.
func multipartBody(t *testing.T, name string, data []byte, contentType string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, name))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, writer.FormDataContentType()
}

// assertJSONError checks the error body has the documented {"error": "..."} shape.
func assertJSONError(t *testing.T, res *httptest.ResponseRecorder) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Errorf("error body is not {\"error\": \"...\"}: %q", res.Body.String())
	}
}

func decodeJob(t *testing.T, res *httptest.ResponseRecorder) board.Job {
	t.Helper()
	var job board.Job
	if err := json.Unmarshal(res.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	return job
}

func call(srv *Server, method, path, cookie string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	return res
}

type mem struct {
	mu       sync.Mutex
	users    map[string]board.User
	byEmail  map[string]string
	sessions map[string]memSession
	jobs     map[string]board.Job
}

type memSession struct {
	userID  string
	expires time.Time
}

func newMem() *mem {
	return &mem{
		users:    map[string]board.User{},
		byEmail:  map[string]string{},
		sessions: map[string]memSession{},
		jobs:     map[string]board.Job{},
	}
}

func (m *mem) CreateUser(_ context.Context, email, passwordHash string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byEmail[email]; ok {
		return board.User{}, board.ErrDuplicate
	}
	user := board.User{ID: board.NewID(), Email: email, PasswordHash: passwordHash}
	m.users[user.ID] = user
	m.byEmail[email] = user.ID
	return user, nil
}

func (m *mem) UserByEmail(_ context.Context, email string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byEmail[email]
	if !ok {
		return board.User{}, board.ErrNotFound
	}
	return m.users[id], nil
}

func (m *mem) CreateSession(_ context.Context, userID, token string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[token] = memSession{userID: userID, expires: expires}
	return nil
}

func (m *mem) DeleteSession(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
	return nil
}

func (m *mem) UserByToken(_ context.Context, token string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[token]
	if !ok || time.Now().After(session.expires) {
		return board.User{}, board.ErrNotFound
	}
	user, ok := m.users[session.userID]
	if !ok {
		return board.User{}, board.ErrNotFound
	}
	return user, nil
}

func (m *mem) ListJobs(_ context.Context, userID string) ([]board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var jobs []board.Job
	for _, job := range m.jobs {
		if job.UserID == userID {
			jobs = append(jobs, cloneJob(job))
		}
	}
	if jobs == nil {
		jobs = []board.Job{}
	}
	return jobs, nil
}

func (m *mem) CreateJob(_ context.Context, job board.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[job.ID] = cloneJob(job)
	return nil
}

func (m *mem) Job(_ context.Context, userID, id string) (board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok || job.UserID != userID {
		return board.Job{}, board.ErrNotFound
	}
	return cloneJob(job), nil
}

func (m *mem) AttachFile(_ context.Context, userID, id, filename string, at time.Time) (board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok || job.UserID != userID {
		return board.Job{}, board.ErrNotFound
	}
	if job.Filename != "" {
		return board.Job{}, board.ErrConflict
	}
	job.Filename = filename
	job.UpdatedAt = at
	m.jobs[id] = cloneJob(job)
	return cloneJob(job), nil
}

// StartJob mirrors the Postgres store: the conditional UPDATE and the
// one-running-sheet-per-account unique index, under one lock.
func (m *mem) StartJob(_ context.Context, userID, id, runID string, at time.Time) (board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok || job.UserID != userID {
		return board.Job{}, board.ErrNotFound
	}
	if job.Status != board.StatusDraft || job.Filename == "" {
		return board.Job{}, board.ErrConflict
	}
	for _, other := range m.jobs {
		if other.UserID == userID && other.Status == board.StatusRunning {
			return board.Job{}, board.ErrBusy
		}
	}
	job.Status = board.StatusRunning
	job.RunID = runID
	job.UpdatedAt = at
	m.jobs[id] = cloneJob(job)
	return cloneJob(job), nil
}

// edit changes a stored sheet directly, standing in for the worker in tests
// of the HTTP API.
func (m *mem) edit(t *testing.T, id string, change func(*board.Job)) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		t.Fatalf("no sheet %s", id)
	}
	job.Checklist = append([]board.Step(nil), job.Checklist...)
	change(&job)
	m.jobs[id] = job
}

func cloneJob(job board.Job) board.Job {
	job.Checklist = append([]board.Step(nil), job.Checklist...)
	return job
}
