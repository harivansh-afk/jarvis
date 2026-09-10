package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"git.harivan.sh/harivansh-afk/jarvis/internal/daemon"
)

func main() {
	home, _ := os.UserHomeDir()
	listen := flag.String("listen", "127.0.0.1:19743", "HTTP listen address")
	state := flag.String("state-dir", filepath.Join(home, ".local/state/jarvis"), "private persistent state")
	mux := flag.String("mux", "muxd", "Mux CLI executable")
	socket := flag.String("mux-socket", "", "Mux socket path")
	origins := flag.String("origins", "http://localhost:19743,http://127.0.0.1:19743", "allowed browser origins")
	model := flag.String("backend-model", "gpt-6-astra", "managed reasoning model")
	effort := flag.String("reasoning-effort", "low", "backend reasoning effort")
	keyFile := flag.String("openai-key-file", "", "file containing the OpenAI project key")
	flag.Parse()
	key := os.Getenv("OPENAI_API_KEY")
	if *keyFile != "" {
		data, err := os.ReadFile(*keyFile)
		if err != nil {
			slog.Error("read OpenAI key file", "error", err)
			os.Exit(1)
		}
		key = strings.TrimSpace(string(data))
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	app, err := daemon.New(daemon.Config{StateDir: *state, MuxPath: *mux, MuxSocket: *socket, Origins: strings.Split(*origins, ","), APIKey: key, BackendModel: *model, ReasoningEffort: *effort})
	if err != nil {
		slog.Error("initialize", "error", err)
		os.Exit(1)
	}
	defer app.Close()
	go app.Run(ctx)
	server := &http.Server{Addr: *listen, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("Jarvis ready", "listen", *listen, "voice_configured", key != "", "token_file", filepath.Join(*state, "access-token"))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
}
