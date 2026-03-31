package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/bonkcn/ccp-switcher/internal/app"
)

type Store struct {
	db        *sql.DB
	masterKey []byte
}

type Provider struct {
	ID                        int64
	UID                       string
	Kind                      string
	Name                      string
	BaseURL                   string
	Secret                    string
	Model                     string
	ReasoningEffort           string
	CodexApprovalPolicy       string
	CodexSandboxMode          string
	ClaudeDefaultMode         string
	ClaudeUseSandbox          bool
	ClaudeSkipDangerousPrompt bool
	LastTestStatus            string
	LastTestMessage           string
	LastTestAt                time.Time
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

type SwitchLog struct {
	ID           int64
	Kind         string
	ProviderID   int64
	ProviderName string
	Action       string
	Summary      string
	BackupDir    string
	Success      bool
	CreatedAt    time.Time
}

type BootstrapCredentials struct {
	Password string
	APIToken string
	Path     string
	Created  bool
}

func New(dbPath string, masterKey []byte) (*Store, error) {
	if err := os.MkdirAll(filepathDir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	if _, err := db.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	s := &Store{db: db, masterKey: masterKey}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}

	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS providers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	uid TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL CHECK (kind IN ('claude', 'codex')),
	name TEXT NOT NULL,
	base_url TEXT NOT NULL,
	secret_ciphertext TEXT NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	reasoning_effort TEXT NOT NULL DEFAULT '',
	codex_approval_policy TEXT NOT NULL DEFAULT 'on-request',
	codex_sandbox_mode TEXT NOT NULL DEFAULT 'workspace-write',
	claude_default_mode TEXT NOT NULL DEFAULT 'default',
	claude_use_sandbox INTEGER NOT NULL DEFAULT 0,
	claude_skip_dangerous_prompt INTEGER NOT NULL DEFAULT 0,
	last_test_status TEXT NOT NULL DEFAULT '',
	last_test_message TEXT NOT NULL DEFAULT '',
	last_test_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS active_providers (
	kind TEXT PRIMARY KEY CHECK (kind IN ('claude', 'codex')),
	provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
	switched_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS switch_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL,
	provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
	action TEXT NOT NULL,
	summary TEXT NOT NULL,
	backup_dir TEXT NOT NULL DEFAULT '',
	success INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS app_settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
`

	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE providers ADD COLUMN uid TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE providers ADD COLUMN codex_approval_policy TEXT NOT NULL DEFAULT 'on-request'`,
		`ALTER TABLE providers ADD COLUMN codex_sandbox_mode TEXT NOT NULL DEFAULT 'workspace-write'`,
	} {
		if _, err := s.db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("apply schema migration %q: %w", stmt, err)
		}
	}
	if err := s.ensureProviderUIDs(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_providers_uid ON providers(uid) WHERE uid <> ''`); err != nil {
		return fmt.Errorf("create provider uid index: %w", err)
	}
	return nil
}

func (s *Store) ListProviders(kind string) ([]Provider, error) {
	query := `
SELECT id, uid, kind, name, base_url, secret_ciphertext, model, reasoning_effort,
       codex_approval_policy, codex_sandbox_mode,
       claude_default_mode, claude_use_sandbox, claude_skip_dangerous_prompt,
       last_test_status, last_test_message, last_test_at, created_at, updated_at
FROM providers`
	var args []any
	if kind != "" {
		query += " WHERE kind = ?"
		args = append(args, kind)
	}
	query += " ORDER BY kind, name, id"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()

	var providers []Provider
	for rows.Next() {
		provider, err := s.scanProvider(rows)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate providers: %w", err)
	}
	return providers, nil
}

func (s *Store) GetProvider(id int64) (Provider, error) {
	row := s.db.QueryRow(`
SELECT id, uid, kind, name, base_url, secret_ciphertext, model, reasoning_effort,
       codex_approval_policy, codex_sandbox_mode,
       claude_default_mode, claude_use_sandbox, claude_skip_dangerous_prompt,
       last_test_status, last_test_message, last_test_at, created_at, updated_at
FROM providers
WHERE id = ?`, id)

	provider, err := s.scanProvider(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Provider{}, err
		}
		return Provider{}, fmt.Errorf("get provider: %w", err)
	}
	return provider, nil
}

func (s *Store) SaveProvider(provider *Provider) error {
	if provider.Kind != "claude" && provider.Kind != "codex" {
		return errors.New("provider kind must be claude or codex")
	}
	if strings.TrimSpace(provider.Name) == "" {
		return errors.New("provider name is required")
	}
	if strings.TrimSpace(provider.BaseURL) == "" {
		return errors.New("provider base url is required")
	}
	if provider.Secret == "" {
		return errors.New("provider secret is required")
	}
	provider.UID = strings.TrimSpace(provider.UID)
	if provider.UID == "" {
		uid, err := newProviderUID()
		if err != nil {
			return err
		}
		provider.UID = uid
	}

	ciphertext, err := app.EncryptString(s.masterKey, provider.Secret)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if provider.ID == 0 {
		result, err := s.db.Exec(`
INSERT INTO providers (
	uid, kind, name, base_url, secret_ciphertext, model, reasoning_effort,
	codex_approval_policy, codex_sandbox_mode,
	claude_default_mode, claude_use_sandbox, claude_skip_dangerous_prompt,
	last_test_status, last_test_message, last_test_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', ?, ?)`,
			provider.UID,
			provider.Kind,
			strings.TrimSpace(provider.Name),
			strings.TrimSpace(provider.BaseURL),
			ciphertext,
			strings.TrimSpace(provider.Model),
			strings.TrimSpace(provider.ReasoningEffort),
			strings.TrimSpace(provider.CodexApprovalPolicy),
			strings.TrimSpace(provider.CodexSandboxMode),
			strings.TrimSpace(defaultString(provider.ClaudeDefaultMode, "default")),
			boolToInt(provider.ClaudeUseSandbox),
			boolToInt(provider.ClaudeSkipDangerousPrompt),
			now,
			now,
		)
		if err != nil {
			return fmt.Errorf("insert provider: %w", err)
		}
		id, _ := result.LastInsertId()
		provider.ID = id
		return nil
	}

	_, err = s.db.Exec(`
UPDATE providers
SET uid = ?, name = ?, base_url = ?, secret_ciphertext = ?, model = ?, reasoning_effort = ?,
    codex_approval_policy = ?, codex_sandbox_mode = ?,
    claude_default_mode = ?, claude_use_sandbox = ?, claude_skip_dangerous_prompt = ?,
    updated_at = ?
WHERE id = ?`,
		provider.UID,
		strings.TrimSpace(provider.Name),
		strings.TrimSpace(provider.BaseURL),
		ciphertext,
		strings.TrimSpace(provider.Model),
		strings.TrimSpace(provider.ReasoningEffort),
		strings.TrimSpace(provider.CodexApprovalPolicy),
		strings.TrimSpace(provider.CodexSandboxMode),
		strings.TrimSpace(defaultString(provider.ClaudeDefaultMode, "default")),
		boolToInt(provider.ClaudeUseSandbox),
		boolToInt(provider.ClaudeSkipDangerousPrompt),
		now,
		provider.ID,
	)
	if err != nil {
		return fmt.Errorf("update provider: %w", err)
	}
	return nil
}

