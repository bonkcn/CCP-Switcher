package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/bonkcn/ccp-switcher/internal/app"
	"github.com/bonkcn/ccp-switcher/internal/store"
)

type Manager struct {
	cfg        app.Config
	store      *store.Store
	httpClient *http.Client
}

type TestResult struct {
	Status  string
	Message string
}

type ModelCatalogResult struct {
	Status  string
	Message string
	Models  []string
}

type RuntimeSessionStatus struct {
	Kind        string
	Session     string
	BinaryPath  string
	BinaryFound bool
	Running     bool
}

type ManagedConfigStatus struct {
	Kind       string
	Compatible bool
	Mode       string
	Details    string
}

func NewManager(cfg app.Config, st *store.Store) *Manager {
	return &Manager{
		cfg:   cfg,
		store: st,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func (m *Manager) ImportExistingConfigs() error {
	if err := m.importClaudeConfig(); err != nil {
		return err
	}
	if err := m.importCodexConfig(); err != nil {
		return err
	}
	return nil
}

func (m *Manager) SwitchProvider(id int64) (store.Provider, string, error) {
	provider, err := m.store.GetProvider(id)
	if err != nil {
		return store.Provider{}, "", err
	}

	backupDir, err := m.backupProviderFiles(provider.Kind)
	if err != nil {
		_ = m.store.AddSwitchLog(provider.Kind, provider.ID, "switch", "backup failed: "+err.Error(), "", false)
		return store.Provider{}, "", err
	}

	applyErr := func() error {
		switch provider.Kind {
		case "claude":
			return m.applyClaudeProvider(provider)
		case "codex":
			return m.applyCodexProvider(provider)
		default:
			return errors.New("unsupported provider kind")
		}
	}()
	if applyErr != nil {
		_ = m.store.AddSwitchLog(provider.Kind, provider.ID, "switch", applyErr.Error(), backupDir, false)
		return store.Provider{}, backupDir, applyErr
	}

	if err := m.store.SetActiveProvider(provider.Kind, provider.ID); err != nil {
		_ = m.store.AddSwitchLog(provider.Kind, provider.ID, "switch", "failed to mark provider active: "+err.Error(), backupDir, false)
		return store.Provider{}, backupDir, err
	}

	summary := fmt.Sprintf("switched %s to %s", provider.Kind, provider.Name)
	if err := m.store.AddSwitchLog(provider.Kind, provider.ID, "switch", summary, backupDir, true); err != nil {
		return provider, backupDir, err
	}
	return provider, backupDir, nil
}

func (m *Manager) TestConnectivity(id int64) (store.Provider, TestResult, error) {
	provider, err := m.store.GetProvider(id)
	if err != nil {
		return store.Provider{}, TestResult{}, err
	}

	req, err := http.NewRequest(http.MethodGet, provider.BaseURL, nil)
	if err != nil {
		return provider, TestResult{Status: "error", Message: "invalid base URL: " + err.Error()}, nil
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return provider, TestResult{Status: "error", Message: "connectivity test failed: " + err.Error()}, nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	status := "ok"
	if resp.StatusCode >= 400 {
		status = "error"
	}
	return provider, TestResult{
		Status:  status,
		Message: summarizeConnectivityResult(provider.BaseURL, resp, body),
	}, nil
}

func (m *Manager) FetchModels(id int64) (store.Provider, ModelCatalogResult, error) {
	provider, err := m.store.GetProvider(id)
	if err != nil {
		return store.Provider{}, ModelCatalogResult{}, err
	}
	return provider, m.FetchModelsForProvider(provider), nil
}

func (m *Manager) TestModelAPI(id int64, model string) (store.Provider, TestResult, error) {
	provider, err := m.store.GetProvider(id)
	if err != nil {
		return store.Provider{}, TestResult{}, err
	}
	return provider, m.TestModelAPIForProvider(provider, model), nil
}

func (m *Manager) TestModelCLI(id int64, model string) (store.Provider, TestResult, error) {
	provider, err := m.store.GetProvider(id)
	if err != nil {
		return store.Provider{}, TestResult{}, err
	}
	return provider, m.TestModelCLIForProvider(provider, model), nil
}

func (m *Manager) FetchModelsForProvider(provider store.Provider) ModelCatalogResult {
	switch provider.Kind {
	case "codex":
		return m.fetchCodexModels(provider)
	case "claude":
		return m.fetchClaudeModels(provider)
	default:
		return ModelCatalogResult{Status: "error", Message: "unsupported provider kind"}
	}
}

func (m *Manager) TestModelAPIForProvider(provider store.Provider, model string) TestResult {
	switch provider.Kind {
	case "codex":
		return m.testCodexModelAPI(provider, model)
	case "claude":
		return m.testClaudeModelAPI(provider, model)
	default:
		return TestResult{Status: "error", Message: "unsupported provider kind"}
	}
}

func (m *Manager) TestModelCLIForProvider(provider store.Provider, model string) TestResult {
	switch provider.Kind {
	case "codex":
		return m.testCodexModelCLI(provider, model)
	case "claude":
		return m.testClaudeModelCLI(provider, model)
	default:
		return TestResult{Status: "error", Message: "unsupported provider kind"}
	}
}

func (m *Manager) SessionStatus(kind string) RuntimeSessionStatus {
	status := RuntimeSessionStatus{
		Kind:    kind,
		Session: m.sessionName(kind),
	}

	command := m.commandForKind(kind)
	if binaryPath, err := exec.LookPath(command); err == nil {
		status.BinaryPath = binaryPath
		status.BinaryFound = true
	}

	if err := exec.Command("tmux", "has-session", "-t", status.Session).Run(); err == nil {
		status.Running = true
	}
	return status
}

func (m *Manager) ManagedStatus(kind string) ManagedConfigStatus {
	switch kind {
	case "claude":
		return m.inspectClaudeManagedStatus()
	case "codex":
		return m.inspectCodexManagedStatus()
	default:
		return ManagedConfigStatus{
			Kind:    kind,
			Mode:    "unknown",
			Details: "unsupported provider kind",
		}
	}
}

func (m *Manager) Launch(kind string) (RuntimeSessionStatus, error) {
	status := m.SessionStatus(kind)
	if !status.BinaryFound {
		return status, fmt.Errorf("%s command not found", kind)
	}
	if status.Running {
		return status, nil
	}

	script := fmt.Sprintf("cd %q && exec %q", m.cfg.DefaultWorkdir, status.BinaryPath)
	cmd := exec.Command("tmux", "new-session", "-d", "-s", status.Session, "bash", "-lc", script)
	if output, err := cmd.CombinedOutput(); err != nil {
		return status, fmt.Errorf("launch %s: %v: %s", kind, err, strings.TrimSpace(string(output)))
	}

	return m.SessionStatus(kind), nil
}

func (m *Manager) Stop(kind string) error {
	session := m.sessionName(kind)
	if err := exec.Command("tmux", "has-session", "-t", session).Run(); err != nil {
		return nil
	}
	if err := exec.Command("tmux", "kill-session", "-t", session).Run(); err != nil {
		return fmt.Errorf("stop %s session: %w", kind, err)
	}
	return nil
}

func (m *Manager) importClaudeConfig() error {
	providers, err := m.store.ListProviders("claude")
	if err != nil {
		return err
	}
	if len(providers) > 0 {
		return nil
	}

	provider, err := readClaudeProvider(m.cfg.ClaudeSettingsPath)
	if err != nil {
		return err
	}
	if provider == nil {
		return nil
	}

	provider.Name = "Imported current Claude"
	if err := m.store.SaveProvider(provider); err != nil {
		return err
	}
	return m.store.SetActiveProvider("claude", provider.ID)
}

func (m *Manager) importCodexConfig() error {
	providers, err := m.store.ListProviders("codex")
	if err != nil {
		return err
	}
	if len(providers) > 0 {
		return nil
	}

	provider, err := readCodexProvider(m.cfg.CodexConfigPath, m.cfg.CodexAuthPath)
	if err != nil {
		return err
	}
	if provider == nil {
		return nil
	}

	provider.Name = "Imported current Codex"
	if err := m.store.SaveProvider(provider); err != nil {
		return err
	}
	return m.store.SetActiveProvider("codex", provider.ID)
}

func readClaudeProvider(path string) (*store.Provider, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read claude settings: %w", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(content, &raw); err != nil {
		return nil, fmt.Errorf("parse claude settings: %w", err)
	}

	env := nestedMap(raw, "env")
	baseURL, _ := env["ANTHROPIC_BASE_URL"].(string)
	secret, _ := env["ANTHROPIC_AUTH_TOKEN"].(string)
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(secret) == "" {
		return nil, nil
	}

	provider := &store.Provider{
		Kind:                      "claude",
		BaseURL:                   strings.TrimSpace(baseURL),
		Secret:                    strings.TrimSpace(secret),
		Model:                     stringValue(raw["model"]),
		ClaudeUseSandbox:          stringValue(env["IS_SANDBOX"]) == "1",
		ClaudeSkipDangerousPrompt: boolValue(raw["skipDangerousModePermissionPrompt"]),
		ClaudeDefaultMode:         stringValue(nestedMap(raw, "permissions")["defaultMode"]),
	}
	if provider.ClaudeDefaultMode == "" {
		provider.ClaudeDefaultMode = "default"
	}
	return provider, nil
}

func readCodexProvider(configPath string, authPath string) (*store.Provider, error) {
	authContent, err := os.ReadFile(authPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read codex auth: %w", err)
	}

	var auth map[string]string
	if err := json.Unmarshal(authContent, &auth); err != nil {
		return nil, fmt.Errorf("parse codex auth: %w", err)
	}
	secret := strings.TrimSpace(auth["OPENAI_API_KEY"])
	if secret == "" {
		return nil, nil
	}

	configData, err := readTOMLFile(configPath)
	if err != nil {
		return nil, err
	}

	modelProviders := nestedMap(configData, "model_providers")
	custom := nestedMap(modelProviders, "custom")
	baseURL := strings.TrimSpace(stringValue(custom["base_url"]))
	if baseURL == "" {
		return nil, nil
	}

	return &store.Provider{
		Kind:                "codex",
		BaseURL:             baseURL,
		Secret:              secret,
		Model:               stringValue(configData["model"]),
		ReasoningEffort:     stringValue(configData["model_reasoning_effort"]),
		CodexApprovalPolicy: stringValue(configData["approval_policy"]),
		CodexSandboxMode:    stringValue(configData["sandbox_mode"]),
	}, nil
}

func (m *Manager) applyClaudeProvider(provider store.Provider) error {
	if content, err := os.ReadFile(m.cfg.ClaudeSettingsPath); err == nil {
		updated, ok, err := updateClaudeSettingsText(content, provider)
		if err != nil {
			return err
		}
		if ok {
			return writeAtomic(m.cfg.ClaudeSettingsPath, updated, 0o600)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing claude settings: %w", err)
	}

	return m.seedClaudeProvider(provider)
}

func (m *Manager) applyCodexProvider(provider store.Provider) error {
	if content, err := os.ReadFile(m.cfg.CodexAuthPath); err == nil {
		updated, ok, err := updateCodexAuthText(content, provider.Secret)
		if err != nil {
			return err
		}
		if ok {
			if err := writeAtomic(m.cfg.CodexAuthPath, updated, 0o600); err != nil {
				return err
			}
		} else if err := writeCodexAuthFile(m.cfg.CodexAuthPath, provider.Secret); err != nil {
			return err
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := writeCodexAuthFile(m.cfg.CodexAuthPath, provider.Secret); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("read existing codex auth: %w", err)
	}

	if content, err := os.ReadFile(m.cfg.CodexConfigPath); err == nil {
		updated, ok, err := updateCodexConfigText(content, provider)
		if err != nil {
			return err
		}
		if ok {
			return writeAtomic(m.cfg.CodexConfigPath, updated, 0o600)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing codex config: %w", err)
	}

	return m.seedCodexProvider(provider)
}

func (m *Manager) seedClaudeProvider(provider store.Provider) error {
	data := make(map[string]any)
	if content, err := os.ReadFile(m.cfg.ClaudeSettingsPath); err == nil && len(bytes.TrimSpace(content)) > 0 {
		if err := json.Unmarshal(content, &data); err != nil {
			return fmt.Errorf("parse existing claude settings: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing claude settings: %w", err)
	}

	env := ensureNestedMap(data, "env")
	env["ANTHROPIC_BASE_URL"] = provider.BaseURL
	env["ANTHROPIC_AUTH_TOKEN"] = provider.Secret
	env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
	env["CLAUDE_CODE_ATTRIBUTION_HEADER"] = "0"
	if provider.ClaudeUseSandbox || provider.ClaudeDefaultMode == "bypassPermissions" {
		env["IS_SANDBOX"] = "1"
	} else {
		delete(env, "IS_SANDBOX")
	}

	permissions := ensureNestedMap(data, "permissions")
	if provider.ClaudeDefaultMode != "" {
		permissions["defaultMode"] = provider.ClaudeDefaultMode
	}
	if provider.Model != "" {
		data["model"] = provider.Model
	}
	if provider.ClaudeSkipDangerousPrompt {
		data["skipDangerousModePermissionPrompt"] = true
	} else {
		delete(data, "skipDangerousModePermissionPrompt")
	}

	content, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claude settings: %w", err)
	}
	content = append(content, '\n')
	return writeAtomic(m.cfg.ClaudeSettingsPath, content, 0o600)
}

func (m *Manager) seedCodexProvider(provider store.Provider) error {
	configData, err := readTOMLFile(m.cfg.CodexConfigPath)
	if err != nil {
		return err
	}

	configData["model_provider"] = "custom"
	if provider.Model != "" {
		configData["model"] = provider.Model
	}
	if provider.ReasoningEffort != "" {
		configData["model_reasoning_effort"] = provider.ReasoningEffort
	}
	if provider.CodexApprovalPolicy != "" {
		configData["approval_policy"] = provider.CodexApprovalPolicy
	}
	if provider.CodexSandboxMode != "" {
		configData["sandbox_mode"] = provider.CodexSandboxMode
	}

	modelProviders := ensureNestedMap(configData, "model_providers")
	custom := ensureNestedMap(modelProviders, "custom")
	custom["name"] = "custom"
	custom["wire_api"] = "responses"
	custom["requires_openai_auth"] = true
	custom["base_url"] = provider.BaseURL

	if err := writeCodexAuthFile(m.cfg.CodexAuthPath, provider.Secret); err != nil {
		return err
	}

	var buf bytes.Buffer
	encoder := toml.NewEncoder(&buf)
	if err := encoder.Encode(configData); err != nil {
		return fmt.Errorf("encode codex config: %w", err)
	}
	return writeAtomic(m.cfg.CodexConfigPath, buf.Bytes(), 0o600)
}

func (m *Manager) inspectClaudeManagedStatus() ManagedConfigStatus {
	content, err := os.ReadFile(m.cfg.ClaudeSettingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return ManagedConfigStatus{
			Kind:       "claude",
			Compatible: false,
			Mode:       "首次切换会初始化",
			Details:    "未找到 settings.json。首次切换时会生成一份托管配置，之后只替换 token/base URL。",
		}
	}
	if err != nil {
		return ManagedConfigStatus{
			Kind:    "claude",
			Mode:    "检查失败",
			Details: err.Error(),
		}
	}
	if claudeSupportsTextUpdate(content) {
		return ManagedConfigStatus{
			Kind:       "claude",
			Compatible: true,
			Mode:       "定点替换",
			Details:    "当前 Claude 配置已兼容。切换时优先只替换 ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN，并保留文件布局。",
		}
	}
	return ManagedConfigStatus{
		Kind:       "claude",
		Compatible: false,
		Mode:       "首次切换会初始化",
		Details:    "当前 Claude 配置缺少关键字段。首次切换会补全托管配置，后续再走定点替换。",
	}
}

func (m *Manager) inspectCodexManagedStatus() ManagedConfigStatus {
	configContent, configErr := os.ReadFile(m.cfg.CodexConfigPath)
	authContent, authErr := os.ReadFile(m.cfg.CodexAuthPath)
	if errors.Is(configErr, os.ErrNotExist) || errors.Is(authErr, os.ErrNotExist) {
		return ManagedConfigStatus{
			Kind:       "codex",
			Compatible: false,
			Mode:       "首次切换会初始化",
			Details:    "Codex 配置或 auth 文件不存在。首次切换会初始化托管文件，之后只替换关键字段。",
		}
	}
	if configErr != nil {
		return ManagedConfigStatus{Kind: "codex", Mode: "检查失败", Details: configErr.Error()}
	}
	if authErr != nil {
		return ManagedConfigStatus{Kind: "codex", Mode: "检查失败", Details: authErr.Error()}
	}
	if codexConfigSupportsTextUpdate(configContent) && codexAuthSupportsTextUpdate(authContent) {
		return ManagedConfigStatus{
			Kind:       "codex",
			Compatible: true,
			Mode:       "定点替换",
			Details:    "当前 Codex 配置已兼容。切换时优先只替换 OPENAI_API_KEY 与 custom.base_url；模型、推理强度、审批策略、沙箱模式仅在数据库中已设置时才写入。",
		}
	}
	return ManagedConfigStatus{
		Kind:       "codex",
		Compatible: false,
		Mode:       "首次切换会初始化",
		Details:    "当前 Codex 配置缺少 custom provider 关键字段。首次切换会初始化一次，之后再走定点替换。",
	}
}

func (m *Manager) backupProviderFiles(kind string) (string, error) {
	backupDir := filepath.Join(
		m.cfg.DataDir,
		"backups",
		kind,
		time.Now().UTC().Format("20060102-150405"),
	)
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}

	var paths []string
	switch kind {
	case "claude":
		paths = []string{m.cfg.ClaudeSettingsPath}
	case "codex":
		paths = []string{m.cfg.CodexConfigPath, m.cfg.CodexAuthPath}
	default:
		return "", errors.New("unsupported provider kind")
	}

	for _, path := range paths {
		if err := copyIfExists(path, filepath.Join(backupDir, filepath.Base(path))); err != nil {
			return "", err
		}
	}
	return backupDir, nil
}

func (m *Manager) fetchCodexModels(provider store.Provider) ModelCatalogResult {
	endpoint, err := joinEndpoint(provider.BaseURL, "models")
	if err != nil {
		return ModelCatalogResult{Status: "error", Message: "invalid base URL: " + err.Error()}
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return ModelCatalogResult{Status: "error", Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+provider.Secret)
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return ModelCatalogResult{Status: "error", Message: "fetch models failed: " + err.Error()}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if resp.StatusCode >= 400 {
		result := summarizeErrorHTTPResult(endpoint, resp, body)
		return ModelCatalogResult{Status: result.Status, Message: result.Message}
	}

	models := parseModelList(body)
	if len(models) == 0 {
		result := summarizeModelFetchWithoutList(endpoint, resp, body)
		return ModelCatalogResult{Status: result.Status, Message: result.Message}
	}
	return ModelCatalogResult{Status: "ok", Message: summarizeModelList(models), Models: models}
}

func (m *Manager) fetchClaudeModels(provider store.Provider) ModelCatalogResult {
	endpoint, err := joinEndpoint(provider.BaseURL, "models")
	if err != nil {
		return ModelCatalogResult{Status: "error", Message: "invalid base URL: " + err.Error()}
	}

	type attempt struct {
		name    string
		headers map[string]string
	}

	attempts := []attempt{
		{
			name: "Authorization bearer",
			headers: map[string]string{
				"Authorization": "Bearer " + provider.Secret,
				"Accept":        "application/json",
			},
		},
		{
			name: "Anthropic x-api-key",
			headers: map[string]string{
				"x-api-key":         provider.Secret,
				"anthropic-version": "2023-06-01",
				"Accept":            "application/json",
			},
		},
	}

	var messages []string
	for _, attempt := range attempts {
		req, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			return ModelCatalogResult{Status: "error", Message: err.Error()}
		}
		for key, value := range attempt.headers {
			req.Header.Set(key, value)
		}

		resp, err := m.httpClient.Do(req)
		if err != nil {
			messages = append(messages, attempt.name+": "+err.Error())
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
		resp.Body.Close()

		if resp.StatusCode >= 400 {
			result := summarizeErrorHTTPResult(endpoint, resp, body)
			messages = append(messages, attempt.name+": "+result.Message)
			continue
		}

		models := parseModelList(body)
		if len(models) == 0 {
			result := summarizeModelFetchWithoutList(endpoint, resp, body)
			return ModelCatalogResult{Status: result.Status, Message: result.Message}
		}
		return ModelCatalogResult{Status: "ok", Message: summarizeModelList(models), Models: models}
	}

	return ModelCatalogResult{Status: "error", Message: strings.Join(messages, "\n")}
}

func (m *Manager) testCodexModelAPI(provider store.Provider, model string) TestResult {
	model = strings.TrimSpace(model)
	if model == "" {
		return TestResult{Status: "error", Message: "set a model before running the model call test"}
	}

	endpoint, err := joinEndpoint(provider.BaseURL, "responses")
	if err != nil {
		return TestResult{Status: "error", Message: "invalid base URL: " + err.Error()}
	}

	payload := map[string]any{
		"model":             model,
		"input":             "Reply with exactly OK.",
		"max_output_tokens": 32,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return TestResult{Status: "error", Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+provider.Secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return TestResult{Status: "error", Message: "model call failed: " + err.Error()}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if resp.StatusCode >= 400 {
		return summarizeErrorHTTPResult(endpoint, resp, respBody)
	}

	if text := parseResponseOutputText(respBody); text != "" {
		return TestResult{Status: "ok", Message: "模型调用成功，返回: " + truncateString(text, 220)}
	}
	if looksLikeHTML(resp.Header.Get("Content-Type"), respBody) {
		return TestResult{
			Status:  "error",
			Message: describeHTMLResponse(endpoint, respBody) + " 这说明当前地址更像网页入口，不是兼容的 responses API。",
		}
	}
	return TestResult{
		Status:  "ok",
		Message: summarizeUnknownJSONResult(respBody),
	}
}

func (m *Manager) testCodexModelCLI(provider store.Provider, model string) TestResult {
	model = strings.TrimSpace(model)
	if model == "" {
		return TestResult{Status: "error", Message: "set a model before running the model call test"}
	}

	tempHome, err := os.MkdirTemp("", "aicli-codex-test-*")
	if err != nil {
		return TestResult{Status: "error", Message: "create temp dir: " + err.Error()}
	}
	defer os.RemoveAll(tempHome)

	configData := map[string]any{
		"model_provider":         "custom",
		"model":                  model,
		"approval_policy":        "never",
		"sandbox_mode":           "read-only",
		"model_reasoning_effort": provider.ReasoningEffort,
		"model_providers": map[string]any{
			"custom": map[string]any{
				"name":                 "custom",
				"wire_api":             "responses",
				"requires_openai_auth": true,
				"base_url":             provider.BaseURL,
			},
		},
	}
	if provider.ReasoningEffort == "" {
		delete(configData, "model_reasoning_effort")
	}

	var buf bytes.Buffer
	encoder := toml.NewEncoder(&buf)
	if err := encoder.Encode(configData); err != nil {
		return TestResult{Status: "error", Message: "encode temp codex config: " + err.Error()}
	}
	if err := writeAtomic(filepath.Join(tempHome, ".codex", "config.toml"), buf.Bytes(), 0o600); err != nil {
		return TestResult{Status: "error", Message: err.Error()}
	}
	if err := writeCodexAuthFile(filepath.Join(tempHome, ".codex", "auth.json"), provider.Secret); err != nil {
		return TestResult{Status: "error", Message: err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	lastMessagePath := filepath.Join(tempHome, "last-message.txt")
	cmd := exec.CommandContext(
		ctx,
		m.cfg.CodexCommand,
		"exec",
		"--skip-git-repo-check",
		"--ephemeral",
		"--cd", m.cfg.DefaultWorkdir,
		"--color", "never",
		"--output-last-message", lastMessagePath,
		"-m", model,
		"-c", `approval_policy="never"`,
		"-c", `sandbox_mode="read-only"`,
		"Reply with exactly OK. Do not run commands. Do not use tools.",
	)
	cmd.Dir = m.cfg.DefaultWorkdir
	cmd.Env = append(os.Environ(), "HOME="+tempHome)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return TestResult{Status: "error", Message: "codex CLI model call timed out"}
	}
	if err != nil {
		return TestResult{Status: "error", Message: summarizeCLIError("Codex CLI", err, output)}
	}

	if content, readErr := os.ReadFile(lastMessagePath); readErr == nil {
		trimmed := normalizeWhitespace(string(content))
		if trimmed != "" {
			return TestResult{Status: "ok", Message: "Codex CLI 调用成功，返回: " + truncateString(trimmed, 220)}
		}
	}
	trimmed := normalizeWhitespace(string(output))
	if trimmed == "" {
		return TestResult{Status: "ok", Message: "Codex CLI 调用成功，但没有返回可见文本。"}
	}
	return TestResult{Status: "ok", Message: "Codex CLI 调用成功，返回: " + truncateString(trimmed, 220)}
}

func (m *Manager) testClaudeModelAPI(provider store.Provider, model string) TestResult {
	model = strings.TrimSpace(model)
	if model == "" {
		return TestResult{Status: "error", Message: "set a model before running the model call test"}
	}

	endpoint, err := joinEndpoint(provider.BaseURL, "messages")
	if err != nil {
		return TestResult{Status: "error", Message: "invalid base URL: " + err.Error()}
	}

	payload := map[string]any{
		"model":      model,
		"max_tokens": 32,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with exactly OK."},
		},
	}
	body, _ := json.Marshal(payload)

	type attempt struct {
		name    string
		headers map[string]string
	}
	attempts := []attempt{
		{
			name: "Anthropic x-api-key",
			headers: map[string]string{
				"x-api-key":         provider.Secret,
				"anthropic-version": "2023-06-01",
				"content-type":      "application/json",
				"accept":            "application/json",
			},
		},
		{
			name: "Authorization bearer",
			headers: map[string]string{
				"Authorization": "Bearer " + provider.Secret,
				"content-type":  "application/json",
				"accept":        "application/json",
			},
		},
	}

	var messages []string
	for _, attempt := range attempts {
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return TestResult{Status: "error", Message: err.Error()}
		}
		for key, value := range attempt.headers {
			req.Header.Set(key, value)
		}

		resp, err := m.httpClient.Do(req)
		if err != nil {
			messages = append(messages, attempt.name+": "+err.Error())
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
		resp.Body.Close()

		if resp.StatusCode >= 400 {
			result := summarizeErrorHTTPResult(endpoint, resp, respBody)
			messages = append(messages, attempt.name+": "+result.Message)
			continue
		}
		if text := parseClaudeMessageText(respBody); text != "" {
			return TestResult{Status: "ok", Message: "Claude API 调用成功，返回: " + truncateString(text, 220)}
		}
		if looksLikeHTML(resp.Header.Get("Content-Type"), respBody) {
			return TestResult{
				Status:  "error",
				Message: describeHTMLResponse(endpoint, respBody) + " 这说明当前地址更像网页入口，不是兼容的 messages API。",
			}
		}
		return TestResult{Status: "ok", Message: summarizeUnknownJSONResult(respBody)}
	}

	return TestResult{Status: "error", Message: strings.Join(messages, "\n")}
}

func (m *Manager) testClaudeModelCLI(provider store.Provider, model string) TestResult {
	model = strings.TrimSpace(model)
	if model == "" {
		return TestResult{Status: "error", Message: "set a model before running the model call test"}
	}

	tempHome, err := os.MkdirTemp("", "aicli-claude-test-*")
	if err != nil {
		return TestResult{Status: "error", Message: "create temp dir: " + err.Error()}
	}
	defer os.RemoveAll(tempHome)

	settingsPath := filepath.Join(tempHome, ".claude", "settings.json")
	configData := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_BASE_URL":                       provider.BaseURL,
			"ANTHROPIC_AUTH_TOKEN":                     provider.Secret,
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
			"CLAUDE_CODE_ATTRIBUTION_HEADER":           "0",
		},
		"permissions": map[string]any{
			"defaultMode": defaultString(provider.ClaudeDefaultMode, "default"),
		},
		"model": model,
	}
	if provider.ClaudeUseSandbox || provider.ClaudeDefaultMode == "bypassPermissions" {
		nestedMap(configData, "env")["IS_SANDBOX"] = "1"
	}
	if provider.ClaudeSkipDangerousPrompt {
		configData["skipDangerousModePermissionPrompt"] = true
	}

	content, err := json.MarshalIndent(configData, "", "  ")
	if err != nil {
		return TestResult{Status: "error", Message: "marshal temp settings: " + err.Error()}
	}
	content = append(content, '\n')
	if err := writeAtomic(settingsPath, content, 0o600); err != nil {
		return TestResult{Status: "error", Message: err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(
		ctx,
		m.cfg.ClaudeCommand,
		"-p",
		"--model", model,
		"--permission-mode", defaultString(provider.ClaudeDefaultMode, "default"),
		"--tools", "",
		"Reply with exactly OK.",
	)
	cmd.Dir = m.cfg.DefaultWorkdir
	cmd.Env = append(os.Environ(), "HOME="+tempHome)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return TestResult{Status: "error", Message: "claude model call timed out"}
	}
	if err != nil {
		return TestResult{Status: "error", Message: summarizeCLIError("Claude CLI", err, output)}
	}
	trimmed := normalizeWhitespace(string(output))
	if trimmed == "" {
		return TestResult{Status: "ok", Message: "Claude CLI 调用成功，但没有返回可见文本。"}
	}
	return TestResult{Status: "ok", Message: "Claude CLI 调用成功，返回: " + truncateString(trimmed, 220)}
}

func updateClaudeSettingsText(content []byte, provider store.Provider) ([]byte, bool, error) {
	if !claudeSupportsTextUpdate(content) {
		return content, false, nil
	}

	updated, ok := replaceJSONFieldString(string(content), "ANTHROPIC_BASE_URL", provider.BaseURL)
	if !ok {
		return content, false, nil
	}
	updated, ok = replaceJSONFieldString(updated, "ANTHROPIC_AUTH_TOKEN", provider.Secret)
	if !ok {
		return content, false, nil
	}
	if provider.Model != "" {
		if next, replaced := replaceJSONFieldString(updated, "model", provider.Model); replaced {
			updated = next
		}
	}
	if provider.ClaudeDefaultMode != "" {
		if next, replaced := replaceJSONFieldString(updated, "defaultMode", provider.ClaudeDefaultMode); replaced {
			updated = next
		}
	}
	return ensureTrailingNewline([]byte(updated)), true, nil
}

func updateCodexAuthText(content []byte, secret string) ([]byte, bool, error) {
	if !codexAuthSupportsTextUpdate(content) {
		return content, false, nil
	}
	updated, ok := replaceJSONFieldString(string(content), "OPENAI_API_KEY", secret)
	if !ok {
		return content, false, nil
	}
	return ensureTrailingNewline([]byte(updated)), true, nil
}

func updateCodexConfigText(content []byte, provider store.Provider) ([]byte, bool, error) {
	if !codexConfigSupportsTextUpdate(content) {
		return content, false, nil
	}

	lines := splitPreserveLines(string(content))
	if !replaceTOMLStringInSection(lines, "", "model_provider", "custom") {
		return content, false, nil
	}
	if !replaceTOMLStringInSection(lines, "model_providers.custom", "base_url", provider.BaseURL) {
		return content, false, nil
	}
	if provider.Model != "" {
		_ = replaceTOMLStringInSection(lines, "", "model", provider.Model)
	}
	if provider.ReasoningEffort != "" {
		_ = replaceTOMLStringInSection(lines, "", "model_reasoning_effort", provider.ReasoningEffort)
	}
	if provider.CodexApprovalPolicy != "" && !replaceTOMLStringInSection(lines, "", "approval_policy", provider.CodexApprovalPolicy) {
		return content, false, nil
	}
	if provider.CodexSandboxMode != "" && !replaceTOMLStringInSection(lines, "", "sandbox_mode", provider.CodexSandboxMode) {
		return content, false, nil
	}

	return []byte(strings.Join(lines, "")), true, nil
}

func writeCodexAuthFile(path string, secret string) error {
	authContent, err := json.MarshalIndent(map[string]string{
		"OPENAI_API_KEY": secret,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal codex auth: %w", err)
	}
	authContent = append(authContent, '\n')
	return writeAtomic(path, authContent, 0o600)
}

func summarizeConnectivityResult(endpoint string, resp *http.Response, body []byte) string {
	contentType := mediaType(resp.Header.Get("Content-Type"))
	if looksLikeHTML(contentType, body) {
		return fmt.Sprintf(
			"HTTP %d。地址可达，但返回的是 HTML 页面。%s 这只能说明站点在线，不能说明 API 兼容。",
			resp.StatusCode,
			describeHTMLResponse(endpoint, body),
		)
	}
	if message := parseJSONErrorMessage(body); message != "" && resp.StatusCode >= 400 {
		return fmt.Sprintf("HTTP %d。%s", resp.StatusCode, message)
	}
	if contentType == "" {
		contentType = "unknown"
	}
	return fmt.Sprintf("HTTP %d。地址可达，返回类型为 %s。", resp.StatusCode, contentType)
}

func summarizeErrorHTTPResult(endpoint string, resp *http.Response, body []byte) TestResult {
	if looksLikeHTML(resp.Header.Get("Content-Type"), body) {
		return TestResult{
			Status:  "error",
			Message: fmt.Sprintf("HTTP %d。%s 这通常说明 base URL 指向了网页入口，而不是兼容的 API 根路径。", resp.StatusCode, describeHTMLResponse(endpoint, body)),
		}
	}
	if message := parseJSONErrorMessage(body); message != "" {
		return TestResult{
			Status:  "error",
			Message: fmt.Sprintf("HTTP %d。%s", resp.StatusCode, message),
		}
	}
	return TestResult{
		Status:  "error",
		Message: fmt.Sprintf("HTTP %d。接口返回了非预期响应。%s", resp.StatusCode, truncateString(normalizeWhitespace(string(body)), 220)),
	}
}

func summarizeModelFetchWithoutList(endpoint string, resp *http.Response, body []byte) TestResult {
	if looksLikeHTML(resp.Header.Get("Content-Type"), body) {
		return TestResult{
			Status:  "error",
			Message: describeHTMLResponse(endpoint, body) + " 这通常说明你填的是控制台主页，或网关没有把该地址暴露成标准 /models 接口。",
		}
	}
	if mediaType(resp.Header.Get("Content-Type")) == "application/json" {
		return TestResult{
			Status:  "error",
			Message: "模型接口可达，但响应不是标准的 models 列表，未找到 data[]。",
		}
	}
	return TestResult{
		Status:  "error",
		Message: "模型接口可达，但返回内容不像标准 models 响应。",
	}
}

func summarizeModelList(models []string) string {
	const preview = 8
	if len(models) <= preview {
		return fmt.Sprintf("成功识别 %d 个模型: %s", len(models), strings.Join(models, ", "))
	}
	return fmt.Sprintf("成功识别 %d 个模型。示例: %s", len(models), strings.Join(models[:preview], ", "))
}

func summarizeUnknownJSONResult(body []byte) string {
	if message := parseJSONErrorMessage(body); message != "" {
		return message
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err == nil {
		var keys []string
		for key := range payload {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) > 6 {
			keys = keys[:6]
		}
		return "模型调用成功，但没有提取到明确文本输出。返回字段: " + strings.Join(keys, ", ")
	}
	return fmt.Sprintf("模型调用成功，但响应不可解析。摘要: %s", truncateString(normalizeWhitespace(string(body)), 220))
}

func summarizeCLIError(label string, err error, output []byte) string {
	lines := cleanedNonEmptyLines(string(output))
	if len(lines) == 0 {
		return fmt.Sprintf("%s 测试失败: %v", label, err)
	}
	if len(lines) > 4 {
		lines = lines[:4]
	}
	return fmt.Sprintf("%s 测试失败: %v。%s", label, err, strings.Join(lines, " | "))
}

func readTOMLFile(path string) (map[string]any, error) {
	data := make(map[string]any)
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return data, nil
		}
		return nil, fmt.Errorf("read toml file %s: %w", path, err)
	}
	if len(bytes.TrimSpace(content)) == 0 {
		return data, nil
	}
	if err := toml.Unmarshal(content, &data); err != nil {
		return nil, fmt.Errorf("parse toml file %s: %w", path, err)
	}
	return data, nil
}

func nestedMap(root map[string]any, key string) map[string]any {
	if root == nil {
		return map[string]any{}
	}
	raw, ok := root[key]
	if !ok {
		return map[string]any{}
	}
	if mapped, ok := raw.(map[string]any); ok {
		return mapped
	}
	return map[string]any{}
}

func ensureNestedMap(root map[string]any, key string) map[string]any {
	if root == nil {
		root = make(map[string]any)
	}
	if mapped, ok := root[key].(map[string]any); ok {
		return mapped
	}
	mapped := make(map[string]any)
	root[key] = mapped
	return mapped
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
}

func boolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	default:
		return false
	}
}

func defaultString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func joinEndpoint(base string, pathPart string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("base URL must include scheme and host")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	appendPath := strings.TrimLeft(pathPart, "/")
	switch {
	case basePath == "":
		parsed.Path = "/" + appendPath
	case appendPath == "":
		parsed.Path = basePath
	default:
		parsed.Path = basePath + "/" + appendPath
	}
	parsed.RawPath = ""
	return parsed.String(), nil
}

func parseModelList(body []byte) []string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}

	var result []string
	data, ok := payload["data"].([]any)
	if !ok {
		return nil
	}
	for _, item := range data {
		mapped, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := mapped["id"].(string); ok && id != "" {
			result = append(result, id)
			continue
		}
		if name, ok := mapped["name"].(string); ok && name != "" {
			result = append(result, name)
		}
	}
	sort.Strings(result)
	return result
}

func parseResponseOutputText(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if text, ok := payload["output_text"].(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func parseClaudeMessageText(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}

	content, ok := payload["content"].([]any)
	if !ok {
		return ""
	}

	var parts []string
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if blockType, _ := block["type"].(string); blockType != "" && blockType != "text" {
			continue
		}
		if text, _ := block["text"].(string); strings.TrimSpace(text) != "" {
			parts = append(parts, strings.TrimSpace(text))
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func writeAtomic(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create parent dir for %s: %w", path, err)
	}

	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, content, mode); err != nil {
		return fmt.Errorf("write temp file %s: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename temp file %s: %w", path, err)
	}
	return nil
}

func copyIfExists(src string, dst string) error {
	content, err := os.ReadFile(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read backup source %s: %w", src, err)
	}
	if err := writeAtomic(dst, content, 0o600); err != nil {
		return fmt.Errorf("write backup file %s: %w", dst, err)
	}
	return nil
}

func truncateString(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n...[truncated]"
}

func claudeSupportsTextUpdate(content []byte) bool {
	text := string(content)
	return strings.Contains(text, `"ANTHROPIC_BASE_URL"`) && strings.Contains(text, `"ANTHROPIC_AUTH_TOKEN"`)
}

func codexAuthSupportsTextUpdate(content []byte) bool {
	return strings.Contains(string(content), `"OPENAI_API_KEY"`)
}

func codexConfigSupportsTextUpdate(content []byte) bool {
	lines := splitPreserveLines(string(content))
	if !hasTOMLKeyInSection(lines, "", "model_provider") {
		return false
	}
	return hasTOMLKeyInSection(lines, "model_providers.custom", "base_url")
}

func replaceJSONFieldString(content string, key string, value string) (string, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return content, false
	}
	re := regexp.MustCompile(`("` + regexp.QuoteMeta(key) + `"\s*:\s*)"(?:\\.|[^"\\])*"`)
	if !re.MatchString(content) {
		return content, false
	}
	return re.ReplaceAllString(content, `${1}`+string(encoded)), true
}

func splitPreserveLines(content string) []string {
	lines := strings.SplitAfter(content, "\n")
	if len(lines) == 0 {
		return []string{""}
	}
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func hasTOMLKeyInSection(lines []string, section string, key string) bool {
	currentSection := ""
	for _, rawLine := range lines {
		line, _ := splitLineEnding(rawLine)
		if nextSection, ok := parseTOMLSection(line); ok {
			currentSection = nextSection
			continue
		}
		if currentSection != section {
			continue
		}
		if tomlLineHasKey(line, key) {
			return true
		}
	}
	return false
}

func replaceTOMLStringInSection(lines []string, section string, key string, value string) bool {
	currentSection := ""
	for index, rawLine := range lines {
		line, ending := splitLineEnding(rawLine)
		if nextSection, ok := parseTOMLSection(line); ok {
			currentSection = nextSection
			continue
		}
		if currentSection != section {
			continue
		}
		updated, ok := replaceTOMLStringLine(line, key, value)
		if ok {
			lines[index] = updated + ending
			return true
		}
	}
	return false
}

func tomlLineHasKey(line string, key string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return false
	}
	commentStripped := stripTOMLComment(trimmed)
	parts := strings.SplitN(commentStripped, "=", 2)
	if len(parts) != 2 {
		return false
	}
	return strings.TrimSpace(parts[0]) == key
}

func replaceTOMLStringLine(line string, key string, value string) (string, bool) {
	if !tomlLineHasKey(line, key) {
		return line, false
	}

	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	comment := ""
	body := strings.TrimSpace(line)
	if index := findTOMLCommentIndex(body); index >= 0 {
		comment = strings.TrimSpace(body[index:])
		body = strings.TrimSpace(body[:index])
	}

	parts := strings.SplitN(body, "=", 2)
	if len(parts) != 2 {
		return line, false
	}
	rawValue := strings.TrimSpace(parts[1])
	quote := `"`
	if strings.HasPrefix(rawValue, `'`) {
		quote = `'`
	}

	updated := indent + key + " = " + quoteTOMLString(value, quote)
	if comment != "" {
		updated += " " + strings.TrimSpace(comment)
	}
	return updated, true
}

func parseTOMLSection(line string) (string, bool) {
	trimmed := strings.TrimSpace(stripTOMLComment(line))
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return "", false
	}
	return strings.Trim(trimmed, "[]"), true
}

func stripTOMLComment(line string) string {
	if index := findTOMLCommentIndex(line); index >= 0 {
		return strings.TrimRight(line[:index], " \t")
	}
	return strings.TrimRight(line, " \t")
}

func findTOMLCommentIndex(line string) int {
	inSingle := false
	inDouble := false
	for index, r := range line {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return index
			}
		}
	}
	return -1
}

