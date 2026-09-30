package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func fixture(t *testing.T) (*Store, *Vault, Job) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Close() })
	v := &Vault{Keys: map[string][]byte{"v1": randomBytes(32)}, Active: "v1"}
	cipher, err := v.Seal("cred", "test-private-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`INSERT INTO credentials(id,label,kind,encrypted) VALUES('cred','test','sprite',?)`, cipher); err != nil {
		t.Fatal(err)
	}
	target := Target{ID: "test", Name: "test-sprite", SpriteID: "sprite-id", URL: "https://test.sprites.app", CredentialID: "cred"}
	if _, err = s.DB.Exec(`INSERT INTO targets(id,data) VALUES(?,?)`, target.ID, encoded(target)); err != nil {
		t.Fatal(err)
	}
	j, err := s.CreateJob(Job{Name: "test", TargetID: "test", Schedule: "* * * * *", Timezone: "UTC", Enabled: true, Execution: Execution{Type: "exec", Argv: []string{"/bin/true"}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return s, v, j
}
func mustExec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
func queued(t *testing.T, s *Store, j Job) Run {
	t.Helper()
	id, err := s.Enqueue(j.ID, randomID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Run(id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCronDSTAndValidation(t *testing.T) {
	s, err := schedule("30 2 * * *", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	if got := s.Next(before).UTC(); !got.Equal(time.Date(2026, 3, 9, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("spring DST: %s", got)
	}
	s, err = schedule("30 1 * * *", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	first := s.Next(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	second := s.Next(first)
	if second.Sub(first) != time.Hour {
		t.Fatalf("fall DST: %s -> %s", first, second)
	}
	for _, spec := range []string{"@hourly", "* * * * * *", "CRON_TZ=UTC * * * * *", "0 0 31 2 *"} {
		if _, err := schedule(spec, "UTC"); err == nil {
			t.Fatalf("accepted %s", spec)
		}
	}
	for _, path := range []string{"https://evil.test/", "//evil.test/", "/\\evil", "/ok\n"} {
		j := Job{Name: "x", TargetID: "test", Schedule: "* * * * *", Execution: Execution{Type: "http", Path: path}}
		if j.Validate() == nil {
			t.Fatalf("accepted path %q", path)
		}
	}
}
func TestDurableQueueAndSnapshots(t *testing.T) {
	s, _, j := fixture(t)
	id, err := s.Enqueue(j.ID, "same", "test")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Enqueue(j.ID, "same", "test")
	if err != nil || again != id {
		t.Fatal("manual idempotency", err)
	}
	j.Name = "edited"
	if err = s.SaveJob(j, false, "test"); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveJob(j, false, "test"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale revision accepted", err)
	}
	r, _ := s.Run(id)
	if r.Snapshot.Job.Name != "test" || r.Status != "queued" {
		t.Fatal("manual snapshot mutated", r)
	}
	sch := NewScheduler(s, nil, 2, 1)
	mustExec(t, s, `UPDATE runs SET start_before=? WHERE id=?`, nowMS()-1, id)
	if _, err = sch.claim(time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	r, _ = s.Run(id)
	if r.Status != "skipped" {
		t.Fatalf("expired claim rolled back: %s", r.Status)
	}
}
func TestMaterializationAndOverlap(t *testing.T) {
	s, _, j := fixture(t)
	sch := NewScheduler(s, nil, 10, 2)
	due := time.Now().Truncate(time.Minute)
	j.NextRun = due.UnixMilli()
	mustExec(t, s, `UPDATE jobs SET next_run=?,data=? WHERE id=?`, j.NextRun, encoded(j), j.ID)
	if err := sch.Materialize(due.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sch.Materialize(due.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(100, 0)
	if len(runs) != 1 {
		t.Fatal("duplicate occurrence", len(runs))
	}
	r, err := sch.claim(due.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	second := queued(t, s, j)
	if _, err = sch.claim(time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("overlap claimed", err)
	}
	second, _ = s.Run(second.ID)
	if second.Status != "skipped" {
		t.Fatal("overlap skip not committed", second.Status)
	}
	mustExec(t, s, `UPDATE runs SET status='unknown' WHERE id=?`, r.ID)
	third := queued(t, s, j)
	sch.claim(time.Now())
	third, _ = s.Run(third.ID)
	if third.Status != "skipped" {
		t.Fatal("unknown did not block overlap")
	}
	mustExec(t, s, `UPDATE runs SET resolved=1 WHERE id=?`, r.ID)
	queued(t, s, j)
	if _, err = sch.claim(time.Now()); err != nil {
		t.Fatal("resolved unknown still blocks", err)
	}
}
func TestRecoveryAndRetry(t *testing.T) {
	s, _, j := fixture(t)
	sch := NewScheduler(s, nil, 10, 2)
	r := queued(t, s, j)
	mustExec(t, s, `UPDATE runs SET status='running',session_id='remote-1',attempt=1,deadline=? WHERE id=?`, nowMS()+10000, r.ID)
	other := queued(t, s, j)
	mustExec(t, s, `UPDATE runs SET status='dispatching' WHERE id=?`, other.ID)
	if err := sch.Recover(); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Run(r.ID)
	other, _ = s.Run(other.ID)
	if r.Status != "recovering" || other.Status != "unknown" {
		t.Fatal(r.Status, other.Status)
	}
	got, err := sch.claim(time.Now())
	if err != nil || got.SessionID != "remote-1" || got.Attempt != 1 {
		t.Fatal("recovery redispatched", got, err)
	}
	mustExec(t, s, `UPDATE runs SET status='succeeded'`)
	j.Policy.RetrySafe = true
	j.Policy.MaxAttempts = 2
	if err = s.SaveJob(j, false, "test"); err != nil {
		t.Fatal(err)
	}
	queued(t, s, j)
	got, err = sch.claim(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = sch.finish(got, Result{Status: "unknown", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	again, _ := s.Run(got.ID)
	if again.Status != "retry_wait" {
		t.Fatal(again.Status)
	}
	next, err := sch.claim(time.Now().Add(time.Minute))
	if err != nil || next.ID != got.ID || next.Attempt != 2 {
		t.Fatal("retry identity", next, err)
	}
	if err = sch.finish(next, Result{Status: "unknown", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	again, _ = s.Run(got.ID)
	if again.Status != "unknown" {
		t.Fatal("unbounded retries")
	}
}
func TestVaultTokensBackupAndRevocation(t *testing.T) {
	s, v, _ := fixture(t)
	cipher, _ := v.Seal("cred", "secret-value")
	if strings.Contains(cipher, "secret-value") {
		t.Fatal("plaintext")
	}
	if _, err := v.Open("different", cipher); err == nil {
		t.Fatal("credential substitution")
	}
	if err := s.AddUser("robot", "operator", "", true); err != nil {
		t.Fatal(err)
	}
	raw, _, err := s.NewToken("robot", "test", []string{"jobs:write", "jobs:read"}, []string{"test"}, time.Hour, "test")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.AuthenticateToken(raw)
	if err != nil || !p.Allows("jobs:write", "test") || p.Allows("jobs:write", "other") || p.Admin() {
		t.Fatal("scope enforcement", err)
	}
	if err = s.ChangeUser("robot", "role", "reader", "test"); err != nil {
		t.Fatal(err)
	}
	p, err = s.AuthenticateToken(raw)
	if err != nil || p.Allows("jobs:write", "test") {
		t.Fatal("role downgrade ignored", err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err = s.Backup(backup); err != nil {
		t.Fatal(err)
	}
	b, err := Open(backup, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.DB.Close()
	if _, err = b.AuthenticateToken(raw); err != nil {
		t.Fatal("backup not complete", err)
	}
	if err = s.ResetAccess(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateToken(raw); err == nil {
		t.Fatal("revoked token accepted")
	}
	v.Keys["v2"] = randomBytes(32)
	v.Active = "v2"
	if err = s.Reencrypt(v); err != nil {
		t.Fatal(err)
	}
	delete(v.Keys, "v1")
	if value, err := s.Secret(v, "cred"); err != nil || value != "test-private-secret" {
		t.Fatal("key rotation", err)
	}
}
func TestHTTPPermissionsAndCSRF(t *testing.T) {
	s, v, j := fixture(t)
	if err := s.AddUser("admin", "admin", "correct-password-123", false); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s, v)
	a := NewApp(s, v, e, NewScheduler(s, e, 2, 1), "https://cron.test", false)
	h := a.Handler()
	request := func(method, path, data, token string, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(data))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	login := request("POST", "/api/login", `{"username":"admin","password":"correct-password-123"}`, "", nil, "", "https://cron.test")
	if login.Code != 200 {
		t.Fatal(login.Code, login.Body.String())
	}
	var session struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(login.Body.Bytes(), &session)
	cookie := login.Result().Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly {
		t.Fatal("insecure cookie")
	}
	for _, pair := range [][2]string{{"", "https://cron.test"}, {session.CSRF, "https://evil.test"}} {
		w := request("POST", "/api/logout", "{}", "", cookie, pair[0], pair[1])
		if w.Code != 403 {
			t.Fatal("CSRF bypass", w.Code)
		}
	}
	raw, _, err := s.NewToken("admin", "limited", []string{"jobs:read"}, []string{"test"}, time.Hour, "test")
	if err != nil {
		t.Fatal(err)
	}
	if w := request("GET", "/api/jobs/"+j.ID, "", raw, nil, "", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("DELETE", "/api/jobs/"+j.ID, "", raw, nil, "", ""); w.Code != 403 {
		t.Fatal("write allowed", w.Code)
	}
	if w := request("GET", "/api/credentials", "", raw, nil, "", ""); w.Code != 403 {
		t.Fatal("token credential access", w.Code)
	}
	if w := request("GET", "/api/jobs", "", raw, cookie, "", ""); w.Code != 400 {
		t.Fatal("mixed auth", w.Code)
	}
	w := request("GET", "/api/credentials", "", "", cookie, "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "test-private-secret") || strings.Contains(w.Body.String(), "encrypted") {
		t.Fatal("secret disclosure", w.Body.String())
	}
}
func TestHTTPExecutionContract(t *testing.T) {
	s, v, j := fixture(t)
	var mode atomic.Int32
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Idempotency-Key") != "stable" || r.Header.Get("Authorization") != "Bearer test-private-secret" {
			t.Error("dispatch headers")
		}
		switch mode.Load() {
		case 0:
			io.WriteString(w, "ok")
		case 1:
			w.WriteHeader(202)
		case 2:
			w.Header().Set("Location", "/other")
			w.WriteHeader(302)
		case 3:
			w.Header().Set("Retry-After", "9999")
			w.WriteHeader(429)
		case 4:
			io.WriteString(w, strings.Repeat("x", outputLimit+10))
		}
	}))
	defer server.Close()
	e := NewExecutor(s, v)
	e.HTTP = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	j.Execution = Execution{Type: "http", Method: "POST", Path: "/work"}
	r := Run{ID: "stable", Snapshot: Snapshot{Job: j, Target: Target{URL: server.URL, CredentialID: "cred"}}}
	for _, tc := range []struct {
		mode   int32
		status string
		retry  bool
	}{{0, "succeeded", false}, {1, "unknown", false}, {2, "failed", false}, {3, "failed", true}, {4, "succeeded", false}} {
		mode.Store(tc.mode)
		before := requests.Load()
		result := e.executeHTTP(context.Background(), r)
		if result.Status != tc.status || result.Retryable != tc.retry || requests.Load() != before+1 {
			t.Fatalf("mode %d: %+v", tc.mode, result)
		}
		if tc.mode == 3 && result.RetryAfter > 5*time.Minute {
			t.Fatal("uncapped retry-after")
		}
		if tc.mode == 4 && (!result.Truncated || len(result.Stdout) != outputLimit) {
			t.Fatal("unbounded output")
		}
	}
}
func TestExecProtocolReconnectAndCancellation(t *testing.T) {
	for _, mode := range []string{"success", "reconnect", "missing-exit", "cancel", "shell-reconnect", "shell-cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, v, j := fixture(t)
			if strings.HasPrefix(mode, "shell-") {
				j.Execution = Execution{Type: "shell", Script: "echo hello"}
				mode = strings.TrimPrefix(mode, "shell-")
			}
			var starts, attaches, kills atomic.Int32
			up := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/kill") {
					kills.Add(1)
					fmt.Fprintln(w, `{"type":"signal_sent"}`)
					fmt.Fprintln(w, `{"type":"exited"}`)
					return
				}
				attaching := strings.HasSuffix(r.URL.Path, "/remote-1")
				if attaching {
					attaches.Add(1)
				} else {
					starts.Add(1)
					if r.URL.Query().Get("max_run_after_disconnect") != "60s" {
						t.Error("disconnect lifetime missing")
					}
				}
				c, err := up.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer c.Close()
				c.WriteJSON(map[string]string{"type": "session_info", "session_id": "remote-1"})
				if mode == "cancel" {
					for {
						if _, _, err = c.ReadMessage(); err != nil {
							return
						}
					}
				}
				if mode == "missing-exit" {
					return
				}
				if mode == "reconnect" && !attaching {
					return
				}
				c.WriteMessage(websocket.BinaryMessage, append([]byte{1}, []byte("hello")...))
				c.WriteMessage(websocket.BinaryMessage, []byte{3, 0})
			}))
			defer server.Close()
			e := NewExecutor(s, v)
			e.BaseURL = server.URL
			e.Dialer = &websocket.Dialer{}
			e.HTTP = server.Client()
			r := Run{ID: "stable", Snapshot: Snapshot{Job: j, Target: Target{Name: "test-sprite", CredentialID: "cred"}}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var saved string
			result := e.executeExec(ctx, r, func(id string) error {
				saved = id
				if mode == "cancel" {
					cancel()
				}
				return nil
			})
			expected := "succeeded"
			if mode == "missing-exit" {
				expected = "unknown"
			}
			if mode == "cancel" {
				expected = "cancelled"
			}
			if result.Status != expected || starts.Load() != 1 || saved != "remote-1" {
				t.Fatalf("%+v starts=%d session=%s", result, starts.Load(), saved)
			}
			if mode == "reconnect" && attaches.Load() != 1 {
				t.Fatal("did not attach")
			}
			if mode == "cancel" && kills.Load() != 1 {
				t.Fatal("did not kill remote")
			}
		})
	}
}

func TestOfflineRestore(t *testing.T) {
	s, _, _ := fixture(t)
	if err := s.AddUser("restore-user", "operator", "", true); err != nil {
		t.Fatal(err)
	}
	raw, _, err := s.NewToken("restore-user", "restore-test", []string{"jobs:read"}, []string{"test"}, time.Hour, "test")
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err = s.Backup(backup); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "restored.db")
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err = Restore(path, backup); err == nil {
		t.Fatal("restore ignored daemon lock")
	}
	lock.Close()
	if err = Restore(path, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.DB.Close()
	users, err := restored.Users()
	if err != nil || len(users) != 1 {
		t.Fatal("restore lost users", err)
	}
	if _, err = restored.AuthenticateToken(raw); err == nil {
		t.Fatal("restore resurrected API token")
	}
	jobs, err := restored.Jobs()
	if err != nil || len(jobs) != 1 {
		t.Fatal("restore lost jobs", err)
	}
}

func TestCancelledRetryPreservesUncertainty(t *testing.T) {
	for _, operation := range []string{"cancel", "edit"} {
		t.Run(operation, func(t *testing.T) {
			s, _, j := fixture(t)
			j.Policy.RetrySafe = true
			j.Policy.MaxAttempts = 2
			if err := s.SaveJob(j, false, "test"); err != nil {
				t.Fatal(err)
			}
			j, _ = jobFrom(s.DB, j.ID)
			r := queued(t, s, j)
			mustExec(t, s, `UPDATE runs SET manual=0 WHERE id=?`, r.ID)
			sch := NewScheduler(s, nil, 2, 1)
			r, err := sch.claim(time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err = sch.finish(r, Result{Status: "unknown", Retryable: true}); err != nil {
				t.Fatal(err)
			}
			if operation == "edit" {
				j.Enabled = false
				err = s.SaveJob(j, false, "test")
			} else {
				mustExec(t, s, `UPDATE runs SET cancel=1,next_attempt=0 WHERE id=?`, r.ID)
				_, err = sch.claim(time.Now())
				if errors.Is(err, sql.ErrNoRows) {
					err = nil
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			r, _ = s.Run(r.ID)
			if r.Status != "unknown" {
				t.Fatalf("lost uncertain remote outcome: %s", r.Status)
			}
		})
	}
}

func TestTokenTargetProvisioning(t *testing.T) {
	s, v, _ := fixture(t)
	for _, role := range []string{"admin", "operator", "reader"} {
		if err := s.AddUser(role, role, "", true); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []string{"operator", "reader"} {
		if _, _, err := s.NewToken(role, "provision", []string{"targets:write"}, []string{"future"}, time.Hour, "test"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s received provisioning scope: %v", role, err)
		}
	}
	token, _, err := s.NewToken("admin", "provision", []string{"targets:write", "targets:read", "credentials:write"}, []string{"future"}, time.Hour, "test")
	if err != nil {
		t.Fatal(err)
	}
	readOnly, _, err := s.NewToken("admin", "reader", []string{"targets:read"}, []string{"test"}, time.Hour, "test")
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-private-secret" {
			t.Error("metadata credential missing")
		}
		respond(w, 200, map[string]any{"id": "verified-id", "name": "test-sprite", "url": "https://verified.sprites.app", "organization": "verified-org", "url_settings": map[string]string{"auth": "sprite"}})
	}))
	defer remote.Close()
	e := NewExecutor(s, v)
	e.BaseURL = remote.URL
	e.HTTP = remote.Client()
	handler := NewApp(s, v, e, NewScheduler(s, e, 2, 1), "https://cron.test", false).Handler()
	call := func(method, path, raw string, target Target) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(encoded(target)))
		r.Header.Set("Authorization", "Bearer "+raw)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	credentialRequest := httptest.NewRequest("POST", "/api/credentials", strings.NewReader(`{"id":"api-created","label":"Automation","kind":"sprite","value":"test-private-secret"}`))
	credentialRequest.Header.Set("Authorization", "Bearer "+token)
	credentialResponse := httptest.NewRecorder()
	handler.ServeHTTP(credentialResponse, credentialRequest)
	if credentialResponse.Code != 201 {
		t.Fatal("credential bootstrap failed", credentialResponse.Code)
	}
	target := Target{ID: "future", Name: "test-sprite", CredentialID: "api-created", URL: "https://untrusted.invalid", SpriteID: "spoofed"}
	if w := call("POST", "/api/targets", readOnly, target); w.Code != 403 {
		t.Fatal("read-only token created target", w.Code)
	}
	outside := target
	outside.ID = "outside"
	if w := call("POST", "/api/targets", token, outside); w.Code != 403 {
		t.Fatal("allowlist bypass", w.Code)
	}
	if requests.Load() != 0 {
		t.Fatal("unauthorized request reached Sprite API")
	}
	if w := call("POST", "/api/targets", token, target); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	saved, err := targetFrom(s.DB, "future")
	if err != nil || saved.SpriteID != "verified-id" || saved.URL != "https://verified.sprites.app" {
		t.Fatal("unverified metadata persisted", saved, err)
	}
	if w := call("POST", "/api/targets", token, target); w.Code != 409 {
		t.Fatal("duplicate target not rejected", w.Code)
	}
	if w := call("PUT", "/api/targets/future", token, target); w.Code != 200 {
		t.Fatal("target update failed", w.Code, w.Body.String())
	}
	if w := call("PUT", "/api/targets/test", token, target); w.Code != 403 {
		t.Fatal("update used body ID instead of path allowlist", w.Code)
	}
	if w := call("GET", "/api/credentials", token, Target{}); w.Code != 403 {
		t.Fatal("provisioner could read credentials", w.Code)
	}
	var events int
	if err = s.DB.QueryRow(`SELECT count(*) FROM audit WHERE action='target.save' AND object='future'`).Scan(&events); err != nil || events != 2 {
		t.Fatal("missing audit", events, err)
	}
	if err = s.ChangeUser("admin", "role", "operator", "test"); err != nil {
		t.Fatal(err)
	}
	before := requests.Load()
	if w := call("PUT", "/api/targets/future", token, target); w.Code != 403 {
		t.Fatal("downgraded owner retained write access", w.Code)
	}
	if requests.Load() != before {
		t.Fatal("downgraded request reached upstream")
	}
}

func TestTokenCredentialProvisioning(t *testing.T) {
	s, v, j := fixture(t)
	for _, role := range []string{"admin", "operator", "reader"} {
		if err := s.AddUser(role, role, "", true); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []string{"operator", "reader"} {
		if _, _, err := s.NewToken(role, "provision", []string{"credentials:write"}, []string{"test"}, time.Hour, "test"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s received credential scope: %v", role, err)
		}
	}
	token, info, err := s.NewToken("admin", "provision", []string{"credentials:write"}, []string{"test"}, time.Hour, "test")
	if err != nil {
		t.Fatal(err)
	}
	ordinary, _, err := s.NewToken("admin", "jobs", []string{"jobs:write", "targets:write"}, []string{"test"}, time.Hour, "test")
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s, v)
	handler := NewApp(s, v, e, NewScheduler(s, e, 2, 1), "https://cron.test", false).Handler()
	call := func(method, path, raw, secret string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(encoded(map[string]string{"id": "provisioned", "label": "Automation", "kind": "sprite", "value": secret})))
		r.Header.Set("Authorization", "Bearer "+raw)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("secret in API response")
		}
		return w
	}
	if w := call("POST", "/api/credentials", ordinary, "initial-secret"); w.Code != 403 {
		t.Fatal("ordinary token wrote credential", w.Code)
	}
	if w := call("POST", "/api/credentials", token, "initial-secret"); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertSecret := func(want string, version int) {
		t.Helper()
		var cipher string
		var gotVersion int
		if err := s.DB.QueryRow(`SELECT encrypted,version FROM credentials WHERE id='provisioned'`).Scan(&cipher, &gotVersion); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(cipher, want) {
			t.Fatal("plaintext storage")
		}
		plain, err := v.Open("provisioned", cipher)
		if err != nil || plain != want || gotVersion != version {
			t.Fatal("credential mismatch", err, gotVersion)
		}
	}
	assertSecret("initial-secret", 1)
	if w := call("POST", "/api/credentials", token, "replacement-secret"); w.Code != 409 {
		t.Fatal("duplicate create overwrote credential", w.Code)
	}
	assertSecret("initial-secret", 1)
	r := queued(t, s, j)
	for _, state := range []string{"running", "dispatching", "recovering"} {
		mustExec(t, s, `UPDATE runs SET status=? WHERE id=?`, state, r.ID)
		if w := call("PUT", "/api/credentials/provisioned", token, "replacement-secret"); w.Code != 409 {
			t.Fatalf("rotated while %s: %d", state, w.Code)
		}
		assertSecret("initial-secret", 1)
	}
	mustExec(t, s, `UPDATE runs SET status='succeeded' WHERE id=?`, r.ID)
	if w := call("PUT", "/api/credentials/provisioned", token, "replacement-secret"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertSecret("replacement-secret", 2)
	if w := call("PUT", "/api/credentials/missing", token, "replacement-secret"); w.Code != 404 {
		t.Fatal("missing update", w.Code)
	}
	if w := call("GET", "/api/credentials", token, "replacement-secret"); w.Code != 403 {
		t.Fatal("write scope allowed listing", w.Code)
	}
	var count int
	if err = s.DB.QueryRow(`SELECT count(*) FROM audit WHERE action='credential.save' AND object='provisioned' AND actor=? AND detail=''`, "admin/token:"+info.ID).Scan(&count); err != nil || count != 2 {
		t.Fatal("audit does not identify token without secret", count, err)
	}
	if err = s.ChangeUser("admin", "role", "operator", "test"); err != nil {
		t.Fatal(err)
	}
	if w := call("PUT", "/api/credentials/provisioned", token, "forbidden-secret"); w.Code != 403 {
		t.Fatal("downgraded owner retained access", w.Code)
	}
	if err = s.ChangeUser("admin", "role", "admin", "test"); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeToken(info.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if w := call("PUT", "/api/credentials/provisioned", token, "forbidden-secret"); w.Code != 401 {
		t.Fatal("revoked token retained access", w.Code)
	}
	assertSecret("replacement-secret", 2)
}

func TestShellExecution(t *testing.T) {
	for _, shell := range []string{"/bin/sh", "/bin/bash"} {
		t.Run(shell, func(t *testing.T) {
			s, v, j := fixture(t)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("foo first\nbar ignored\nfoo second\n"), 0600); err != nil {
				t.Fatal(err)
			}
			script := "echo \"Hello World \" `date +%Y` > output.txt\ngrep foo input.txt | cut -d \" \" -f 2\nprintf '%s' \"$GREETING\"\ncat output.txt"
			if shell == "/bin/bash" {
				script = "shopt -q login_shell || exit 40\nset -o pipefail\nvalues=(one two)\n[[ ${values[1]} == two ]] || exit 42\n" + script
			}
			j.Execution = Execution{Type: "shell", Shell: shell, Script: script, Directory: dir, Env: map[string]string{"GREETING": "literal $(echo should-not-expand)\n"}}
			if err := s.SaveJob(j, false, "test"); err != nil {
				t.Fatal(err)
			}
			r := queued(t, s, j)
			if r.Snapshot.Job.Execution.Script != script {
				t.Fatal("script snapshot changed")
			}
			up := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				argv := req.URL.Query()["cmd"]
				if len(argv) != 3 || argv[0] != shell || argv[1] != "-lc" || argv[2] != script {
					t.Error("script must remain one unmodified argument")
					w.WriteHeader(400)
					return
				}
				c, err := up.Upgrade(w, req, nil)
				if err != nil {
					return
				}
				defer c.Close()
				c.WriteJSON(map[string]string{"type": "session_info", "session_id": "shell-1"})
				command := exec.Command(argv[0], argv[1:]...)
				command.Dir = req.URL.Query().Get("dir")
				command.Env = append(os.Environ(), req.URL.Query()["env"]...)
				output, err := command.CombinedOutput()
				code := 0
				if err != nil {
					code = 1
				}
				c.WriteMessage(websocket.BinaryMessage, append([]byte{1}, output...))
				c.WriteMessage(websocket.BinaryMessage, []byte{3, byte(code)})
			}))
			defer server.Close()
			e := NewExecutor(s, v)
			e.BaseURL = server.URL
			e.Dialer = &websocket.Dialer{}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result := e.executeExec(ctx, r, func(string) error { return nil })
			expected := "first\nsecond\nliteral $(echo should-not-expand)\nHello World  " + time.Now().Format("2006") + "\n"
			if result.Status != "succeeded" || result.Stdout != expected {
				t.Fatalf("shell result: %+v, want %q", result, expected)
			}
			// Interrupted shell jobs must reattach to the existing session after restart.
			mustExec(t, s, `UPDATE runs SET status='running',session_id='shell-1',attempt=1,deadline=? WHERE id=?`, nowMS()+10000, r.ID)
			sch := NewScheduler(s, e, 2, 1)
			if err := sch.Recover(); err != nil {
				t.Fatal(err)
			}
			recovered, err := sch.claim(time.Now())
			if err != nil || recovered.SessionID != "shell-1" || recovered.Attempt != 1 {
				t.Fatal("shell recovery lost session", recovered, err)
			}
		})
	}
}

func TestShellValidation(t *testing.T) {
	base := Job{Name: "shell", TargetID: "test", Schedule: "* * * * *"}
	base.Execution = Execution{Type: "shell", Script: "echo ok"}
	if err := base.Validate(); err != nil || base.Execution.Shell != "/bin/bash" {
		t.Fatal("shell default", err)
	}
	for _, cfg := range []Execution{
		{Type: "shell", Script: "  \n"}, {Type: "shell", Script: "echo\x00"},
		{Type: "shell", Script: "echo ok", Shell: "/custom/shell"},
		{Type: "shell", Script: "echo ok", Argv: []string{"true"}},
		{Type: "shell", Script: strings.Repeat("x", 65536)},
		{Type: "shell", Script: "echo ok", Env: map[string]string{"SPRITE_CRON_RUN_ID": "spoofed"}},
		{Type: "exec", Argv: []string{"true"}, Script: "echo ignored"},
	} {
		j := base
		j.Execution = cfg
		if err := j.Validate(); err == nil {
			t.Fatalf("accepted invalid execution: type=%s shell=%s", cfg.Type, cfg.Shell)
		}
	}
}

func TestTargetOptionalApplicationAuthentication(t *testing.T) {
	s, v, _ := fixture(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"id": "sprite-id", "name": "test-sprite", "url": "https://test.sprites.app", "url_settings": map[string]string{"auth": "public"}})
	}))
	defer remote.Close()
	e := NewExecutor(s, v)
	e.BaseURL, e.HTTP = remote.URL, remote.Client()
	target := Target{ID: "test", Name: "test-sprite", CredentialID: "cred", Public: true}
	if _, err := e.ValidateTarget(context.Background(), target); err != nil {
		t.Fatal("public target without application authentication", err)
	}
	for _, header := range []string{"Authorization", "Cookie", "Host", "Fly-Test", "X-Forwarded-For", "Idempotency-Key", "X-Sprite-Cron-Run-ID", "Content-Length", "Transfer-Encoding", "Connection"} {
		target.AppHeader, target.AppCredentialID = header, "cred"
		if _, err := e.ValidateTarget(context.Background(), target); err != nil {
			t.Errorf("header %q: %v", header, err)
		}
	}
	for _, tc := range []struct{ header, credential string }{{"Bad Header", "cred"}, {"X-Test\r\nInjected", "cred"}, {"", "cred"}, {"X-Test", ""}} {
		target.AppHeader, target.AppCredentialID = tc.header, tc.credential
		if _, err := e.ValidateTarget(context.Background(), target); err == nil {
			t.Errorf("accepted invalid header pair %q/%q", tc.header, tc.credential)
		}
	}
}

func TestApplicationHeaderDelivery(t *testing.T) {
	s, v, j := fixture(t)
	r := queued(t, s, j)
	r.Snapshot.Job.Execution = Execution{Type: "http", Method: "GET", Path: "/ping"}
	for _, header := range []string{"", "Authorization", "Cookie", "Host", "Fly-Test", "X-Forwarded-For", "Idempotency-Key", "X-Sprite-Cron-Run-ID"} {
		t.Run(header, func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if header == "" {
					if req.Header.Get("Authorization") != "" {
						t.Error("public ping sent Sprite credential")
					}
				} else {
					got := req.Header.Get(header)
					if header == "Host" {
						got = req.Host
					}
					if got != "test-private-secret" {
						t.Errorf("application header was not delivered: %q", header)
					}
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer remote.Close()
			e := NewExecutor(s, v)
			e.HTTP = remote.Client()
			r.Snapshot.Target.URL = remote.URL
			r.Snapshot.Target.Public = header != "Authorization"
			r.Snapshot.Target.AppHeader, r.Snapshot.Target.AppCredentialID = header, ""
			if header != "" {
				r.Snapshot.Target.AppCredentialID = "cred"
			}
			if result := e.executeHTTP(context.Background(), r); result.Status != "succeeded" {
				t.Fatal(result)
			}
		})
	}
}
