package main

import (
	"crypto/tls"
	"log"
	"net/http"
	"os"

	"golang.org/x/crypto/acme/autocert"

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

	handler := server.Routes()

	// Start ACME HTTPS listener if enabled
	tlsCfg := web.LoadTLSConfig(st)
	if tlsCfg.Enabled && tlsCfg.Domain != "" {
		if err := os.MkdirAll(cfg.CertDir, 0o700); err != nil {
			logger.Printf("WARNING: cannot create cert dir %s: %v", cfg.CertDir, err)
		} else {
			m := &autocert.Manager{
				Cache:      autocert.DirCache(cfg.CertDir),
				Prompt:     autocert.AcceptTOS,
				HostPolicy: autocert.HostWhitelist(tlsCfg.Domain),
			}

			// :80 for ACME HTTP-01 challenge + redirect to HTTPS
			go func() {
				logger.Printf("HTTP :80 (ACME challenge + redirect)")
				if err := http.ListenAndServe(":80", m.HTTPHandler(nil)); err != nil {
					logger.Printf("HTTP :80 listener error: %v", err)
				}
			}()

			// :443 TLS with auto-cert
			tlsSrv := &http.Server{
				Addr:    ":443",
				Handler: handler,
				TLSConfig: &tls.Config{
					GetCertificate: m.GetCertificate,
					NextProtos:     []string{"h2", "http/1.1", "acme-tls/1"},
				},
			}
			go func() {
				logger.Printf("HTTPS :443 → %s (autocert)", tlsCfg.Domain)
				if err := tlsSrv.ListenAndServeTLS("", ""); err != nil {
					logger.Printf("HTTPS :443 listener error: %v", err)
				}
			}()
		}
	}

	// Primary HTTP listener (always active as fallback / local access)
	logger.Printf("listening on http://%s", cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, handler); err != nil {
		logger.Fatalf("http server stopped: %v", err)
	}
}
