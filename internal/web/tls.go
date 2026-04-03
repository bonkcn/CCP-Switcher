package web

import (
	"github.com/bonkcn/ccp-switcher/internal/store"
)

// TLSConfig holds the ACME auto-certificate settings stored in app_settings.
type TLSConfig struct {
	Enabled bool
	Domain  string
}

// LoadTLSConfig reads TLS settings from the database.
func LoadTLSConfig(st *store.Store) TLSConfig {
	enabled, _ := st.GetSetting("tls_enabled")
	domain, _ := st.GetSetting("tls_domain")
	return TLSConfig{
		Enabled: enabled == "true",
		Domain:  domain,
	}
}

// SaveTLSConfig persists TLS settings to the database.
func SaveTLSConfig(st *store.Store, cfg TLSConfig) error {
	enabledStr := "false"
	if cfg.Enabled {
		enabledStr = "true"
	}
	if err := st.SetSetting("tls_enabled", enabledStr); err != nil {
		return err
	}
	return st.SetSetting("tls_domain", cfg.Domain)
}
