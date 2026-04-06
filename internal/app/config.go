package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Config struct {
	ListenAddr               string
	DataDir                  string
	DatabasePath             string
	MasterKeyPath            string
	BootstrapCredentialsPath string
	ClaudeSettingsPath       string
	CodexConfigPath          string
	CodexAuthPath            string
	CodexAccountsDir         string
	ClaudeAccountsDir        string
	ClaudeCommand            string
	CodexCommand             string
	DefaultWorkdir           string
	CertDir                  string
	ServiceFilePath          string
}

func LoadConfig() Config {
	dataDir := getenv("CCP_SWITCHER_DATA_DIR", "/root/.ccp-switcher")

	return Config{
		ListenAddr:               getenv("CCP_SWITCHER_LISTEN", "127.0.0.1:4680"),
		DataDir:                  dataDir,
		DatabasePath:             getenv("CCP_SWITCHER_DB_PATH", filepath.Join(dataDir, "app.db")),
		MasterKeyPath:            getenv("CCP_SWITCHER_MASTER_KEY_PATH", filepath.Join(dataDir, "master.key")),
		BootstrapCredentialsPath: getenv("CCP_SWITCHER_BOOTSTRAP_PATH", filepath.Join(dataDir, "bootstrap-credentials.txt")),
		ClaudeSettingsPath:       getenv("CCP_SWITCHER_CLAUDE_SETTINGS", "/root/.claude/settings.json"),
		CodexConfigPath:          getenv("CCP_SWITCHER_CODEX_CONFIG", "/root/.codex/config.toml"),
		CodexAuthPath:            getenv("CCP_SWITCHER_CODEX_AUTH", "/root/.codex/auth.json"),
		CodexAccountsDir:         getenv("CCP_SWITCHER_CODEX_ACCOUNTS_DIR", filepath.Join(dataDir, "accounts", "codex")),
		ClaudeAccountsDir:        getenv("CCP_SWITCHER_CLAUDE_ACCOUNTS_DIR", filepath.Join(dataDir, "accounts", "claude")),
		ClaudeCommand:            commandPath("CCP_SWITCHER_CLAUDE_CMD", "/root/.local/bin/claude", "claude"),
		CodexCommand:             commandPath("CCP_SWITCHER_CODEX_CMD", "", "codex"),
		DefaultWorkdir:           getenv("CCP_SWITCHER_WORKDIR", "/root"),
		CertDir:                  getenv("CCP_SWITCHER_CERT_DIR", filepath.Join(dataDir, "certs")),
		ServiceFilePath:          getenv("CCP_SWITCHER_SERVICE_PATH", "/etc/systemd/system/ccp-switcher.service"),
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func commandPath(key string, preferred string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	if preferred != "" {
		if _, err := os.Stat(preferred); err == nil {
			return preferred
		}
	}
	if resolved, err := exec.LookPath(fallback); err == nil {
		return resolved
	}
	return fallback
}
