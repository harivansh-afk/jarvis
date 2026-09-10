package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type Config struct {
	StateDir, MuxPath, MuxSocket, APIKey, BackendModel, ReasoningEffort string
	Origins                                                             []string
	Terminals                                                           TerminalIO
	APIBase                                                             string
	HTTPClient                                                          *http.Client
}
type inspection struct {
	Snapshot Snapshot
	At       time.Time
}
type Event struct {
	Type string    `json:"type"`
	At   time.Time `json:"at"`
	Data any       `json:"data"`
}
type App struct {
	cfg         Config
	mux         TerminalIO
	store       *Store
	token       string
	mu          sync.Mutex
	commandMu   sync.Mutex
	terminals   map[string]Terminal
	inspections map[string]inspection
	subscribers map[chan Event]struct{}
	connected   bool
	observed    time.Time
	lastError   string
	live        *Live
	ctx         context.Context
}

func New(cfg Config) (*App, error) {
	if cfg.BackendModel == "" {
		cfg.BackendModel = "gpt-6-astra"
	}
	if cfg.ReasoningEffort == "" {
		cfg.ReasoningEffort = "low"
	}
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.openai.com/v1"
	}
	cfg.APIBase = strings.TrimRight(cfg.APIBase, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	store, token, err := openStore(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	term := cfg.Terminals
	if term == nil {
		term = Mux{Path: cfg.MuxPath, Socket: cfg.MuxSocket}
	}
	return &App{cfg: cfg, mux: term, store: store, token: strings.TrimSpace(token), terminals: map[string]Terminal{}, inspections: map[string]inspection{}, subscribers: map[chan Event]struct{}{}, ctx: context.Background()}, nil
}
func (a *App) Close() error {
	a.mu.Lock()
	live := a.live
	a.mu.Unlock()
	if live != nil {
		live.Close()
	}
	return a.store.db.Close()
}
func (a *App) publish(kind string, data any) {
	e := Event{Type: kind, At: time.Now().UTC(), Data: data}
	a.mu.Lock()
	defer a.mu.Unlock()
	for ch := range a.subscribers {
		select {
		case ch <- e:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- Event{Type: "refresh", At: e.At}:
			default:
			}
		}
	}
}
func (a *App) subscribe() (chan Event, func()) {
	ch := make(chan Event, 32)
	a.mu.Lock()
	a.subscribers[ch] = struct{}{}
	a.mu.Unlock()
	return ch, func() { a.mu.Lock(); delete(a.subscribers, ch); a.mu.Unlock() }
}
func (a *App) Run(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()
	for ctx.Err() == nil {
		list, err := a.mux.List(ctx)
		if err == nil {
			a.mu.Lock()
			a.terminals = map[string]Terminal{}
			for _, t := range list {
				a.terminals[t.Name] = t
			}
			a.connected = true
			a.lastError = ""
			a.observed = time.Now().UTC()
			a.mu.Unlock()
			a.reconcileWatches(ctx)
			a.publish("refresh", nil)
			err = a.mux.Watch(ctx, a.observe)
		}
		a.mu.Lock()
		a.connected = false
		if err != nil {
			a.lastError = err.Error()
		}
		a.mu.Unlock()
		a.publish("refresh", nil)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("Mux observer disconnected", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (a *App) reconcileWatches(ctx context.Context) {
	watches, err := a.store.watches()
	if err != nil {
		return
	}
	for _, watch := range watches {
		a.mu.Lock()
		_, exists := a.terminals[watch.Name]
		a.mu.Unlock()
		if exists {
			snapshot, err := a.mux.Inspect(ctx, watch.Name)
			if err != nil {
				continue
			}
			if snapshot.Generation == watch.Generation {
				continue
			}
		}
		if a.store.watch(watch, false) == nil {
			a.publish("notice", fmt.Sprintf("Watch removed for %s: the terminal disappeared or changed identity. Inspect before following it again.", watch.Name))
		}
	}
}
func (a *App) observe(t Terminal) {
	a.mu.Lock()
	old, known := a.terminals[t.Name]
	if t.Exited {
		delete(a.terminals, t.Name)
	} else {
		a.terminals[t.Name] = t
	}
	a.observed = time.Now().UTC()
	a.mu.Unlock()
	a.publish("terminal", t)
	if !known {
		return
	}
	oldState, newState := "", ""
	if old.Agent != nil {
		oldState = old.Agent.State
	}
	if t.Agent != nil {
		newState = t.Agent.State
	}
	if !t.Exited && (newState == oldState || (newState != "blocked" && newState != "idle")) {
		return
	}
	watches, err := a.store.watches()
	if err != nil {
		return
	}
	for _, watch := range watches {
		if watch.Name != t.Name {
			continue
		}
		// Metadata is an observation, never a claim that the underlying task succeeded.
		message := fmt.Sprintf("Terminal %s was observed to exit. Its task outcome has not been established.", t.Name)
		if !t.Exited {
			a.mu.Lock()
			ctx := a.ctx
			a.mu.Unlock()
			snap, err := a.mux.Inspect(ctx, t.Name)
			if err != nil {
				return
			}
			if snap.Generation != watch.Generation {
				message = fmt.Sprintf("The identity of watched terminal %s changed; its watch has been removed. Inspect and watch it again if needed.", t.Name)
				_ = a.store.watch(watch, false)
			} else {
				message = fmt.Sprintf("Mux's terminal classifier now reports %s as %s. This is a heuristic; inspect the terminal before claiming completion or answering an approval.", t.Name, newState)
			}
		} else {
			_ = a.store.watch(watch, false)
		}
		a.publish("notice", message)
		a.mu.Lock()
		live := a.live
		a.mu.Unlock()
		if live != nil {
			live.Update(message, true)
		}
	}
}
func (a *App) overview() map[string]any {
	a.mu.Lock()
	ts := make([]Terminal, 0, len(a.terminals))
	for _, t := range a.terminals {
		ts = append(ts, t)
	}
	connected, observed, lastError := a.connected, a.observed, a.lastError
	live := a.live
	a.mu.Unlock()
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
	watches, _ := a.store.watches()
	receipts, _ := a.store.receipts()
	return map[string]any{"terminals": ts, "mux_connected": connected, "observed_at": observed, "error": lastError, "watches": watches, "receipts": receipts, "voice_configured": a.cfg.APIKey != "", "voice_connected": live != nil && live.Connected(), "backend_model": a.cfg.BackendModel}
}
func (a *App) inspect(ctx context.Context, name string) (map[string]any, error) {
	s, err := a.mux.Inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	id := randomID()
	a.mu.Lock()
	for key, v := range a.inspections {
		if time.Since(v.At) > time.Minute {
			delete(a.inspections, key)
		}
	}
	if len(a.inspections) >= 256 {
		a.mu.Unlock()
		return nil, errors.New("too many outstanding inspections")
	}
	a.inspections[id] = inspection{s, time.Now()}
	a.mu.Unlock()
	return map[string]any{"inspection_id": id, "snapshot": s}, nil
}

type InputArgs struct {
	InspectionID string `json:"inspection_id"`
	Text         string `json:"text"`
	Submit       bool   `json:"submit"`
	Key          string `json:"key,omitempty"`
}

func inputBytes(p InputArgs) ([]byte, error) {
	if p.Key != "" {
		if p.Text != "" || p.Submit {
			return nil, errors.New("key cannot be combined with text")
		}
		switch p.Key {
		case "enter":
			return []byte{'\r'}, nil
		case "escape":
			return []byte{27}, nil
		case "interrupt":
			return []byte{3}, nil
		default:
			return nil, errors.New("unsupported terminal key")
		}
	}
	if strings.IndexFunc(p.Text, func(r rune) bool { return unicode.IsControl(r) }) >= 0 {
		return nil, errors.New("text must be a single line without control characters")
	}
	data := []byte(p.Text)
	if p.Submit {
		data = append(data, '\r')
	}
	if len(data) == 0 || len(data) > 8192 {
		return nil, errors.New("input must contain 1–8192 bytes")
	}
	return data, nil
}
func (a *App) sendInput(ctx context.Context, id string, p InputArgs) (*Receipt, error) {
	if len(id) < 8 || len(id) > 200 {
		return nil, errors.New("a stable request ID of 8–200 characters is required")
	}
	data, err := inputBytes(p)
	if err != nil {
		return nil, err
	}
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	existing, err := a.store.receipt(id)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	a.mu.Lock()
	inspection, ok := a.inspections[p.InspectionID]
	delete(a.inspections, p.InspectionID)
	a.mu.Unlock()
	if !ok || time.Since(inspection.At) > time.Minute {
		return nil, errors.New("inspection expired or already used; inspect again")
	}
	if err = a.store.begin(id, inspection.Snapshot.Name); err != nil {
		return nil, err
	}
	result, writeErr := a.mux.Input(ctx, inspection.Snapshot, data)
	status := "written"
	if writeErr != nil {
		status = "unknown"
		result, _ = json.Marshal(map[string]string{"error": writeErr.Error(), "guidance": "Inspect before any new action. This command will not be replayed."})
	}
	if err = a.store.finish(id, status, result); err != nil {
		return nil, err
	}
	receipt, err := a.store.receipt(id)
	a.publish("receipt", receipt)
	return receipt, err
}
func (a *App) setWatch(ctx context.Context, name string, enabled bool) error {
	watch := Watch{Name: name}
	if enabled {
		snap, err := a.mux.Inspect(ctx, name)
		if err != nil {
			return err
		}
		watch.Generation = snap.Generation
	}
	if err := a.store.watch(watch, enabled); err != nil {
		return err
	}
	a.publish("refresh", nil)
	return nil
}
func machineOverview() map[string]any {
	out := map[string]any{"observed_at": time.Now().UTC()}
	for name, path := range map[string]string{"load": "/proc/loadavg", "memory": "/proc/meminfo", "cpu_pressure": "/proc/pressure/cpu", "memory_pressure": "/proc/pressure/memory", "io_pressure": "/proc/pressure/io", "boot_id": "/proc/sys/kernel/random/boot_id"} {
		data, err := os.ReadFile(path)
		if err == nil {
			out[name] = string(data)
		}
	}
	return out
}
