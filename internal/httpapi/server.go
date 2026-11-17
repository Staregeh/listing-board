package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"listingboard/internal/board"
)

const sessionCookie = "lb_session"

type Server struct {
	store   board.Store
	dataDir string
	webDir  string
	handler http.Handler
}

type account struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

func New(store board.Store, dataDir, webDir string) *Server {
	s := &Server{store: store, dataDir: dataDir, webDir: webDir}
	s.handler = withCORS(s.routes())
	return s
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/register", s.register)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/jobs", s.listJobs)
	mux.HandleFunc("POST /api/jobs", s.createJob)
	mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
	mux.HandleFunc("POST /api/jobs/{id}/file", s.upload)
	mux.HandleFunc("POST /api/jobs/{id}/start", s.start)
	mux.HandleFunc("GET /api/jobs/{id}/file", s.download)
	if s.webDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.webDir)))
	}
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Request body is not valid JSON")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if !strings.Contains(email, "@") || len(email) > 200 {
		writeErr(w, http.StatusBadRequest, "Enter an email address")
		return
	}
	if len(body.Password) < 8 || len(body.Password) > 200 {
		writeErr(w, http.StatusBadRequest, "Password must be at least 8 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the account")
		return
	}
	user, err := s.store.CreateUser(r.Context(), email, string(hash))
	if errors.Is(err, board.ErrDuplicate) {
		writeErr(w, http.StatusConflict, "An account with that email already exists")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the account")
		return
	}
	if err := s.openSession(w, r, user); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start the session")
		return
	}
	writeJSON(w, http.StatusCreated, account{ID: user.ID, Email: user.Email})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Request body is not valid JSON")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	user, err := s.store.UserByEmail(r.Context(), email)
	if errors.Is(err, board.ErrNotFound) {
		writeErr(w, http.StatusUnauthorized, "That email or password is incorrect")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not sign in")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(body.Password)) != nil {
		writeErr(w, http.StatusUnauthorized, "That email or password is incorrect")
		return
	}
	if err := s.openSession(w, r, user); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start the session")
		return
	}
	writeJSON(w, http.StatusOK, account{ID: user.ID, Email: user.Email})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, account{ID: user.ID, Email: user.Email})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	jobs, err := s.store.ListJobs(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load sheets")
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	var body struct {
		Title   string `json:"title"`
		Address string `json:"address"`
	}
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Request body is not valid JSON")
		return
	}
	title := strings.TrimSpace(body.Title)
	address := strings.TrimSpace(body.Address)
	if title == "" || address == "" || len(title) > 200 || len(address) > 200 {
		writeErr(w, http.StatusBadRequest, "Enter a title and an address")
		return
	}
	now := time.Now().UTC()
	job := board.Job{
		ID:        board.NewID(),
		UserID:    user.ID,
		Title:     title,
		Address:   address,
		Status:    "draft",
		Checklist: board.NewChecklist(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.CreateJob(r.Context(), job); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the sheet")
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	if job.Filename != "" {
		writeErr(w, http.StatusBadRequest, msgAlreadyAttached)
		return
	}

	// The body limit leaves room for the multipart framing around the file;
	// the file itself is held to MaxBrochureSize when it is read below.
	r.Body = http.MaxBytesReader(w, r.Body, board.MaxBrochureSize+multipartSlack)
	name, data, status, msg := readBrochure(r)
	if msg != "" {
		writeErr(w, status, msg)
		return
	}
	if _, ok := board.ClassifyBrochure(data); !ok {
		writeErr(w, http.StatusBadRequest, "Brochure must be a PDF or a UTF-8 text file")
		return
	}

	// Only a file that passed every check reaches the disk. It goes to a
	// temporary name first, and replaces body only once the sheet has
	// claimed the upload, so a rejected or losing upload leaves nothing.
	path := board.BrochurePath(s.dataDir, user.ID, job.ID)
	tmp, err := writeTemp(filepath.Dir(path), data)
	if err != nil {
		log.Printf("upload %s: %v", job.ID, err)
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	attached, err := s.store.AttachFile(r.Context(), user.ID, job.ID, name, time.Now().UTC())
	if err != nil {
		_ = os.Remove(tmp)
		switch {
		case errors.Is(err, board.ErrConflict):
			writeErr(w, http.StatusBadRequest, msgAlreadyAttached)
		case errors.Is(err, board.ErrNotFound):
			writeErr(w, http.StatusNotFound, "Not found")
		default:
			log.Printf("upload %s: %v", job.ID, err)
			writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		}
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		// The sheet now names a brochure that is not on disk. The worker
		// reports that as "Brochure is missing", so the failure stays visible.
		_ = os.Remove(tmp)
		log.Printf("upload %s: %v", job.ID, err)
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	writeJSON(w, http.StatusOK, attached)
}

const (
	msgAlreadyAttached = "This sheet already has a brochure"
	msgTooLarge        = "Brochure must be 1 MiB or smaller"
	// multipartSlack is how far the whole request may exceed the file limit:
	// enough for boundaries, part headers and a long filename.
	multipartSlack = 64 << 10
)

// readBrochure streams the multipart body to the "file" part and reads at
// most one byte past the size limit, so an oversized file is rejected
// without being buffered or written anywhere. It returns the client's
// original filename exactly as sent; the caller must never use it as a path.
func readBrochure(r *http.Request) (name string, data []byte, status int, msg string) {
	mr, err := r.MultipartReader()
	if err != nil {
		return "", nil, http.StatusBadRequest, "Send the brochure as multipart form field file"
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return "", nil, http.StatusBadRequest, "Choose a brochure file"
		}
		if err != nil {
			return "", nil, http.StatusBadRequest, uploadReadError(err)
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		name := originalFilename(part)
		if strings.TrimSpace(name) == "" {
			return "", nil, http.StatusBadRequest, "Choose a brochure file"
		}
		data, err := io.ReadAll(io.LimitReader(part, board.MaxBrochureSize+1))
		if err != nil {
			return "", nil, http.StatusBadRequest, uploadReadError(err)
		}
		if len(data) > board.MaxBrochureSize {
			return "", nil, http.StatusBadRequest, msgTooLarge
		}
		return name, data, 0, ""
	}
}

func uploadReadError(err error) string {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return msgTooLarge
	}
	return "Brochure upload was incomplete"
}

// originalFilename returns the filename parameter exactly as the client sent
// it. multipart.Part.FileName would strip directories, but the sheet keeps
// the original name, and safety comes from never using it as a path.
func originalFilename(part *multipart.Part) string {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil {
		return ""
	}
	return params["filename"]
}

// writeTemp writes data to a new temporary file in dir and returns its path.
func writeTemp(dir string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "body.tmp-*")
	if err != nil {
		return "", err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(f.Name())
		return "", errors.Join(werr, cerr)
	}
	return f.Name(), nil
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	if strings.TrimSpace(job.Filename) == "" {
		writeErr(w, http.StatusBadRequest, "Attach a brochure first")
		return
	}
	job.Status = "success"
	job.RunID = board.NewID()
	job.Error = ""
	for i := range job.Checklist {
		job.Checklist[i].State = "done"
	}
	job.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateJob(r.Context(), job); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start the sheet")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	if job.Filename == "" {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	// Brochures are at most 1 MiB, so reading one whole is cheap, and the
	// type is decided from the stored bytes, the same way the upload was.
	data, err := os.ReadFile(board.BrochurePath(s.dataDir, user.ID, job.ID))
	if err != nil {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	contentType, ok := board.ClassifyBrochure(data)
	if !ok {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", attachment(job.Filename))
	http.ServeContent(w, r, "", job.UpdatedAt, bytes.NewReader(data))
}

// attachment builds a Content-Disposition header for the original filename,
// minus any directories, encoded so quotes or non-ASCII can't break it.
func attachment(filename string) string {
	name := path.Base(strings.ReplaceAll(filename, `\`, "/"))
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": name}); v != "" {
		return v
	}
	return "attachment"
}

// load fetches the sheet named in the path for the signed-in user and writes
// 404 when it is missing or belongs to someone else.
func (s *Server) load(w http.ResponseWriter, r *http.Request, user board.User) (board.Job, bool) {
	job, err := s.store.Job(r.Context(), user.ID, r.PathValue("id"))
	if errors.Is(err, board.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Not found")
		return board.Job{}, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load the sheet")
		return board.Job{}, false
	}
	return job, true
}

func (s *Server) current(w http.ResponseWriter, r *http.Request) (board.User, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		writeErr(w, http.StatusUnauthorized, "Sign in")
		return board.User{}, false
	}
	user, err := s.store.UserByToken(r.Context(), cookie.Value)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "Sign in")
		return board.User{}, false
	}
	return user, true
}

func (s *Server) openSession(w http.ResponseWriter, r *http.Request, user board.User) error {
	token := board.NewToken()
	if err := s.store.CreateSession(r.Context(), user.ID, token, time.Now().Add(24*time.Hour)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400,
	})
	return nil
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin == "http://localhost:5173" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