func splitLineEnding(line string) (string, string) {
	if strings.HasSuffix(line, "\r\n") {
		return strings.TrimSuffix(line, "\r\n"), "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return strings.TrimSuffix(line, "\n"), "\n"
	}
	return line, ""
}

func quoteTOMLString(value string, quote string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	if quote == `'` {
		escaped = strings.ReplaceAll(escaped, `'`, `\'`)
		return quote + escaped + quote
	}
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return quote + escaped + quote
}

func ensureTrailingNewline(content []byte) []byte {
	if len(content) == 0 || content[len(content)-1] == '\n' {
		return content
	}
	return append(content, '\n')
}

func mediaType(contentType string) string {
	if contentType == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.TrimSpace(contentType)
	}
	return mediaType
}

func looksLikeHTML(contentType string, body []byte) bool {
	mt := mediaType(contentType)
	if mt == "text/html" || mt == "application/xhtml+xml" {
		return true
	}
	trimmed := strings.TrimSpace(strings.ToLower(string(body)))
	return strings.HasPrefix(trimmed, "<!doctype html") || strings.HasPrefix(trimmed, "<html")
}

func describeHTMLResponse(endpoint string, body []byte) string {
	title := extractHTMLTitle(string(body))
	if title != "" {
		return fmt.Sprintf("%s 返回了 HTML 页面，标题为 %q。", endpoint, title)
	}
	return endpoint + " 返回了 HTML 页面。"
}

