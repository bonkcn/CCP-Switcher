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
	TmuxSessionPrefix        string
}

func LoadConfig() Config {
	dataDir := getenv("AI_CLI_MANAGER_DATA_DIR", "/root/.ccp-switcher")

	return Config{
		ListenAddr:               getenv("AI_CLI_MANAGER_LISTEN", "127.0.0.1:4680"),
		DataDir:                  dataDir,
		DatabasePath:             getenv("AI_CLI_MANAGER_DB_PATH", filepath.Join(dataDir, "app.db")),
		MasterKeyPath:            getenv("AI_CLI_MANAGER_MASTER_KEY_PATH", filepath.Join(dataDir, "master.key")),
		BootstrapCredentialsPath: getenv("AI_CLI_MANAGER_BOOTSTRAP_PATH", filepath.Join(dataDir, "bootstrap-credentials.txt")),
		ClaudeSettingsPath:       getenv("AI_CLI_MANAGER_CLAUDE_SETTINGS", "/root/.claude/settings.json"),
		CodexConfigPath:          getenv("AI_CLI_MANAGER_CODEX_CONFIG", "/root/.codex/config.toml"),
		CodexAuthPath:            getenv("AI_CLI_MANAGER_CODEX_AUTH", "/root/.codex/auth.json"),
		ClaudeCommand:            getenv("AI_CLI_MANAGER_CLAUDE_CMD", "claude"),
		CodexCommand:             getenv("AI_CLI_MANAGER_CODEX_CMD", "codex"),
		DefaultWorkdir:           getenv("AI_CLI_MANAGER_WORKDIR", "/root"),
		TmuxSessionPrefix:        getenv("AI_CLI_MANAGER_TMUX_PREFIX", "ccp-switcher"),
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
