package app

import (
	"os"
	"path/filepath"
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
	ClaudeCommand            string
	CodexCommand             string
	DefaultWorkdir           string
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
		ClaudeCommand:            getenv("CCP_SWITCHER_CLAUDE_CMD", "claude"),
		CodexCommand:             getenv("CCP_SWITCHER_CODEX_CMD", "codex"),
		DefaultWorkdir:           getenv("CCP_SWITCHER_WORKDIR", "/root"),
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
