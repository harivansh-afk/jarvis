package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRealMuxThroughAuthenticatedHTTP(t *testing.T) {
	binary := os.Getenv("JARVIS_TEST_MUX")
	if binary == "" {
		t.Skip("set JARVIS_TEST_MUX to the v9 muxd binary for real PTY integration")
	}
	dir, err := os.MkdirTemp("/tmp", "jarvis-mux-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "control.sock")
	env := append(os.Environ(), "MUXD_SOCKET="+socket, "MUXD_MIGRATE_SOCKET="+filepath.Join(dir, "migrate.sock"), "MUXD_NO_AUTOSTART=1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	server := exec.CommandContext(ctx, binary, "--socket", socket)
	server.Env = env
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Process.Kill(); _ = server.Wait() }()
	terminal := Mux{Path: binary, Socket: socket}
	wait := func(condition func() bool) {
		t.Helper()
		for !condition() {
			select {
			case <-ctx.Done():
				t.Fatal("real Mux timed out")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	wait(func() bool { _, err := terminal.List(ctx); return err == nil })
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	attached := exec.CommandContext(ctx, filepath.Join(filepath.Dir(binary), "mux-attach"), "local:jarvis-e2e", "--", cat)
	attached.Env = env
	stdin, err := attached.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := attached.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = attached.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = attached.Process.Kill(); _ = attached.Wait() }()
	marker := make(chan struct{})
	go func() {
		var seen []byte
		buffer := make([]byte, 4096)
		for {
			n, err := stdout.Read(buffer)
			seen = append(seen, buffer[:n]...)
			if bytes.Contains(seen, []byte("jarvis-e2e-marker")) {
				close(marker)
				_, _ = io.Copy(io.Discard, stdout)
				return
			}
			if err != nil {
				return
			}
			if len(seen) > 65536 {
				seen = seen[len(seen)-4096:]
			}
		}
	}()
	wait(func() bool {
		s, err := terminal.Inspect(ctx, "jarvis-e2e")
		return err == nil && s.ForegroundPGID != nil && *s.ForegroundPGID > 0
	})
	a, err := New(Config{StateDir: filepath.Join(dir, "state"), Terminals: terminal})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	web := httptest.NewServer(a.Handler())
	defer web.Close()
	a.cfg.Origins = []string{web.URL}
	request := func(method, path string, body []byte) *http.Response {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, method, web.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+a.token)
		r.Header.Set("Idempotency-Key", "real-mux-request")
		response, err := web.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := request("GET", "/api/inspect?name=jarvis-e2e", nil)
	var inspected struct {
		ID       string   `json:"inspection_id"`
		Snapshot Snapshot `json:"snapshot"`
	}
	err = json.NewDecoder(response.Body).Decode(&inspected)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 {
		t.Fatal("inspect", err, response.StatusCode)
	}
	body, _ := json.Marshal(InputArgs{InspectionID: inspected.ID, Text: "jarvis-e2e-marker", Submit: true})
	for range 2 {
		response = request("POST", "/api/input", body)
		var receipt Receipt
		err = json.NewDecoder(response.Body).Decode(&receipt)
		response.Body.Close()
		if err != nil || receipt.Status != "written" {
			t.Fatal("input", err, receipt)
		}
	}
	select {
	case <-marker:
	case <-ctx.Done():
		t.Fatal("original attached client did not receive input result")
	}
	fresh, err := terminal.Inspect(ctx, "jarvis-e2e")
	if err != nil || fresh.Generation != inspected.Snapshot.Generation || fresh.Rows != inspected.Snapshot.Rows || fresh.Cols != inspected.Snapshot.Cols {
		t.Fatal("terminal identity or dimensions changed", err)
	}
	if !strings.Contains(strings.Join(fresh.Text, "\n"), "jarvis-e2e-marker") {
		t.Fatal("viewport lacks marker")
	}
	if fresh.InputRevision != inspected.Snapshot.InputRevision+1 {
		t.Fatal("HTTP duplicate wrote twice")
	}
	list, err := terminal.List(ctx)
	if err != nil || len(list) != 1 || !list[0].Attached {
		t.Fatal("original client detached", list, err)
	}
}
