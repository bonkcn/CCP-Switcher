package main

import (
	"log"
	"net/http"
	"os"

	"github.com/bonkcn/ccp-switcher/internal/app"
	runtimecfg "github.com/bonkcn/ccp-switcher/internal/runtime"
	"github.com/bonkcn/ccp-switcher/internal/store"
	"github.com/bonkcn/ccp-switcher/internal/web"
)

func main() {
	logger := log.New(os.Stdout, "[ccp-switcher] ", log.LstdFlags|log.Lmsgprefix)
	cfg := app.LoadConfig()

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		logger.Fatalf("create data dir: %v", err)
	}

	masterKey, err := app.EnsureMasterKey(cfg.MasterKeyPath)
	if err != nil {
		logger.Fatalf("ensure master key: %v", err)
	}

	st, err := store.New(cfg.DatabasePath, masterKey)
	if err != nil {
		logger.Fatalf("open store: %v", err)
	}
	defer st.Close()

	bootstrap, err := st.EnsureBootstrapCredentials(cfg.BootstrapCredentialsPath)
	if err != nil {
		logger.Fatalf("bootstrap credentials: %v", err)
	}
	if bootstrap.Created {
		logger.Printf("bootstrap credentials written to %s", bootstrap.Path)
	}

	manager := runtimecfg.NewManager(cfg, st)
	if err := manager.ImportExistingConfigs(); err != nil {
		logger.Fatalf("import existing configs: %v", err)
	}

	server, err := web.NewServer(cfg, st, manager, logger)
	if err != nil {
		logger.Fatalf("create server: %v", err)
	}

	server.StartAutoSync()

	logger.Printf("listening on http://%s", cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, server.Routes()); err != nil {
		logger.Fatalf("http server stopped: %v", err)
	}
}