func (s *Store) DeleteProvider(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM active_providers WHERE provider_id = ?`, id); err != nil {
		return fmt.Errorf("delete active provider link: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM providers WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete provider: %w", err)
	}
	return nil
}

func (s *Store) RecordTestResult(id int64, status string, message string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
UPDATE providers
SET last_test_status = ?, last_test_message = ?, last_test_at = ?, updated_at = ?
WHERE id = ?`, status, message, now, now, id)
	if err != nil {
		return fmt.Errorf("record test result: %w", err)
	}
	return nil
}

func (s *Store) ListActiveProviderIDs() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT kind, provider_id FROM active_providers`)
	if err != nil {
		return nil, fmt.Errorf("list active providers: %w", err)
	}
	defer rows.Close()

	result := make(map[string]int64)
	for rows.Next() {
		var kind string
		var providerID int64
		if err := rows.Scan(&kind, &providerID); err != nil {
			return nil, fmt.Errorf("scan active provider: %w", err)
		}
		result[kind] = providerID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active providers: %w", err)
	}
	return result, nil
}

func (s *Store) GetActiveProvider(kind string) (*Provider, error) {
	row := s.db.QueryRow(`
SELECT p.id, p.uid, p.kind, p.name, p.base_url, p.secret_ciphertext, p.model, p.reasoning_effort,
       p.codex_approval_policy, p.codex_sandbox_mode,
       p.claude_default_mode, p.claude_use_sandbox, p.claude_skip_dangerous_prompt,
       p.last_test_status, p.last_test_message, p.last_test_at, p.created_at, p.updated_at
