package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`)
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
var ErrConflict = errors.New("resource changed or operation conflicts with current state")
var ErrForbidden = errors.New("not authorized")

func nowMS() int64         { return time.Now().UTC().UnixMilli() }
func encoded(v any) string { b, _ := json.Marshal(v); return string(b) }
func schedule(spec, zone string) (cron.Schedule, error) {
	if len(strings.Fields(spec)) != 5 || strings.Contains(spec, "TZ=") {
		return nil, errors.New("schedule must contain exactly five cron fields")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, errors.New("invalid IANA time zone")
	}
	s, err := parser.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("invalid schedule: %w", err)
	}
	s.(*cron.SpecSchedule).Location = loc
	if s.Next(time.Now()).IsZero() {
		return nil, errors.New("schedule has no next occurrence within five years")
	}
	return s, nil
}

type Policy struct {
	TimeoutSeconds    int    `json:"timeout_seconds"`
	StartGraceSeconds int    `json:"start_grace_seconds"`
	Overlap           string `json:"overlap"`
	Misfire           string `json:"misfire"`
	MaxAttempts       int    `json:"max_attempts"`
	RetrySafe         bool   `json:"retry_safe"`
}
type Execution struct {
	Type            string            `json:"type"`
	Argv            []string          `json:"argv,omitempty"`
	Directory       string            `json:"directory,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Method          string            `json:"method,omitempty"`
	Path            string            `json:"path,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Body            string            `json:"body,omitempty"`
	SuccessStatuses []int             `json:"success_statuses,omitempty"`
}
type Job struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TargetID  string    `json:"target_id"`
	Schedule  string    `json:"schedule"`
	Timezone  string    `json:"timezone"`
	Enabled   bool      `json:"enabled"`
	Execution Execution `json:"execution"`
	Policy    Policy    `json:"policy"`
	Revision  int       `json:"revision"`
	NextRun   int64     `json:"next_run_at"`
}

func (j *Job) Validate() error {
	if len(j.Name) < 1 || len(j.Name) > 120 {
		return errors.New("name must contain 1–120 characters")
	}
	if !identifier.MatchString(j.TargetID) {
		return errors.New("invalid target_id")
	}
	if j.Timezone == "" {
		j.Timezone = "UTC"
	}
	if _, err := schedule(j.Schedule, j.Timezone); err != nil {
		return err
	}
	p := &j.Policy
	if p.TimeoutSeconds == 0 {
		p.TimeoutSeconds = 900
	}
	if p.StartGraceSeconds == 0 {
		p.StartGraceSeconds = 60
	}
	if p.Overlap == "" {
		p.Overlap = "forbid"
	}
	if p.Misfire == "" {
		p.Misfire = "skip"
	}
	if p.MaxAttempts == 0 {
		p.MaxAttempts = 1
	}
	if p.TimeoutSeconds < 1 || p.TimeoutSeconds > 86400 || p.StartGraceSeconds < 1 || p.StartGraceSeconds > 3600 {
		return errors.New("timeout must be 1–86400 seconds and grace 1–3600 seconds")
	}
	if p.Overlap != "forbid" && p.Overlap != "allow" {
		return errors.New("overlap must be forbid or allow")
	}
	if p.Misfire != "skip" && p.Misfire != "coalesce" {
		return errors.New("misfire must be skip or coalesce")
	}
	if p.MaxAttempts < 1 || p.MaxAttempts > 5 || p.MaxAttempts > 1 && !p.RetrySafe {
		return errors.New("max_attempts must be 1–5; retries require retry_safe")
	}
	e := &j.Execution
	switch e.Type {
	case "exec":
		if len(e.Argv) == 0 || len(e.Argv) > 128 || e.Argv[0] == "" {
			return errors.New("exec requires argv (1–128 entries)")
		}
		if len(encoded(e)) > 65536 {
			return errors.New("exec configuration exceeds 64 KiB")
		}
		for _, arg := range e.Argv {
			if strings.ContainsRune(arg, 0) {
				return errors.New("argv contains NUL")
			}
		}
		for key, value := range e.Env {
			if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(key) || strings.HasPrefix(key, "SPRITE_CRON_") || strings.ContainsRune(value, 0) {
				return errors.New("invalid or reserved environment name/value")
			}
		}
	case "http":
		if e.Method == "" {
			e.Method = "POST"
		}
		if e.Method != "GET" && e.Method != "POST" && e.Method != "PUT" && e.Method != "PATCH" && e.Method != "DELETE" {
			return errors.New("unsupported HTTP method")
		}
		u, err := url.Parse(e.Path)
		if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || !strings.HasPrefix(e.Path, "/") || strings.HasPrefix(e.Path, "//") || u.Fragment != "" || strings.ContainsAny(e.Path, "\\\r\n") {
			return errors.New("HTTP path must be a relative path starting with a single slash")
		}
		if len(e.Body) > 65536 || len(encoded(e.Headers)) > 8192 {
			return errors.New("HTTP body/headers too large")
		}
		for k, v := range e.Headers {
			lower := strings.ToLower(k)
			if !validHeader(k) || reservedHeader(lower) || strings.ContainsAny(v, "\r\n") {
				return errors.New("invalid or reserved HTTP header")
			}
		}
		for _, code := range e.SuccessStatuses {
			if code < 200 || code > 299 || code == 202 {
				return errors.New("success statuses must be 2xx, excluding 202")
			}
		}
	default:
		return errors.New("execution.type must be exec or http")
	}
	return nil
}
func validHeader(k string) bool {
	return regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`).MatchString(k)
}
func reservedHeader(k string) bool {
	return k == "authorization" || k == "host" || k == "cookie" || k == "connection" || k == "content-length" || k == "transfer-encoding" || k == "idempotency-key" || strings.HasPrefix(k, "proxy-") || strings.HasPrefix(k, "fly-") || strings.HasPrefix(k, "x-fly-") || strings.HasPrefix(k, "x-sprite") || strings.HasPrefix(k, "forwarded") || strings.HasPrefix(k, "x-forwarded-")
}

