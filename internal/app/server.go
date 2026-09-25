package app

import (
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFiles embed.FS

type rateEntry struct {
	count int
	start time.Time
}
type App struct {
	Store         *Store
	Vault         *Vault
	Executor      *Executor
	Scheduler     *Scheduler
	Origin        string
	Insecure      bool
	limitMu       sync.Mutex
	limits        map[string]rateEntry
	passwordSlots chan struct{}
	dummyHash     string
}

func NewApp(s *Store, v *Vault, e *Executor, sch *Scheduler, origin string, insecure bool) *App {
	dummy, _ := PasswordHash("invalid-user-dummy-password")
	return &App{Store: s, Vault: v, Executor: e, Scheduler: sch, Origin: origin, Insecure: insecure, limits: map[string]rateEntry{}, passwordSlots: make(chan struct{}, 2), dummyHash: dummy}
}

type apiError struct {
	code    int
	message string
}

func (e apiError) Error() string { return e.message }
func invalid(e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, ErrConflict) || errors.Is(e, ErrForbidden) || errors.Is(e, sql.ErrNoRows) {
		return e
	}
	return apiError{400, e.Error()}
}
func respond(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
func fail(w http.ResponseWriter, err error) {
	code, message := 500, "internal error"
	var ae apiError
	switch {
	case errors.As(err, &ae):
		code, message = ae.code, ae.message
	case errors.Is(err, ErrForbidden):
		code, message = 403, "not authorized"
	case errors.Is(err, ErrConflict):
		code, message = 409, ErrConflict.Error()
	case errors.Is(err, sql.ErrNoRows):
		code, message = 404, "not found"
	}
	respond(w, code, map[string]string{"error": message})
}
func body(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 128*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return apiError{400, "invalid JSON body or unknown field"}
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return apiError{400, "request must contain one JSON object"}
	}
	return nil
}
func (a *App) limited(key string, max int) bool {
	a.limitMu.Lock()
	defer a.limitMu.Unlock()
	now := time.Now()
	if len(a.limits) >= 10000 {
		for k, v := range a.limits {
			if now.Sub(v.start) > time.Minute {
				delete(a.limits, k)
			}
		}
		if len(a.limits) >= 10000 {
			return true
		}
	}
	v := a.limits[key]
	if now.Sub(v.start) > time.Minute {
		v = rateEntry{start: now}
	}
	v.count++
	a.limits[key] = v
	return v.count > max
}
func (a *App) passwordOK(hash, password string) bool {
	select {
	case a.passwordSlots <- struct{}{}:
		defer func() { <-a.passwordSlots }()
		return PasswordOK(hash, password)
	default:
		return false
	}
}
func (a *App) cookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: "sprite_cron_session", Value: value, Path: "/", HttpOnly: true, Secure: !a.Insecure, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

type endpoint func(http.ResponseWriter, *http.Request, Principal) error