FROM active_providers a
JOIN providers p ON p.id = a.provider_id
WHERE a.kind = ?`, kind)

	provider, err := s.scanProvider(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get active provider: %w", err)
	}
	return &provider, nil
}

func (s *Store) SetActiveProvider(kind string, providerID int64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
INSERT INTO active_providers (kind, provider_id, switched_at)
VALUES (?, ?, ?)
ON CONFLICT(kind) DO UPDATE SET provider_id = excluded.provider_id, switched_at = excluded.switched_at`,
		kind, providerID, now,
	)
	if err != nil {
		return fmt.Errorf("set active provider: %w", err)
	}
	return nil
}

func (s *Store) AddSwitchLog(kind string, providerID int64, action string, summary string, backupDir string, success bool) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
INSERT INTO switch_logs (kind, provider_id, action, summary, backup_dir, success, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		kind, providerID, action, summary, backupDir, boolToInt(success), now,
	)
	if err != nil {
		return fmt.Errorf("add switch log: %w", err)
	}
	return nil
}

func (s *Store) ListSwitchLogs(limit int) ([]SwitchLog, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := s.db.Query(`
SELECT l.id, l.kind, l.provider_id, p.name, l.action, l.summary, l.backup_dir, l.success, l.created_at
FROM switch_logs l
JOIN providers p ON p.id = l.provider_id
ORDER BY l.id DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list switch logs: %w", err)
	}
	defer rows.Close()

	var logs []SwitchLog
	for rows.Next() {
		var log SwitchLog
		var success int
		var createdAt string
		if err := rows.Scan(
			&log.ID,
			&log.Kind,
			&log.ProviderID,
			&log.ProviderName,
			&log.Action,
			&log.Summary,
			&log.BackupDir,
			&success,
			&createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan switch log: %w", err)
		}
		log.Success = success == 1
		log.CreatedAt = parseTime(createdAt)
		logs = append(logs, log)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate switch logs: %w", err)
	}
	return logs, nil
}

func (s *Store) GetSetting(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get setting %s: %w", key, err)
	}
	return value, nil
}

func (s *Store) SetSetting(key string, value string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
INSERT INTO app_settings (key, value, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, now,
	)
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

func (s *Store) EnsureBootstrapCredentials(path string) (*BootstrapCredentials, error) {
	passwordHash, err := s.GetSetting("password_hash")
	if err != nil {
		return nil, err
	}
	apiTokenHash, err := s.GetSetting("api_token_hash")
	if err != nil {
		return nil, err
	}

	result := &BootstrapCredentials{Path: path}
	if passwordHash != "" && apiTokenHash != "" {
		return result, nil
	}

	if passwordHash == "" {
		password, err := app.RandomToken(12)
		if err != nil {
			return nil, err
		}
		hash, err := app.HashPassword(password)
		if err != nil {
			return nil, err
		}
		if err := s.SetSetting("password_hash", hash); err != nil {
			return nil, err
		}
		result.Password = password
		result.Created = true
	}

	if apiTokenHash == "" {
		token, err := app.RandomToken(18)
		if err != nil {
			return nil, err
		}
		if err := s.SetSetting("api_token_hash", app.HashToken(token)); err != nil {
			return nil, err
		}
		result.APIToken = token
		result.Created = true
	}

	if result.Created {
		var lines []string
		lines = append(lines, "# CCP Switcher bootstrap credentials")
		lines = append(lines, "# Generated at "+time.Now().UTC().Format(time.RFC3339))
		if result.Password != "" {
			lines = append(lines, "PASSWORD="+result.Password)
		}
		if result.APIToken != "" {
			lines = append(lines, "API_TOKEN="+result.APIToken)
		}
		lines = append(lines, "")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
			return nil, fmt.Errorf("write bootstrap credentials: %w", err)
		}
	}

	return result, nil
}

func (s *Store) AuthenticatePassword(password string) (bool, error) {
	hash, err := s.GetSetting("password_hash")
	if err != nil {
		return false, err
	}
	if hash == "" {
		return false, nil
	}
	if err := app.CheckPassword(hash, password); err != nil {
		return false, nil
	}
	return true, nil
}

func (s *Store) UpdatePassword(password string) error {
	hash, err := app.HashPassword(password)
	if err != nil {
		return err
	}
	return s.SetSetting("password_hash", hash)
}

func (s *Store) AuthenticateAPIToken(token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	hash, err := s.GetSetting("api_token_hash")
	if err != nil {
		return false, err
	}
	if hash == "" {
		return false, nil
	}
	return hash == app.HashToken(token), nil
}

func (s *Store) RotateAPIToken() (string, error) {
	token, err := app.RandomToken(18)
	if err != nil {
		return "", err
	}
	if err := s.SetSetting("api_token_hash", app.HashToken(token)); err != nil {
		return "", err
	}
	return token, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanProvider(sc scanner) (Provider, error) {
	var provider Provider
	var secretCiphertext string
	var useSandbox int
	var skipDangerousPrompt int
	var lastTestAt string
	var createdAt string
	var updatedAt string

	err := sc.Scan(
		&provider.ID,
		&provider.UID,
		&provider.Kind,
		&provider.Name,
		&provider.BaseURL,
		&secretCiphertext,
		&provider.Model,
		&provider.ReasoningEffort,
		&provider.CodexApprovalPolicy,
		&provider.CodexSandboxMode,
		&provider.ClaudeDefaultMode,
		&useSandbox,
		&skipDangerousPrompt,
		&provider.LastTestStatus,
		&provider.LastTestMessage,
		&lastTestAt,
		&createdAt,
		&updatedAt,
	)
	if err != nil {
		return Provider{}, err
	}

	secret, err := app.DecryptString(s.masterKey, secretCiphertext)
	if err != nil {
		return Provider{}, fmt.Errorf("decrypt provider secret: %w", err)
	}

	provider.Secret = secret
	provider.ClaudeUseSandbox = useSandbox == 1
	provider.ClaudeSkipDangerousPrompt = skipDangerousPrompt == 1
	provider.LastTestAt = parseTime(lastTestAt)
	provider.CreatedAt = parseTime(createdAt)
	provider.UpdatedAt = parseTime(updatedAt)
	return provider, nil
}

func (s *Store) ensureProviderUIDs() error {
	rows, err := s.db.Query(`SELECT id FROM providers WHERE uid = ''`)
	if err != nil {
		return fmt.Errorf("query providers missing uid: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan provider missing uid: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate providers missing uid: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin uid migration: %w", err)
	}
	defer tx.Rollback()

	for _, id := range ids {
		uid, err := newProviderUID()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE providers SET uid = ? WHERE id = ?`, uid, id); err != nil {
			return fmt.Errorf("assign provider uid: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit uid migration: %w", err)
	}
	return nil
}

func newProviderUID() (string, error) {
	uid, err := app.RandomToken(16)
	if err != nil {
		return "", fmt.Errorf("generate provider uid: %w", err)
	}
	return uid, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func defaultString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func filepathDir(path string) string {
	if path == "" {
		return "."
	}
	dir := path
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			dir = path[:i]
			break
		}
	}
	return dir
}
