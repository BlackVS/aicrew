// Command aicrewd is aicrew's HTTPS service. It reads a configuration file,
// opens the store and serves over TLS it terminates itself until it gets
// SIGINT or SIGTERM, then shuts down gracefully and closes the store.
//
//	aicrewd -config /path/to/aicrewd.json
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/BlackVS/aicrew/internal/server"
	"github.com/BlackVS/aicrew/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

// run is the whole service: 0 after a clean shutdown, 1 on a failure, 2 on a
// usage error.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("aicrewd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to the JSON configuration file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *configPath == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	cfg, err := server.LoadConfig(*configPath)
	if err != nil {
		log.Error("configuration refused", "error", err.Error())
		return 1
	}
	st, err := store.Open(ctx, cfg.StorePath)
	if err != nil {
		log.Error("open store", "error", err.Error())
		return 1
	}
	srv, err := server.New(cfg, st, log)
	if err == nil {
		err = srv.ListenAndServe(ctx)
	}
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("service stopped", "error", err.Error())
		return 1
	}
	log.Info("stopped")
	return 0
}
