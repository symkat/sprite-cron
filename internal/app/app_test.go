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
	for _, mode := range []string{"success", "reconnect", "missing-exit", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, v, j := fixture(t)
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
