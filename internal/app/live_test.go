package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in only: these tests execute real commands on the named Sprite.
// The token is read from a protected file, never included in test output.
func TestLiveSprite(t *testing.T) {
	path := os.Getenv("SPRITE_CRON_TEST_TOKEN_FILE")
	if path == "" {
		t.Skip("set SPRITE_CRON_TEST_TOKEN_FILE and SPRITE_CRON_TEST_SPRITE to opt in")
	}
	name := os.Getenv("SPRITE_CRON_TEST_SPRITE")
	if name == "" {
		t.Fatal("SPRITE_CRON_TEST_SPRITE required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read token file")
	}
	s, v, _ := fixture(t)
	sealed, err := v.Seal("cred", strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal("seal credential")
	}
	mustExec(t, s, `UPDATE credentials SET encrypted=? WHERE id='cred'`, sealed)
	e := NewExecutor(s, v)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	target, err := e.ValidateTarget(ctx, Target{ID: "live", Name: name, CredentialID: "cred"})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `INSERT INTO targets(id,data) VALUES(?,?)`, target.ID, encoded(target))
	if err = s.AddUser("live-robot", "operator", "", true); err != nil {
		t.Fatal(err)
	}
	token, _, err := s.NewToken("live-robot", "integration", []string{"jobs:read", "jobs:write", "runs:trigger", "runs:read", "runs:cancel"}, []string{"live"}, time.Hour, "test")
	if err != nil {
		t.Fatal(err)
	}
	sch := NewScheduler(s, e, 2, 2)
	a := NewApp(s, v, e, sch, "https://unused.test", false)
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	request := func(method, path, body string) map[string]json.RawMessage {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", randomID())
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal("local request failed")
		}
		defer resp.Body.Close()
		var out map[string]json.RawMessage
		if json.NewDecoder(resp.Body).Decode(&out) != nil || resp.StatusCode >= 300 {
			t.Fatalf("API %s: status %d", path, resp.StatusCode)
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		exec     Execution
		timeout  int
		expected string
	}{{"exec", Execution{Type: "exec", Argv: []string{"/bin/printf", "sprite-cron-live-ok"}}, 20, "succeeded"}, {"http", Execution{Type: "http", Method: "GET", Path: "/health"}, 30, "succeeded"}, {"timeout", Execution{Type: "exec", Argv: []string{"/bin/sleep", "20"}}, 3, "timed_out"}} {
		t.Run(tc.name, func(t *testing.T) {
			data := request("POST", "/api/jobs", encoded(Job{Name: "live-" + tc.name, TargetID: "live", Schedule: "0 0 * * *", Timezone: "UTC", Execution: tc.exec, Policy: Policy{TimeoutSeconds: tc.timeout}}))
			var jobID string
			json.Unmarshal(data["id"], &jobID)
			data = request("POST", "/api/jobs/"+jobID+"/runs", "")
			var id string
			json.Unmarshal(data["id"], &id)
			r, err := sch.claim(time.Now())
			if err != nil || r.ID != id {
				t.Fatal("claim failed", err)
			}
			sch.Launch(ctx, r)
			sch.wg.Wait()
			result, err := s.Run(id)
			if err != nil || result.Status != tc.expected {
				t.Fatalf("got %s (%s), want %s", result.Status, result.Reason, tc.expected)
			}
			if tc.name == "exec" && result.Stdout != "sprite-cron-live-ok" {
				t.Fatal("unexpected exec output")
			}
			if tc.name == "http" && !strings.Contains(result.Stdout, `"ok"`) {
				t.Fatal("unexpected HTTP response")
			}
			data = request("GET", "/api/runs/"+id, "")
			if data["run"] == nil {
				t.Fatal("missing API result")
			}
			t.Logf("%s: %s (attempt %d, remote session recorded: %t)", tc.name, result.Status, result.Attempt, result.SessionID != "")
		})
	}
}
