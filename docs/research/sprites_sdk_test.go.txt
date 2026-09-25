// Research probes, not application tests. Copy into the pinned SDK checkout.
// These assert observed SDK behavior, including limitations; passing is not
// certification that the SDK satisfies the scheduler's reliability contract.
package sprites_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	sprites "github.com/superfly/sprites-go"
)

func sdk(server *httptest.Server, opts ...sprites.Option) *sprites.Client {
	options := []sprites.Option{sprites.WithBaseURL(server.URL), sprites.WithDisableControl()}
	return sprites.New("research-placeholder-not-a-token", append(options, opts...)...)
}

func TestResearchSessionAndWireOptions(t *testing.T) {
	query := make(chan url.Values, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query <- r.URL.Query()
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"type": "session_info", "session_id": "research-17", "tty": false})
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte{3, 0})
	}))
	defer srv.Close()
	client := sdk(srv)
	defer client.Close()
	cmd := client.Sprite("test").Command("echo", "hello world")
	cmd.Dir = "/jobs"
	cmd.Env = []string{"RUN_ID=demo"}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	ids := make(chan string, 1)
	cmd.TextMessageHandler = func(data []byte) {
		if strings.Contains(string(data), "research-17") {
			ids <- "research-17"
		}
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ids:
	default:
		t.Fatal("session callback not delivered")
	}
	q := <-query
	if q.Get("dir") != "/jobs" || q.Get("env") != "RUN_ID=demo" || len(q["cmd"]) != 2 {
		t.Fatal(q)
	}
	if q.Has("max_run_after_disconnect") {
		t.Fatal("disconnect lifetime unexpectedly exposed")
	}
	if cmd.ExitCode() != 0 {
		t.Fatal(cmd.ExitCode())
	}
}

func TestResearchCancellationDoesNotSignalOrCloseDirectConnection(t *testing.T) {
	connected := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	var kills atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/kill") {
			kills.Add(1)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connected <- conn
		<-release
		conn.Close()
	}))
	defer srv.Close()
	defer close(release)
	client := sdk(srv)
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := client.Sprite("test").CommandContext(ctx, "sleep", "300")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	conn := <-connected
	defer conn.Close()
	cancel()
	if err := cmd.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait: %v", err)
	}
	// Consume the SDK's stdin EOF, then check for a signal or socket closure.
	_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			if ne, ok := err.(interface{ Timeout() bool }); !ok || !ne.Timeout() {
				t.Fatalf("connection closed on cancellation: %v", err)
			}
			break
		}
		if kind == websocket.TextMessage && strings.Contains(string(data), "signal") {
			t.Fatal("unexpected signal")
		}
	}
	if kills.Load() != 0 {
		t.Fatal("unexpected kill request")
	}
}

type failingWriter struct{ writes atomic.Int32 }

func (f *failingWriter) Write(p []byte) (int, error) {
	f.writes.Add(1)
	return 0, errors.New("simulated output storage failure")
}

func TestResearchOutputWriterErrorIsIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.BinaryMessage, append([]byte{1}, []byte("output")...))
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte{3, 0})
	}))
	defer srv.Close()
	client := sdk(srv)
	defer client.Close()
	writer := &failingWriter{}
	cmd := client.Sprite("test").Command("test")
	cmd.Stdout, cmd.Stderr = writer, io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("expected ignored writer error, got %v", err)
	}
	if writer.writes.Load() == 0 {
		t.Fatal("writer not called")
	}
}

func TestResearchSignalDoesNotValidateProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/kill") || r.URL.Query().Get("timeout") != "0s" {
			t.Error(r.URL)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, "{\"type\":\"error\",\"message\":\"not terminated\"}\n")
	}))
	defer srv.Close()
	client := sdk(srv)
	defer client.Close()
	if err := client.Sprite("test").SignalSession(context.Background(), "17", "TERM"); err != nil {
		t.Fatalf("expected status-only acceptance, got %v", err)
	}
}

func TestResearchSessionListShapes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantErr    bool
	}{
		{"object", `{"sessions":[{"id":"17","is_active":true}]}`, false},
		{"documented_array", `[{"id":"17","is_active":true}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tc.body) }))
			defer srv.Close()
			client := sdk(srv)
			defer client.Close()
			sessions, err := client.ListSessions(context.Background(), "test")
			if (err != nil) != tc.wantErr {
				t.Fatalf("sessions=%v err=%v", sessions, err)
			}
			if !tc.wantErr && len(sessions) != 1 {
				t.Fatal(sessions)
			}
		})
	}
}

type countingTransport struct{ calls atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return nil, errors.New("custom HTTP transport reached")
}

func TestResearchHTTPClientDoesNotControlExecDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte{3, 0})
	}))
	defer srv.Close()
	transport := &countingTransport{}
	client := sdk(srv, sprites.WithHTTPClient(&http.Client{Transport: transport}))
	defer client.Close()
	cmd := client.Sprite("test").Command("true")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if transport.calls.Load() != 0 {
		t.Fatal("exec used the custom HTTP transport")
	}
}

func TestResearchMissingExitIsNotSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	}))
	defer srv.Close()
	client := sdk(srv)
	defer client.Close()
	cmd := client.Sprite("test").Command("test")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err == nil {
		t.Fatal("missing exit reported success")
	}
	if cmd.ExitCode() != -1 {
		t.Fatal(cmd.ExitCode())
	}
}
