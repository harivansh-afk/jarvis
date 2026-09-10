package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestDisconnectCancelsPendingVoiceCreation(t *testing.T) {
	started := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer provider.Close()
	a := newTestApp(t, &fakeTerminal{})
	a.cfg.APIKey = "test-project-key"
	a.cfg.APIBase = provider.URL
	a.cfg.HTTPClient = provider.Client()
	result := make(chan error, 1)
	go func() {
		_, err := a.createLive(context.Background(), "offer")
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("creation did not reach provider")
	}
	a.mu.Lock()
	pending := a.live
	a.mu.Unlock()
	pending.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled creation succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not cancel creation")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.live != nil {
		t.Fatal("voice reservation was not released")
	}
}

func TestManagedLiveToolRoundTripAndDuplicateSuppression(t *testing.T) {
	calls := make(chan map[string]any, 4)
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-project-key" {
			t.Error("missing provider authentication")
		}
		if r.URL.Path == "/live/sessions" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			session := body["session"].(map[string]any)
			responses := session["delegation"].(map[string]any)["responses"].(map[string]any)
			if session["model"] != "gpt-live-1" || responses["model"] != "gpt-6-astra" {
				t.Error("wrong model configuration")
			}
			jsonResponse(w, 201, map[string]any{"session": map[string]string{"id": "live_test"}, "transport": map[string]string{"sdp": "answer"}})
			return
		}
		if r.URL.Path != "/live/sessions/live_test/attach" {
			t.Error(r.URL.Path)
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		messages := []string{
			`{"type":"response.event","delegation_id":"d1","event":{"type":"response.created","response":{"id":"r1"}}}`,
			`{"type":"response.event","delegation_id":"d1","event":{"type":"response.output_item.done","item":{"type":"function_call","call_id":"c1","name":"list_terminals","arguments":"{}"}}}`,
			`{"type":"response.event","delegation_id":"d1","event":{"type":"response.output_item.done","item":{"type":"function_call","call_id":"c1","name":"list_terminals","arguments":"{}"}}}`,
			`{"type":"response.event","delegation_id":"d1","event":{"type":"response.completed","response":{"id":"r1","output":[]}}}`,
		}
		for _, message := range messages {
			if err = conn.Write(ctx, websocket.MessageText, []byte(message)); err != nil {
				return
			}
		}
		for range 2 {
			_, data, err := conn.Read(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			var event map[string]any
			_ = json.Unmarshal(data, &event)
			calls <- event
		}
		<-release
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.closed"}`))
	}))
	defer provider.Close()
	a := newTestApp(t, &fakeTerminal{})
	a.cfg.APIKey = "test-project-key"
	a.cfg.APIBase = provider.URL
	a.cfg.HTTPClient = provider.Client()
	response, err := a.createLive(context.Background(), "offer")
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	if err = json.Unmarshal(response, &created); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"response.item.create", "response.create"} {
		select {
		case event := <-calls:
			if event["type"] != want {
				t.Fatalf("event %d: %v", i, event)
			}
			if _, ok := event["delegation_id"]; ok {
				t.Fatal("unsupported delegation_id on response command")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("tool result timed out")
		}
	}
	close(release)
}
