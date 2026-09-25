package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const outputLimit = 1024 * 1024
const spriteAPI = "https://api.sprites.dev"

type Result struct {
	Status, Reason, Stdout, Stderr string
	ExitCode                       *int
	HTTPStatus                     int
	Truncated, Retryable           bool
	RetryAfter                     time.Duration
}
type Runner interface {
	Execute(context.Context, Run, func(string) error) Result
	Terminate(context.Context, Target, string) bool
}
type Executor struct {
	Store   *Store
	Vault   *Vault
	HTTP    *http.Client
	Dialer  *websocket.Dialer
	BaseURL string
}

func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, e
	}
	addrs, e := net.DefaultResolver.LookupIPAddr(ctx, host)
	if e != nil {
		return nil, e
	}
	for _, a := range addrs {
		ip := a.IP
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return nil, errors.New("non-public destination blocked")
		}
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	for _, a := range addrs {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("destination connection failed")
}
func NewExecutor(s *Store, v *Vault) *Executor {
	transport := &http.Transport{DialContext: publicDial, ForceAttemptHTTP2: true, MaxIdleConns: 32, MaxIdleConnsPerHost: 10, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
	return &Executor{Store: s, Vault: v, BaseURL: spriteAPI, HTTP: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Dialer: &websocket.Dialer{NetDialContext: publicDial, HandshakeTimeout: 20 * time.Second}}
}
func (e *Executor) secret(id string) (string, error) { return e.Store.Secret(e.Vault, id) }
func (e *Executor) request(ctx context.Context, method, path, token string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, e.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return e.HTTP.Do(req)
}
func (e *Executor) ValidateTarget(ctx context.Context, t Target) (Target, error) {
	if !identifier.MatchString(t.ID) || !identifier.MatchString(t.Name) {
		return t, errors.New("target ID and Sprite name must be simple names")
	}
	token, err := e.secret(t.CredentialID)
	if err != nil {
		return t, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := e.request(ctx, "GET", "/v1/sprites/"+url.PathEscape(t.Name), token, nil)
	if err != nil {
		return t, errors.New("Sprite metadata request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return t, fmt.Errorf("Sprite metadata returned HTTP %d", resp.StatusCode)
	}
	var info struct {
		ID, Name, Organization, URL string
		Settings                    struct {
			Auth string `json:"auth"`
		} `json:"url_settings"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&info) != nil {
		return t, errors.New("invalid Sprite metadata")
	}
	origin, err := url.Parse(info.URL)
	if err != nil || origin.Scheme != "https" || origin.User != nil || origin.Port() != "" || !strings.HasSuffix(origin.Hostname(), ".sprites.app") || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") || info.ID == "" || info.Name != t.Name {
		return t, errors.New("untrusted Sprite identity or URL")
	}
	if t.SpriteID != "" && t.SpriteID != info.ID {
		return t, errors.New("Sprite identity changed; register a new target")
	}
	if info.Settings.Auth != "sprite" && info.Settings.Auth != "public" {
		return t, errors.New("unknown Sprite URL authentication mode")
	}
	if t.Public != (info.Settings.Auth == "public") {
		return t, errors.New("target public setting does not match Sprite URL authentication")
	}
	if t.Public && t.AppCredentialID == "" {
		return t, errors.New("public targets require an application credential")
	}
	if t.AppCredentialID != "" && (!validHeader(t.AppHeader) || reservedHeader(strings.ToLower(t.AppHeader))) {
		return t, errors.New("application credential requires a non-reserved HTTP header")
	}
	for _, id := range []string{t.HTTPCredentialID, t.AppCredentialID} {
		if id != "" {
			if _, err = e.secret(id); err != nil {
				return t, err
			}
		}
	}
	t.SpriteID = info.ID
	t.Organization = info.Organization
	t.URL = strings.TrimSuffix(info.URL, "/")
	return t, nil
}
func (e *Executor) Execute(ctx context.Context, r Run, onSession func(string) error) Result {
	if r.SessionID != "" && ctx.Err() != nil {
		return e.executeExec(ctx, r, onSession)
	}
	preflightFailure := func(reason string) Result {
		status := "failed"
		if r.SessionID != "" {
			status = "unknown"
		}
		return Result{Status: status, Reason: reason}
	}
	target, err := e.ValidateTarget(ctx, r.Snapshot.Target)
	if err != nil {
		return preflightFailure(err.Error())
	}
	if target.URL != r.Snapshot.Target.URL {
		return preflightFailure("Sprite URL changed; revalidate target")
	}
	secrets := []string{}
	for _, id := range []string{target.CredentialID, target.HTTPCredentialID, target.AppCredentialID} {
		if id != "" {
			secret, err := e.secret(id)
			if err != nil {
				return preflightFailure("credential unavailable")
			}
			secrets = append(secrets, secret)
		}
	}
	var result Result
	if r.Snapshot.Job.Execution.Type == "http" {
		result = e.executeHTTP(ctx, r)
	} else {
		result = e.executeExec(ctx, r, onSession)
	}
	for _, secret := range secrets {
		if secret != "" {
			result.Stdout = strings.ReplaceAll(result.Stdout, secret, "[redacted]")
			result.Stderr = strings.ReplaceAll(result.Stderr, secret, "[redacted]")
			result.Reason = strings.ReplaceAll(result.Reason, secret, "[redacted]")
		}
	}
	return result
}
func (e *Executor) executeHTTP(ctx context.Context, r Run) Result {
	t := r.Snapshot.Target
	cfg := r.Snapshot.Job.Execution
	req, err := http.NewRequestWithContext(ctx, cfg.Method, t.URL+cfg.Path, strings.NewReader(cfg.Body))
	if err != nil {
		return Result{Status: "failed", Reason: "invalid HTTP request"}
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	if !t.Public {
		id := t.HTTPCredentialID
		if id == "" {
			id = t.CredentialID
		}
		secret, err := e.secret(id)
		if err != nil {
			return Result{Status: "failed", Reason: "HTTP credential unavailable"}
		}
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if t.AppCredentialID != "" {
		secret, err := e.secret(t.AppCredentialID)
		if err != nil {
			return Result{Status: "failed", Reason: "application credential unavailable"}
		}
		req.Header.Set(t.AppHeader, secret)
	}
	req.Header.Set("Idempotency-Key", r.ID)
	req.Header.Set("X-Sprite-Cron-Run-ID", r.ID)
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return Result{Status: "unknown", Reason: "HTTP transport interrupted; request may have executed", Retryable: true}
	}
	defer resp.Body.Close()
	b, readErr := io.ReadAll(io.LimitReader(resp.Body, outputLimit+1))
	truncated := len(b) > outputLimit
	if truncated {
		b = b[:outputLimit]
		// Observe the complete response even after the capture budget is exhausted.
		// A broken or timed-out tail is not a completed HTTP job.
		_, readErr = io.Copy(io.Discard, resp.Body)
	}
	result := Result{HTTPStatus: resp.StatusCode, Stdout: string(b), Truncated: truncated}
	if readErr != nil {
		result.Status = "unknown"
		result.Reason = "HTTP response interrupted"
		result.Retryable = true
		return result
	}
	if resp.StatusCode == 202 {
		result.Status = "unknown"
		result.Reason = "HTTP 202 accepted; asynchronous completion is unsupported"
		return result
	}
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	if len(cfg.SuccessStatuses) > 0 {
		success = false
		for _, status := range cfg.SuccessStatuses {
			success = success || status == resp.StatusCode
		}
	}
	if success {
		result.Status = "succeeded"
		return result
	}
	result.Status = "failed"
	result.Reason = fmt.Sprintf("HTTP %d", resp.StatusCode)
	result.Retryable = resp.StatusCode == 429 || resp.StatusCode >= 500
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
		result.RetryAfter = time.Duration(seconds) * time.Second
	} else if until, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
		result.RetryAfter = time.Until(until)
	}
	if result.RetryAfter > 5*time.Minute {
		result.RetryAfter = 5 * time.Minute
	}
	return result
}

type capture struct {
	stdout, stderr strings.Builder
	size           int
	truncated      bool
}

func (c *capture) append(stream byte, b []byte) {
	keep := len(b)
	if c.size+keep > outputLimit {
		keep = outputLimit - c.size
		c.truncated = true
	}
	if keep <= 0 {
		return
	}
	if stream == 2 {
		c.stderr.Write(b[:keep])
	} else {
		c.stdout.Write(b[:keep])
	}
	c.size += keep
}
func (e *Executor) executeExec(ctx context.Context, r Run, onSession func(string) error) Result {
	secret, err := e.secret(r.Snapshot.Target.CredentialID)
	if err != nil {
		status := "failed"
		if r.SessionID != "" {
			status = "unknown"
		}
		return Result{Status: status, Reason: "exec credential unavailable"}
	}
	var cap capture
	id := r.SessionID
	reconnects := 0
	replayed := id != ""
	finish := func(status, reason string, code *int) Result {
		if replayed {
			reason = strings.TrimSpace(reason + "; reattached output may include replay and merged streams")
		}
		return Result{Status: status, Reason: reason, ExitCode: code, Stdout: cap.stdout.String(), Stderr: cap.stderr.String(), Truncated: cap.truncated, Retryable: status == "failed"}
	}
	for {
		if ctx.Err() != nil {
			if id != "" {
				clean, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				stopped := e.Terminate(clean, r.Snapshot.Target, id)
				cancel()
				if stopped {
					status := "cancelled"
					if errors.Is(ctx.Err(), context.DeadlineExceeded) {
						status = "timed_out"
					}
					return finish(status, "remote termination confirmed", nil)
				}
			}
			return finish("unknown", "observer cancelled; remote completion unconfirmed", nil)
		}
		q := url.Values{"stdin": {"false"}, "tty": {"false"}}
		path := "/v1/sprites/" + url.PathEscape(r.Snapshot.Target.Name) + "/exec"
		if id != "" {
			path += "/" + url.PathEscape(id)
		} else {
			q["cmd"] = r.Snapshot.Job.Execution.Argv
			if r.Snapshot.Job.Execution.Type == "shell" {
				shell := r.Snapshot.Job.Execution.Shell
				if shell == "" {
					shell = "/bin/bash"
				}
				q["cmd"] = []string{shell, "-lc", r.Snapshot.Job.Execution.Script}
			}
			q.Set("max_run_after_disconnect", "60s")
			cfg := r.Snapshot.Job.Execution
			if cfg.Directory != "" {
				q.Set("dir", cfg.Directory)
			}
			for k, v := range cfg.Env {
				q.Add("env", k+"="+v)
			}
			q.Add("env", "SPRITE_CRON_RUN_ID="+r.ID)
			q.Add("env", "SPRITE_CRON_SCHEDULED_AT="+time.UnixMilli(r.Scheduled).UTC().Format(time.RFC3339))
		}
		wsURL := "ws" + strings.TrimPrefix(e.BaseURL, "http") + path + "?" + q.Encode()
		conn, resp, dialErr := e.Dialer.DialContext(ctx, wsURL, http.Header{"Authorization": {"Bearer " + secret}})
		if dialErr != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
				resp.Body.Close()
			}
			if ctx.Err() != nil {
				continue
			}
			if id != "" && reconnects < 3 && status != 404 && status != 410 {
				reconnects++
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
				continue
			}
			if id == "" && (status == 400 || status == 401 || status == 403 || status == 404) {
				result := finish("failed", fmt.Sprintf("exec connection rejected (HTTP %d)", status), nil)
				result.Retryable = false
				return result
			}
			return finish("unknown", fmt.Sprintf("exec connection failed (HTTP %d); dispatch or completion uncertain", status), nil)
		}
		conn.SetReadLimit(2 * outputLimit)
		conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(45 * time.Second)) })
		stopped := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopped:
					return
				case <-ctx.Done():
					conn.Close()
					return
				case <-ticker.C:
					if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
						conn.Close()
						return
					}
				}
			}
		}()
		var readErr error
		var exit *int
		persistFailed := false
		for {
			kind, b, err := conn.ReadMessage()
			if err != nil {
				readErr = err
				break
			}
			conn.SetReadDeadline(time.Now().Add(45 * time.Second))
			if kind == websocket.BinaryMessage && len(b) > 0 {
				switch b[0] {
				case 1, 2:
					cap.append(b[0], b[1:])
				case 3:
					if len(b) >= 2 {
						code := int(b[1])
						exit = &code
					}
				}
			}
			if kind == websocket.TextMessage {
				var m struct {
					Type string `json:"type"`
					ID   string `json:"session_id"`
					Code *int   `json:"exit_code"`
				}
				if json.Unmarshal(b, &m) != nil {
					readErr = errors.New("invalid protocol message")
					break
				}
				switch m.Type {
				case "session_info":
					if m.ID != "" {
						id = m.ID
						if err := onSession(id); err != nil {
							persistFailed = true
						}
					}
				case "exit":
					exit = m.Code
				case "error":
					readErr = errors.New("remote exec error")
				}
			}
			if persistFailed || exit != nil || readErr != nil {
				break
			}
		}
		close(stopped)
		conn.Close()
		<-done
		if persistFailed {
			return finish("unknown", "could not persist session acknowledgement", nil)
		}
		if exit != nil {
			if *exit == 0 {
				return finish("succeeded", "", exit)
			}
			return finish("failed", fmt.Sprintf("exit status %d", *exit), exit)
		}
		if ctx.Err() != nil {
			continue
		}
		if id != "" && reconnects < 3 {
			reconnects++
			replayed = true
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		return finish("unknown", "exec stream ended without an exit result", nil)
	}
}
func (e *Executor) Terminate(ctx context.Context, t Target, id string) bool {
	if id == "" {
		return false
	}
	secret, err := e.secret(t.CredentialID)
	if err != nil {
		return false
	}
	for _, signal := range []string{"SIGTERM", "SIGKILL"} {
		resp, err := e.request(ctx, "POST", "/v1/sprites/"+url.PathEscape(t.Name)+"/exec/"+url.PathEscape(id)+"/kill?signal="+signal+"&timeout=3s", secret, nil)
		if err != nil {
			return false
		}
		if resp.StatusCode == 410 {
			resp.Body.Close()
			return true
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return false
		}
		decoder := json.NewDecoder(io.LimitReader(resp.Body, 65536))
		stopped := false
		for {
			var event struct {
				Type string `json:"type"`
			}
			if decoder.Decode(&event) != nil {
				break
			}
			if event.Type == "exited" || event.Type == "complete" {
				stopped = true
			}
		}
		resp.Body.Close()
		if stopped {
			return true
		}
	}
	return false
}