func extractHTMLTitle(body string) string {
	re := regexp.MustCompile(`(?is)<title[^>]*>\s*(.*?)\s*</title>`)
	matches := re.FindStringSubmatch(body)
	if len(matches) != 2 {
		return ""
	}
	return normalizeWhitespace(matches[1])
}

func parseJSONErrorMessage(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}

	if errorObject, ok := payload["error"].(map[string]any); ok {
		if message, ok := errorObject["message"].(string); ok && strings.TrimSpace(message) != "" {
			return strings.TrimSpace(message)
		}
	}
	for _, key := range []string{"message", "detail", "error_description"} {
		if message, ok := payload[key].(string); ok && strings.TrimSpace(message) != "" {
			return strings.TrimSpace(message)
		}
	}
	return ""
}

func normalizeWhitespace(value string) string {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return ""
	}
	return strings.Join(fields, " ")
}

func cleanedNonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		trimmed := normalizeWhitespace(line)
		if trimmed == "" {
			continue
		}
		lines = append(lines, trimmed)
	}
	return lines
}

func (m *Manager) commandForKind(kind string) string {
	switch kind {
	case "claude":
		return m.cfg.ClaudeCommand
	case "codex":
		return m.cfg.CodexCommand
	default:
		return kind
	}
}

func (m *Manager) sessionName(kind string) string {
	return fmt.Sprintf("%s-%s", m.cfg.TmuxSessionPrefix, kind)
}