type Target struct {
	ID               string `json:"id"`
	Name             string `json:"sprite_name"`
	SpriteID         string `json:"sprite_id"`
	Organization     string `json:"organization"`
	URL              string `json:"url"`
	CredentialID     string `json:"credential_id"`
	HTTPCredentialID string `json:"http_credential_id,omitempty"`
	AppCredentialID  string `json:"app_credential_id,omitempty"`
	AppHeader        string `json:"app_header,omitempty"`
	Public           bool   `json:"public"`
}
type Snapshot struct {
	Job    Job    `json:"job"`
	Target Target `json:"target"`
}
type Run struct {
	ID          string   `json:"id"`
	JobID       string   `json:"job_id"`
	TargetID    string   `json:"target_id"`
	Scheduled   int64    `json:"scheduled_at"`
	Created     int64    `json:"created_at"`
	Deadline    int64    `json:"deadline"`
	StartBefore int64    `json:"start_before"`
	Status      string   `json:"status"`
	Reason      string   `json:"reason"`
	SessionID   string   `json:"session_id,omitempty"`
	Attempt     int      `json:"attempt"`
	NextAttempt int64    `json:"next_attempt_at"`
	Cancel      bool     `json:"cancel_requested"`
	Resolved    bool     `json:"resolved"`
	Manual      bool     `json:"manual"`
	Stdout      string   `json:"stdout,omitempty"`
	Stderr      string   `json:"stderr,omitempty"`
	Truncated   bool     `json:"truncated"`
	ExitCode    *int     `json:"exit_code,omitempty"`
	HTTPStatus  int      `json:"http_status,omitempty"`
	Snapshot    Snapshot `json:"-"`
}

func isMutation(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}