func (a *App) protected(fn endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p Principal
		var err error
		auth := r.Header.Get("Authorization")
		cookie, cookieErr := r.Cookie("sprite_cron_session")
		if auth != "" && cookieErr == nil {
			fail(w, apiError{400, "use either a bearer token or a browser session"})
			return
		}
		if strings.HasPrefix(auth, "Bearer ") {
			p, err = a.Store.AuthenticateToken(strings.TrimPrefix(auth, "Bearer "))
		} else if cookieErr == nil {
			p, err = a.Store.AuthenticateSession(cookie.Value)
		} else {
			err = ErrForbidden
		}
		if err != nil {
			respond(w, 401, map[string]string{"error": "authentication required"})
			return
		}
		if p.TokenID == "" && isMutation(r.Method) {
			if r.Header.Get("Origin") != a.Origin || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(p.CSRF)) != 1 {
				fail(w, ErrForbidden)
				return
			}
		}
		if err = fn(w, r, p); err != nil {
			fail(w, err)
		}
	}
}
func (a *App) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"status": "ok"}) })
	m.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if a.Scheduler.Failed.Load() || nowMS()-a.Scheduler.Heartbeat.Load() > 10000 || a.Store.DB.Ping() != nil {
			respond(w, 503, map[string]string{"status": "unavailable"})
			return
		}
		respond(w, 200, map[string]string{"status": "ready"})
	})
	m.HandleFunc("POST /api/login", a.login)
	m.HandleFunc("GET /api/me", a.protected(func(w http.ResponseWriter, r *http.Request, p Principal) error {
		respond(w, 200, map[string]any{"user": p.User, "csrf": p.CSRF})
		return nil
	}))
	m.HandleFunc("POST /api/logout", a.protected(func(w http.ResponseWriter, r *http.Request, p Principal) error {
		if p.TokenID != "" {
			return ErrForbidden
		}
		_, err := a.Store.DB.Exec(`DELETE FROM sessions WHERE digest=?`, p.Session)
		a.cookie(w, "", -1)
		respond(w, 200, map[string]bool{"ok": true})
		return err
	}))
	m.HandleFunc("POST /api/password", a.protected(a.password))
	m.HandleFunc("GET /api/credentials", a.protected(a.credentialsList))
	m.HandleFunc("POST /api/credentials", a.protected(a.credentialsSave))
	m.HandleFunc("PUT /api/credentials/{id}", a.protected(a.credentialsSave))
	m.HandleFunc("GET /api/targets", a.protected(a.targetsList))
	m.HandleFunc("POST /api/targets", a.protected(a.targetsSave))
	m.HandleFunc("PUT /api/targets/{id}", a.protected(a.targetsSave))
	m.HandleFunc("GET /api/jobs", a.protected(a.jobsList))
	m.HandleFunc("POST /api/jobs", a.protected(a.jobsSave))
	m.HandleFunc("GET /api/jobs/{id}", a.protected(a.jobGet))
	m.HandleFunc("PUT /api/jobs/{id}", a.protected(a.jobsSave))
	m.HandleFunc("DELETE /api/jobs/{id}", a.protected(a.jobDelete))
	m.HandleFunc("POST /api/jobs/{id}/runs", a.protected(a.runNow))
	m.HandleFunc("POST /api/schedules/preview", a.protected(a.preview))
	m.HandleFunc("GET /api/runs", a.protected(a.runsList))
	m.HandleFunc("GET /api/runs/{id}", a.protected(a.runGet))
	m.HandleFunc("POST /api/runs/{id}/cancel", a.protected(a.runCancel))
	m.HandleFunc("POST /api/runs/{id}/resolve", a.protected(a.runResolve))
	m.HandleFunc("POST /api/runs/{id}/retry", a.protected(a.runRetry))
	m.HandleFunc("GET /api/tokens", a.protected(a.tokensList))
	m.HandleFunc("POST /api/tokens", a.protected(a.tokensCreate))
	m.HandleFunc("DELETE /api/tokens/{id}", a.protected(a.tokensDelete))
	m.HandleFunc("GET /api/audit", a.protected(a.auditList))
	m.HandleFunc("GET /api/status", a.protected(a.status))
	static, _ := fs.Sub(webFiles, "web")
	m.Handle("GET /", http.FileServer(http.FS(static)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Cache-Control", "no-store")
		m.ServeHTTP(w, r)
	})
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != a.Origin {
		fail(w, ErrForbidden)
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if a.limited("ip:"+ip, 30) {
		fail(w, apiError{429, "too many login attempts; wait one minute"})
		return
	}
	var input struct{ Username, Password string }
	if err := body(w, r, &input); err != nil {
		fail(w, err)
		return
	}
	if a.limited("user:"+input.Username, 10) {
		fail(w, apiError{429, "too many login attempts; wait one minute"})
		return
	}
	u, err := userFrom(a.Store.DB, "username", input.Username)
	hash := u.Password
	if err != nil || u.Service {
		hash = a.dummyHash
	}
	valid := a.passwordOK(hash, input.Password)
	if !valid || err != nil || u.Disabled || u.Service {
		respond(w, 401, map[string]string{"error": "invalid username or password"})
		return
	}
	raw, csrf, err := a.Store.NewSession(u)
	if err != nil {
		fail(w, err)
		return
	}
	a.cookie(w, raw, 12*3600)
	respond(w, 200, map[string]any{"user": u, "csrf": csrf})
}
func (a *App) password(w http.ResponseWriter, r *http.Request, p Principal) error {
	if p.TokenID != "" {
		return ErrForbidden
	}
	var input struct{ Current, New string }
	if err := body(w, r, &input); err != nil {
		return err
	}
	if a.limited("verify:"+p.User.ID, 10) || !a.passwordOK(p.User.Password, input.Current) {
		return ErrForbidden
	}
	if err := a.Store.ChangeUser(p.User.Username, "password", input.New, p.Actor()); err != nil {
		return invalid(err)
	}
	a.cookie(w, "", -1)
	respond(w, 200, map[string]bool{"ok": true})
	return nil
}
func (a *App) credentialsList(w http.ResponseWriter, r *http.Request, p Principal) error {
	if !p.Admin() {
		return ErrForbidden
	}
	rows, err := a.Store.DB.Query(`SELECT id,label,kind,version FROM credentials ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var id, label, kind string
		var version int
		if err = rows.Scan(&id, &label, &kind, &version); err != nil {
			return err
		}
		items = append(items, map[string]any{"id": id, "label": label, "kind": kind, "version": version})
	}
	if err = rows.Err(); err != nil {
		return err
	}
	respond(w, 200, items)
	return nil
}
func (a *App) credentialsSave(w http.ResponseWriter, r *http.Request, p Principal) error {
	if !p.Admin() {
		return ErrForbidden
	}
	var input struct{ ID, Label, Kind, Value string }
	if err := body(w, r, &input); err != nil {
		return err
	}
	if r.PathValue("id") != "" {
		input.ID = r.PathValue("id")
	}
	if !identifier.MatchString(input.ID) || input.Label == "" || len(input.Label) > 120 || len(input.Value) < 1 || len(input.Value) > 8192 || strings.ContainsAny(input.Value, "\r\n") || (input.Kind != "sprite" && input.Kind != "header") {
		return apiError{400, "valid id, label, kind (sprite/header), and value are required"}
	}
	sealed, err := a.Vault.Seal(input.ID, input.Value)
	if err != nil {
		return err
	}
	err = transaction(a.Store.DB, func(tx *sql.Tx) error {
		if r.Method == "POST" {
			_, err = tx.Exec(`INSERT INTO credentials(id,label,kind,encrypted) VALUES(?,?,?,?)`, input.ID, input.Label, input.Kind, sealed)
		} else {
			var active int
			if err = tx.QueryRow(`SELECT count(*) FROM runs WHERE status IN ('running','dispatching','recovering')`).Scan(&active); err != nil {
				return err
			}
			if active > 0 {
				return apiError{409, "wait for active attempts before rotating credentials"}
			}
			result, e := tx.Exec(`UPDATE credentials SET label=?,kind=?,encrypted=?,version=version+1 WHERE id=?`, input.Label, input.Kind, sealed, input.ID)
			err = e
			if e == nil {
				n, _ := result.RowsAffected()
				if n == 0 {
					return sql.ErrNoRows
				}
			}
		}
		if err != nil {
			return err
		}
		return audit(tx, p.Actor(), "credential.save", input.ID, "")
	})
	if err != nil {
		return err
	}
	respond(w, 200, map[string]string{"id": input.ID})
	return nil
}
func (a *App) targetsList(w http.ResponseWriter, r *http.Request, p Principal) error {
	if !p.Allows("targets:read", "") {
		return ErrForbidden
	}
	targets, err := a.Store.Targets()
	if err != nil {
		return err
	}
	out := []Target{}
	for _, t := range targets {
		if p.Allows("targets:read", t.ID) {
			out = append(out, t)
		}
	}
	respond(w, 200, out)
	return nil
}
func (a *App) targetsSave(w http.ResponseWriter, r *http.Request, p Principal) error {
	if !p.Admin() {
		return ErrForbidden
	}
	var t Target
	if err := body(w, r, &t); err != nil {
		return err
	}
	id := r.PathValue("id")
	if id != "" {
		old, err := targetFrom(a.Store.DB, id)
		if err != nil {
			return err
		}
		t.ID = id
		t.SpriteID = old.SpriteID
	} else {
		t.SpriteID = ""
	}
	// Credential types prevent accidental use of application secrets as API tokens.
	for _, item := range []struct{ id, kind string }{{t.CredentialID, "sprite"}, {t.HTTPCredentialID, "sprite"}, {t.AppCredentialID, "header"}} {
		if item.id == "" {
			continue
		}
		var kind string
		if err := a.Store.DB.QueryRow(`SELECT kind FROM credentials WHERE id=?`, item.id).Scan(&kind); err != nil {
			return invalid(errors.New("credential does not exist"))
		}
		if kind != item.kind {
			return apiError{400, "credential type does not match its use"}
		}
	}
	validated, err := a.Executor.ValidateTarget(r.Context(), t)
	if err != nil {
		return invalid(err)
	}
	err = transaction(a.Store.DB, func(tx *sql.Tx) error {
		if id == "" {
			_, err = tx.Exec(`INSERT INTO targets(id,data) VALUES(?,?)`, validated.ID, encoded(validated))
		} else {
			_, err = tx.Exec(`UPDATE targets SET data=? WHERE id=?`, encoded(validated), id)
		}
		if err != nil {
			return err
		}
		return audit(tx, p.Actor(), "target.save", validated.ID, "")
	})
	if err != nil {
		return err
	}
	respond(w, 200, validated)
	return nil
}
func (a *App) jobsList(w http.ResponseWriter, r *http.Request, p Principal) error {
	if !p.Allows("jobs:read", "") {
		return ErrForbidden
	}
	jobs, err := a.Store.Jobs()
	if err != nil {
		return err
	}
	out := []Job{}
	for _, j := range jobs {
		if p.Allows("jobs:read", j.TargetID) {
			out = append(out, j)
		}
	}
	respond(w, 200, out)
	return nil
}
func (a *App) jobGet(w http.ResponseWriter, r *http.Request, p Principal) error {
	j, err := jobFrom(a.Store.DB, r.PathValue("id"))
	if err != nil {
		return err
	}
	if !p.Allows("jobs:read", j.TargetID) {
		return ErrForbidden
	}
	respond(w, 200, j)
	return nil
}
func (a *App) jobsSave(w http.ResponseWriter, r *http.Request, p Principal) error {
	var j Job
	if err := body(w, r, &j); err != nil {
		return err
	}
	if !p.Allows("jobs:write", j.TargetID) {
		return ErrForbidden
	}
	if id := r.PathValue("id"); id != "" {
		old, err := jobFrom(a.Store.DB, id)
		if err != nil {
			return err
		}
		if !p.Allows("jobs:write", old.TargetID) {
			return ErrForbidden
		}
		j.ID = id
		if err = a.Store.SaveJob(j, false, p.Actor()); err != nil {
			return invalid(err)
		}
		j, err = jobFrom(a.Store.DB, id)
		if err != nil {
			return err
		}
		respond(w, 200, j)
	} else {
		created, err := a.Store.CreateJob(j, p.Actor())
		if err != nil {
			return invalid(err)
		}
		respond(w, 201, created)
	}
	return nil
}
func (a *App) jobDelete(w http.ResponseWriter, r *http.Request, p Principal) error {
	j, err := jobFrom(a.Store.DB, r.PathValue("id"))
	if err != nil {
		return err
	}
	if !p.Allows("jobs:write", j.TargetID) {
		return ErrForbidden
	}
	version, e := strconv.Atoi(r.Header.Get("If-Match"))
	if e != nil || version != j.Revision {
		return ErrConflict
	}
	err = transaction(a.Store.DB, func(tx *sql.Tx) error {
		result, e := tx.Exec(`DELETE FROM jobs WHERE id=? AND revision=?`, j.ID, version)
		if e != nil {
			return e
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return ErrConflict
		}
		if _, e = tx.Exec(`UPDATE runs SET status=CASE WHEN status='retry_wait' AND EXISTS(SELECT 1 FROM attempts WHERE run_id=runs.id AND number=runs.attempt AND status='unknown') THEN 'unknown' ELSE 'cancelled' END,reason='job deleted; queued execution cancelled' WHERE job_id=? AND status IN ('queued','retry_wait')`, j.ID); e != nil {
			return e
		}
		return audit(tx, p.Actor(), "job.delete", j.ID, "")
	})
	if err != nil {
		return err
	}
	respond(w, 200, map[string]bool{"ok": true})
	return nil
}
func (a *App) runNow(w http.ResponseWriter, r *http.Request, p Principal) error {
	j, err := jobFrom(a.Store.DB, r.PathValue("id"))
	if err != nil {
		return err
	}
	if !p.Allows("runs:trigger", j.TargetID) {
		return ErrForbidden
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 128 {
		return apiError{400, "Idempotency-Key header (1–128 bytes) is required"}
	}
	id, err := a.Store.Enqueue(j.ID, p.User.ID+":"+key, p.Actor())
	if err != nil {
		return invalid(err)
	}
	respond(w, 202, map[string]string{"id": id})
	return nil
}
func (a *App) preview(w http.ResponseWriter, r *http.Request, p Principal) error {
	if !p.Allows("jobs:read", "") {
		return ErrForbidden
	}
	var in struct{ Schedule, Timezone string }
	if err := body(w, r, &in); err != nil {
		return err
	}
	if in.Timezone == "" {
		in.Timezone = "UTC"
	}
	sc, err := schedule(in.Schedule, in.Timezone)
	if err != nil {
		return invalid(err)
	}
	at := time.Now()
	out := []string{}
	loc, _ := time.LoadLocation(in.Timezone)
	for range 5 {
		at = sc.Next(at)
		out = append(out, at.In(loc).Format(time.RFC3339))
	}
	respond(w, 200, out)
	return nil
}
func (a *App) runsList(w http.ResponseWriter, r *http.Request, p Principal) error {
	if !p.Allows("runs:read", "") {
		return ErrForbidden
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	runs, err := a.Store.Runs(100, offset)
	if err != nil {
		return err
	}
	out := []Run{}
	for _, run := range runs {
		if p.Allows("runs:read", run.TargetID) && (r.URL.Query().Get("job_id") == "" || r.URL.Query().Get("job_id") == run.JobID) {
			run.Stdout = ""
			run.Stderr = ""
			out = append(out, run)
		}
	}
	respond(w, 200, map[string]any{"runs": out, "next_offset": offset + len(runs), "has_more": len(runs) == 100})
	return nil
}
func (a *App) runGet(w http.ResponseWriter, r *http.Request, p Principal) error {
	run, err := a.Store.Run(r.PathValue("id"))
	if err != nil {
		return err
	}
	if !p.Allows("runs:read", run.TargetID) {
		return ErrForbidden
	}
	rows, err := a.Store.DB.Query(`SELECT number,started,finished,status,reason,session_id,truncated,exit_code,http_status FROM attempts WHERE run_id=? ORDER BY number`, run.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	attempts := []any{}
	for rows.Next() {
		var n, started int64
		var finished, exit *int64
		var status, reason, session string
		var truncated bool
		var httpCode int
		if err = rows.Scan(&n, &started, &finished, &status, &reason, &session, &truncated, &exit, &httpCode); err != nil {
			return err
		}
		attempts = append(attempts, map[string]any{"number": n, "started_at": started, "finished_at": finished, "status": status, "reason": reason, "session_id": session, "truncated": truncated, "exit_code": exit, "http_status": httpCode})
	}
	if err = rows.Err(); err != nil {
		return err
	}
	respond(w, 200, map[string]any{"run": run, "attempts": attempts})
	return nil
}
func (a *App) runCancel(w http.ResponseWriter, r *http.Request, p Principal) error {
	run, err := a.Store.Run(r.PathValue("id"))
	if err != nil {
		return err
	}
	if !p.Allows("runs:cancel", run.TargetID) {
		return ErrForbidden
	}
	if run.Status == "unknown" {
		if run.SessionID == "" || !a.Executor.Terminate(r.Context(), run.Snapshot.Target, run.SessionID) {
			return apiError{409, "remote termination unconfirmed; resolve explicitly only after investigation"}
		}
		_, err = a.Store.DB.Exec(`UPDATE runs SET status='cancelled',resolved=1,reason='remote termination confirmed' WHERE id=? AND status='unknown'`, run.ID)
	} else {
		_, err = a.Store.DB.Exec(`UPDATE runs SET cancel=1 WHERE id=? AND status IN ('queued','retry_wait','running','dispatching','recovering')`, run.ID)
	}
	if err != nil {
		return err
	}
	if err = audit(a.Store.DB, p.Actor(), "run.cancel", run.ID, ""); err != nil {
		return err
	}
	respond(w, 202, map[string]bool{"ok": true})
	return nil
}
func (a *App) runResolve(w http.ResponseWriter, r *http.Request, p Principal) error {
	run, err := a.Store.Run(r.PathValue("id"))
	if err != nil {
		return err
	}
	if !p.Allows("runs:cancel", run.TargetID) {
		return ErrForbidden
	}
	var in struct {
		AcceptRisk bool   `json:"accept_risk"`
		Note       string `json:"note"`
	}
	if err = body(w, r, &in); err != nil {
		return err
	}
	if !in.AcceptRisk || len(in.Note) < 1 || len(in.Note) > 500 {
		return apiError{400, "accept_risk=true and a note are required"}
	}
	err = transaction(a.Store.DB, func(tx *sql.Tx) error {
		res, e := tx.Exec(`UPDATE runs SET resolved=1 WHERE id=? AND status='unknown'`, run.ID)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ErrConflict
		}
		return audit(tx, p.Actor(), "run.resolve", run.ID, in.Note)
	})
	if err != nil {
		return err
	}
	respond(w, 200, map[string]bool{"ok": true})
	return nil
}
func (a *App) runRetry(w http.ResponseWriter, r *http.Request, p Principal) error {
	run, err := a.Store.Run(r.PathValue("id"))
	if err != nil {
		return err
	}
	if !p.Allows("runs:trigger", run.TargetID) {
		return ErrForbidden
	}
	if run.Status != "failed" && run.Status != "timed_out" && run.Status != "cancelled" && !(run.Status == "unknown" && run.Resolved) {
		return ErrConflict
	}
	if !run.Snapshot.Job.Policy.RetrySafe {
		return apiError{409, "manual retry requires retry_safe on the run snapshot"}
	}
	err = transaction(a.Store.DB, func(tx *sql.Tx) error {
		res, e := tx.Exec(`UPDATE runs SET status='retry_wait',next_attempt=?,cancel=0,resolved=0,session_id='' WHERE id=? AND status=? AND resolved=?`, nowMS(), run.ID, run.Status, run.Resolved)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ErrConflict
		}
		return audit(tx, p.Actor(), "run.retry", run.ID, "")
	})
	if err != nil {
		return err
	}
	respond(w, 202, map[string]string{"id": run.ID})
	return nil
}
func (a *App) tokensList(w http.ResponseWriter, r *http.Request, p Principal) error {
	if p.TokenID != "" {
		return ErrForbidden
	}
	tokens, err := a.Store.Tokens()
	if err != nil {
		return err
	}
	out := []TokenInfo{}
	for _, t := range tokens {
		if p.Admin() || t.UserID == p.User.ID {
			out = append(out, t)
		}
	}
	respond(w, 200, out)
	return nil
}
func (a *App) tokensCreate(w http.ResponseWriter, r *http.Request, p Principal) error {
	if p.TokenID != "" {
		return ErrForbidden
	}
	var in struct {
		Name, Password  string
		Scopes, Targets []string
		ExpiresHours    int `json:"expires_hours"`
	}
	if err := body(w, r, &in); err != nil {
		return err
	}
	if a.limited("verify:"+p.User.ID, 10) || !a.passwordOK(p.User.Password, in.Password) {
		return ErrForbidden
	}
	if in.ExpiresHours == 0 {
		in.ExpiresHours = 90 * 24
	}
	secret, info, err := a.Store.NewToken(p.User.Username, in.Name, in.Scopes, in.Targets, time.Duration(in.ExpiresHours)*time.Hour, p.Actor())
	if err != nil {
		return invalid(err)
	}
	respond(w, 201, map[string]any{"token": secret, "metadata": info})
	return nil
}
func (a *App) tokensDelete(w http.ResponseWriter, r *http.Request, p Principal) error {
	if p.TokenID != "" {
		return ErrForbidden
	}
	var owner string
	if err := a.Store.DB.QueryRow(`SELECT user_id FROM api_tokens WHERE id=?`, r.PathValue("id")).Scan(&owner); err != nil {
		return err
	}
	if owner != p.User.ID && !p.Admin() {
		return ErrForbidden
	}
	if err := a.Store.RevokeToken(r.PathValue("id"), p.Actor()); err != nil {
		return err
	}
	respond(w, 200, map[string]bool{"ok": true})
	return nil
}
func (a *App) auditList(w http.ResponseWriter, r *http.Request, p Principal) error {
	if !p.Admin() {
		return ErrForbidden
	}
	rows, err := a.Store.DB.Query(`SELECT at,actor,action,object,detail FROM audit ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return err
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var at int64
		var actor, action, object, detail string
		if err = rows.Scan(&at, &actor, &action, &object, &detail); err != nil {
			return err
		}
		items = append(items, map[string]any{"at": at, "actor": actor, "action": action, "object": object, "detail": detail})
	}
	if err = rows.Err(); err != nil {
		return err
	}
	respond(w, 200, items)
	return nil
}
func (a *App) status(w http.ResponseWriter, r *http.Request, p Principal) error {
	if !p.Admin() {
		return ErrForbidden
	}
	rows, err := a.Store.DB.Query(`SELECT status,count(*) FROM runs GROUP BY status`)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		rows.Scan(&state, &n)
		counts[state] = n
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	respond(w, 200, map[string]any{"runs": counts, "heartbeat": a.Scheduler.Heartbeat.Load(), "dispatch_failed": a.Scheduler.Failed.Load(), "dispatch_disabled": a.Scheduler.Disabled})
	return nil
}
