package daemon

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"
)

//go:embed web/*
var assets embed.FS

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, status int, err error) {
	jsonResponse(w, status, map[string]string{"error": err.Error()})
}
func body(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		failure(w, 400, err)
		return false
	}
	return true
}
func (a *App) allowedOrigin(origin string) bool {
	for _, allowed := range a.cfg.Origins {
		if origin == allowed {
			return true
		}
	}
	return false
}
func (a *App) authenticated(r *http.Request) bool {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		if cookie, err := r.Cookie("jarvis_session"); err == nil {
			token = cookie.Value
		}
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(a.token)) == 1
}
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	files, _ := fs.Sub(assets, "web")
	mux.Handle("GET /", http.FileServer(http.FS(files)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Token string `json:"token"`
		}
		if !body(w, r, &p) {
			return
		}
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(p.Token)), []byte(a.token)) != 1 {
			http.Error(w, "invalid access token", 401)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "jarvis_session", Value: a.token, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(r.Header.Get("Origin"), "https://"), SameSite: http.SameSiteStrictMode, MaxAge: 86400})
		jsonResponse(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, a.overview()) })
	mux.HandleFunc("GET /api/machine", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, machineOverview()) })
	mux.HandleFunc("GET /api/inspect", func(w http.ResponseWriter, r *http.Request) {
		result, err := a.inspect(r.Context(), r.URL.Query().Get("name"))
		if err != nil {
			failure(w, 502, err)
			return
		}
		jsonResponse(w, 200, result)
	})
	mux.HandleFunc("POST /api/input", func(w http.ResponseWriter, r *http.Request) {
		var p InputArgs
		if !body(w, r, &p) {
			return
		}
		result, err := a.sendInput(r.Context(), r.Header.Get("Idempotency-Key"), p)
		if err != nil {
			failure(w, 409, err)
			return
		}
		jsonResponse(w, 200, result)
	})
	mux.HandleFunc("POST /api/watch", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}
		if !body(w, r, &p) {
			return
		}
		if err := a.setWatch(r.Context(), p.Name, p.Enabled); err != nil {
			failure(w, 502, err)
			return
		}
		jsonResponse(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/voice", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			SDP string `json:"sdp"`
		}
		if !body(w, r, &p) {
			return
		}
		if strings.TrimSpace(p.SDP) == "" {
			http.Error(w, "SDP required", 400)
			return
		}
		result, err := a.createLive(r.Context(), p.SDP)
		if err != nil {
			failure(w, 502, err)
			return
		}
		jsonResponse(w, 201, result)
	})
	mux.HandleFunc("POST /api/voice/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		live := a.live
		a.mu.Unlock()
		if live != nil {
			live.Touch()
		}
		jsonResponse(w, 200, map[string]bool{"ok": live != nil})
	})
	mux.HandleFunc("DELETE /api/voice", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		live := a.live
		a.mu.Unlock()
		if live != nil {
			live.Close()
		}
		jsonResponse(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/events", a.events)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; media-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'")
		hostOK := false
		for _, origin := range a.cfg.Origins {
			u, err := url.Parse(origin)
			if err == nil && u.Host == r.Host {
				hostOK = true
			}
		}
		if !hostOK {
			http.Error(w, "unexpected host", 403)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			origin := r.Header.Get("Origin")
			if origin != "" && !a.allowedOrigin(origin) {
				http.Error(w, "unexpected origin", 403)
				return
			}
			if r.Method != "GET" && origin == "" && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				http.Error(w, "origin required", 403)
				return
			}
			if r.URL.Path != "/api/login" && !a.authenticated(r) {
				http.Error(w, "authentication required", 401)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (a *App) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsubscribe := a.subscribe()
	defer unsubscribe()
	_, _ = fmt.Fprint(w, "data: {\"type\":\"refresh\"}\n\n")
	flusher.Flush()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-ch:
			data, _ := json.Marshal(event)
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
