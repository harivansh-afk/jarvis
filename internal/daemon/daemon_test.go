package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeTerminal struct {
	mu         sync.Mutex
	writes     int
	fail       bool
	generation string
}

func (f *fakeTerminal) List(context.Context) ([]Terminal, error) {
	return []Terminal{{Name: "test"}}, nil
}
func (f *fakeTerminal) Inspect(context.Context, string) (Snapshot, error) {
	pid := 42
	return Snapshot{Name: "test", Generation: f.generation, ForegroundPGID: &pid, Text: []string{"ready"}}, nil
}
func (f *fakeTerminal) Input(_ context.Context, _ Snapshot, _ []byte) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.fail {
		return nil, errors.New("connection lost after possible write")
	}
	return json.RawMessage(`{"InputWritten":{"bytes":5,"input_revision":1}}`), nil
}
func (f *fakeTerminal) Watch(ctx context.Context, _ func(Terminal)) error {
	<-ctx.Done()
	return ctx.Err()
}
func newTestApp(t *testing.T, term *fakeTerminal) *App {
	t.Helper()
	a, err := New(Config{StateDir: t.TempDir(), Terminals: term, Origins: []string{"http://localhost:19743"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}
func inspectedInput(t *testing.T, a *App) InputArgs {
	t.Helper()
	result, err := a.inspect(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	return InputArgs{InspectionID: result["inspection_id"].(string), Text: "hello", Submit: true}
}

func TestConcurrentDuplicateInputExecutesOnce(t *testing.T) {
	term := &fakeTerminal{}
	a := newTestApp(t, term)
	p := inspectedInput(t, a)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			receipt, err := a.sendInput(context.Background(), "request-123", p)
			if err != nil {
				t.Error(err)
			} else if receipt.Status != "written" {
				t.Error(receipt.Status)
			}
		})
	}
	wg.Wait()
	if term.writes != 1 {
		t.Fatalf("writes=%d", term.writes)
	}
}
func TestUnknownInputIsNotRetried(t *testing.T) {
	term := &fakeTerminal{fail: true}
	a := newTestApp(t, term)
	p := inspectedInput(t, a)
	for range 2 {
		r, err := a.sendInput(context.Background(), "request-unknown", p)
		if err != nil || r.Status != "unknown" {
			t.Fatalf("%+v %v", r, err)
		}
	}
	if term.writes != 1 {
		t.Fatal("unknown delivery was retried")
	}
}
func TestUsedAndExpiredInspectionsCannotWrite(t *testing.T) {
	term := &fakeTerminal{}
	a := newTestApp(t, term)
	p := inspectedInput(t, a)
	if _, err := a.sendInput(context.Background(), "request-first", p); err != nil {
		t.Fatal(err)
	}
	if _, err := a.sendInput(context.Background(), "request-second", p); err == nil {
		t.Fatal("reused inspection accepted")
	}
	p = inspectedInput(t, a)
	a.mu.Lock()
	s := a.inspections[p.InspectionID]
	s.At = time.Now().Add(-2 * time.Minute)
	a.inspections[p.InspectionID] = s
	a.mu.Unlock()
	if _, err := a.sendInput(context.Background(), "request-expired", p); err == nil {
		t.Fatal("expired inspection accepted")
	}
	if term.writes != 1 {
		t.Fatal(term.writes)
	}
}
func TestInputDoesNotInterpretControlSequences(t *testing.T) {
	for _, text := range []string{"hello\nrm -rf x", "\x1b[200~inject", "\x00", "foo\rbar"} {
		if _, err := inputBytes(InputArgs{Text: text, Submit: true}); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
	data, err := inputBytes(InputArgs{Text: "echo '$HOME'", Submit: true})
	if err != nil || string(data) != "echo '$HOME'\r" {
		t.Fatalf("literal input changed: %q %v", data, err)
	}
}
func TestHTTPAuthenticationAndOriginBoundary(t *testing.T) {
	a := newTestApp(t, &fakeTerminal{})
	h := a.Handler()
	cases := []struct {
		method, path, host, origin, token string
		want                              int
	}{
		{"GET", "/api/state", "localhost:19743", "", "", 401},
		{"GET", "/api/state", "evil.example", "", a.token, 403},
		{"GET", "/api/state", "localhost:19743", "https://evil.example", a.token, 403},
		{"GET", "/api/state", "localhost:19743", "", a.token, 200},
		{"POST", "/api/watch", "localhost:19743", "https://evil.example", a.token, 403},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://"+c.host+c.path, strings.NewReader(`{}`))
		r.Header.Set("Origin", c.origin)
		if c.token != "" {
			r.Header.Set("Authorization", "Bearer "+c.token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%+v: got %d", c, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "http://localhost:19743/api/login", strings.NewReader(`{"token":"`+a.token+`"}`))
	r.Header.Set("Origin", "http://localhost:19743")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	cookies := w.Result().Cookies()
	if w.Code != 200 || len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("login cookie boundary", w.Code, cookies)
	}
}
func TestRestartKeepsWatchesAndMarksInFlightDeliveryUnknown(t *testing.T) {
	dir := t.TempDir()
	s, token, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.watch(Watch{"test", "incarnation"}, true); err != nil {
		t.Fatal(err)
	}
	if err = s.begin("inflight", "test"); err != nil {
		t.Fatal(err)
	}
	s.db.Close()
	reopened, again, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	watches, err := reopened.watches()
	if err != nil || len(watches) != 1 || watches[0].Generation != "incarnation" {
		t.Fatal(watches, err)
	}
	receipt, err := reopened.receipt("inflight")
	if err != nil || receipt.Status != "unknown" || again != token {
		t.Fatal(receipt, err)
	}
}
