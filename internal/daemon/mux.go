package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Agent struct {
	Agent string `json:"agent"`
	State string `json:"state"`
	Topic string `json:"topic"`
}
type Terminal struct {
	Name     string   `json:"name"`
	Command  []string `json:"command,omitempty"`
	CWD      *string  `json:"cwd"`
	Agent    *Agent   `json:"agent"`
	Exited   bool     `json:"exited"`
	Attached bool     `json:"attached"`
}
type Snapshot struct {
	Name           string   `json:"name"`
	Generation     string   `json:"generation"`
	Revision       uint64   `json:"revision"`
	InputRevision  uint64   `json:"input_revision"`
	PID            int      `json:"pid"`
	ForegroundPGID *int     `json:"foreground_pgid"`
	Cols           int      `json:"cols"`
	Rows           int      `json:"rows"`
	CursorRow      int      `json:"cursor_row"`
	CursorCol      int      `json:"cursor_col"`
	Text           []string `json:"text"`
	CWD            *string  `json:"cwd"`
	Agent          *Agent   `json:"agent"`
	Exited         bool     `json:"exited"`
}
type TerminalIO interface {
	List(context.Context) ([]Terminal, error)
	Inspect(context.Context, string) (Snapshot, error)
	Input(context.Context, Snapshot, []byte) (json.RawMessage, error)
	Watch(context.Context, func(Terminal)) error
}
type Mux struct{ Path, Socket string }

func (m Mux) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, m.Path, args...)
	cmd.Env = append(os.Environ(), "MUXD_NO_AUTOSTART=1")
	if m.Socket != "" {
		cmd.Env = append(cmd.Env, "MUXD_SOCKET="+m.Socket)
	}
	cmd.WaitDelay = time.Second
	return cmd
}
func (m Mux) query(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := m.command(ctx, args...)
	cmd.Stdin = bytes.NewReader(input)
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	var stdout limitedBuffer
	stdout.limit = 2 << 20
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("mux %s: %w (%s)", args[0], err, strings.TrimSpace(stderr.String()))
	}
	if stdout.exceeded {
		return nil, errors.New("terminal response exceeds limit")
	}
	return stdout.Bytes(), nil
}
func (m Mux) List(ctx context.Context) ([]Terminal, error) {
	data, err := m.query(ctx, nil, "ls", "--json")
	if err != nil {
		return nil, err
	}
	list := []Terminal{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var t Terminal
		err = decoder.Decode(&t)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, nil
}
func target(name string) (string, error) {
	if name == "" || len(name) > 256 || strings.ContainsAny(name, "\x00\r\n") {
		return "", errors.New("invalid terminal name")
	}
	return "local:" + name, nil
}
func (m Mux) Inspect(ctx context.Context, name string) (Snapshot, error) {
	var s Snapshot
	t, err := target(name)
	if err != nil {
		return s, err
	}
	data, err := m.query(ctx, nil, "inspect", t)
	if err == nil {
		err = json.Unmarshal(data, &s)
	}
	return s, err
}
func (m Mux) Input(ctx context.Context, s Snapshot, data []byte) (json.RawMessage, error) {
	if s.ForegroundPGID == nil || *s.ForegroundPGID <= 0 {
		return nil, errors.New("terminal has no known foreground process group")
	}
	if len(data) == 0 || len(data) > 8192 {
		return nil, errors.New("input must contain 1–8192 bytes")
	}
	octets := make([]int, len(data))
	for i, b := range data {
		octets[i] = int(b)
	}
	body, _ := json.Marshal(map[string]any{"generation": s.Generation, "revision": s.Revision, "input_revision": s.InputRevision, "foreground_pgid": *s.ForegroundPGID, "data": octets})
	t, err := target(s.Name)
	if err != nil {
		return nil, err
	}
	return m.query(ctx, body, "input", t)
}
func (m Mux) Watch(ctx context.Context, emit func(Terminal)) error {
	cmd := m.command(ctx, "watch", "--json")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var t Terminal
		if err = json.Unmarshal(scanner.Bytes(), &t); err != nil {
			_ = cmd.Process.Kill()
			break
		}
		emit(t)
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	if err != nil {
		return err
	}
	if scanErr != nil {
		return scanErr
	}
	if waitErr != nil {
		return fmt.Errorf("mux watch: %w (%s)", waitErr, stderr.String())
	}
	return io.EOF
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	limit := b.limit
	if limit == 0 {
		limit = 4096
	}
	remaining := limit - b.Len()
	if remaining < len(p) {
		b.exceeded = true
		p = p[:max(remaining, 0)]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
