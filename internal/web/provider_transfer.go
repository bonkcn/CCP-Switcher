package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bonkcn/ccp-switcher/internal/store"
)

const (
	maxProviderImportBytes       = 2 << 20
	providerTransferFileType     = "ccp-switcher/providers"
	providerTransferFormatV1 int = 1
)

type providerTransferFile struct {
	Type       string                   `json:"type"`
	Version    int                      `json:"version"`
	ExportedAt string                   `json:"exported_at,omitempty"`
	Active     map[string]string        `json:"active,omitempty"`
	Providers  []providerTransferRecord `json:"providers"`
}

type providerTransferRecord struct {
	UID                       string `json:"uid,omitempty"`
	Kind                      string `json:"kind"`
	Name                      string `json:"name"`
	BaseURL                   string `json:"base_url"`
	Secret                    string `json:"secret"`
	Model                     string `json:"model,omitempty"`
	ReasoningEffort           string `json:"reasoning_effort,omitempty"`
	CodexApprovalPolicy       string `json:"codex_approval_policy,omitempty"`
	CodexSandboxMode          string `json:"codex_sandbox_mode,omitempty"`
	ClaudeDefaultMode         string `json:"claude_default_mode,omitempty"`
	ClaudeUseSandbox          bool   `json:"claude_use_sandbox,omitempty"`
	ClaudeSkipDangerousPrompt bool   `json:"claude_skip_dangerous_prompt,omitempty"`
}

type providerImportResult struct {
	Imported       int
	Inserted       int
	Updated        int
	RestoredActive []string
	ReloadedActive []string
}

type providerApplyTarget struct {
	ID     int64
	Reason string
}

func (s *Server) handleProvidersExport(w http.ResponseWriter, r *http.Request) {
	providers, err := s.store.ListProviders("")
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err.Error())
		return
	}
	activeIDs, err := s.store.ListActiveProviderIDs()
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err.Error())
		return
	}

	payload := buildProviderTransferFile(providers, activeIDs)
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "导出 JSON 失败")
		return
	}

	filename := fmt.Sprintf("ccp-switcher-providers-%s.json", time.Now().UTC().Format("20060102T150405Z"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleProvidersImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxProviderImportBytes)
	if err := r.ParseMultipartForm(maxProviderImportBytes); err != nil {
		s.redirectWithMessage(w, r, "/providers", "", providerImportFormError(err))
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		s.redirectWithMessage(w, r, "/providers", "", "请选择要导入的 JSON 文件")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		s.redirectWithMessage(w, r, "/providers", "", "读取导入文件失败: "+err.Error())
		return
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		s.redirectWithMessage(w, r, "/providers", "", "导入文件不能为空")
		return
	}

	payload, err := decodeProviderTransfer(data)
	if err != nil {
		s.redirectWithMessage(w, r, "/providers", "", err.Error())
		return
	}

	result, err := s.importProviderTransfer(payload, r.FormValue("restore_active") == "on")
	if err != nil {
		s.redirectWithMessage(w, r, "/providers", "", err.Error())
		return
	}
	s.redirectWithMessage(w, r, "/providers", result.summary(), "")
}

func buildProviderTransferFile(providers []store.Provider, activeIDs map[string]int64) providerTransferFile {
	payload := providerTransferFile{
		Type:       providerTransferFileType,
		Version:    providerTransferFormatV1,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Providers:  make([]providerTransferRecord, 0, len(providers)),
	}

	uidByID := make(map[int64]string, len(providers))
	for _, provider := range providers {
		payload.Providers = append(payload.Providers, providerTransferRecordFromProvider(provider))
		if provider.UID != "" {
			uidByID[provider.ID] = provider.UID
		}
	}

	for _, kind := range providerKinds() {
		id := activeIDs[kind]
		if uid := strings.TrimSpace(uidByID[id]); uid != "" {
			if payload.Active == nil {
				payload.Active = make(map[string]string)
			}
			payload.Active[kind] = uid
		}
	}

	return payload
}

func decodeProviderTransfer(data []byte) (providerTransferFile, error) {
	var payload providerTransferFile
	if err := json.Unmarshal(data, &payload); err != nil {
		return providerTransferFile{}, fmt.Errorf("导入文件不是合法 JSON: %w", err)
	}
	if err := validateProviderTransfer(&payload); err != nil {
		return providerTransferFile{}, err
	}
	return payload, nil
}

func validateProviderTransfer(payload *providerTransferFile) error {
	payload.Type = strings.TrimSpace(payload.Type)
	if payload.Type == "" {
		payload.Type = providerTransferFileType
	}
	if payload.Type != providerTransferFileType {
		return fmt.Errorf("不支持的导入文件类型: %s", payload.Type)
	}
	switch payload.Version {
	case 0:
		payload.Version = providerTransferFormatV1
	case providerTransferFormatV1:
	default:
		return fmt.Errorf("不支持的导入文件版本: %d", payload.Version)
	}

	normalized := make([]providerTransferRecord, 0, len(payload.Providers))
	seenUID := make(map[string]struct{}, len(payload.Providers))
	seenKindName := make(map[string]struct{}, len(payload.Providers))
	for _, record := range payload.Providers {
		provider, err := transferRecordToProvider(record)
		if err != nil {
			return err
		}
		record = providerTransferRecordFromProvider(provider)
		normalized = append(normalized, record)

		if record.UID != "" {
			if _, exists := seenUID[record.UID]; exists {
				return fmt.Errorf("导入文件中存在重复 uid: %s", record.UID)
			}
			seenUID[record.UID] = struct{}{}
		}

		key := providerKindNameKey(record.Kind, record.Name)
		if _, exists := seenKindName[key]; exists {
			return fmt.Errorf("导入文件中存在重复供应商: %s / %s", kindLabel(record.Kind), record.Name)
		}
		seenKindName[key] = struct{}{}
	}
	payload.Providers = normalized

	if len(payload.Active) == 0 {
		payload.Active = nil
		return nil
	}

	normalizedActive := make(map[string]string, len(payload.Active))
	for kind, uid := range payload.Active {
		kind = strings.TrimSpace(kind)
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if kind != "claude" && kind != "codex" {
			return fmt.Errorf("导入文件中存在未知活跃供应商类型: %s", kind)
		}
		normalizedActive[kind] = uid
	}
	payload.Active = normalizedActive
	return nil
}

func transferRecordToProvider(record providerTransferRecord) (store.Provider, error) {
	provider := store.Provider{
		UID:                       strings.TrimSpace(record.UID),
		Kind:                      strings.TrimSpace(record.Kind),
		Name:                      strings.TrimSpace(record.Name),
		BaseURL:                   record.BaseURL,
		Secret:                    strings.TrimSpace(record.Secret),
		Model:                     strings.TrimSpace(record.Model),
		ReasoningEffort:           strings.TrimSpace(record.ReasoningEffort),
		CodexApprovalPolicy:       strings.TrimSpace(record.CodexApprovalPolicy),
		CodexSandboxMode:          strings.TrimSpace(record.CodexSandboxMode),
		ClaudeDefaultMode:         strings.TrimSpace(record.ClaudeDefaultMode),
		ClaudeUseSandbox:          record.ClaudeUseSandbox,
		ClaudeSkipDangerousPrompt: record.ClaudeSkipDangerousPrompt,
	}
	return normalizeProviderInput(provider)
}

func providerTransferRecordFromProvider(provider store.Provider) providerTransferRecord {
	record := providerTransferRecord{
		UID:                       strings.TrimSpace(provider.UID),
		Kind:                      strings.TrimSpace(provider.Kind),
		Name:                      strings.TrimSpace(provider.Name),
		BaseURL:                   normalizeBaseURL(provider.BaseURL),
		Secret:                    strings.TrimSpace(provider.Secret),
		Model:                     strings.TrimSpace(provider.Model),
		ReasoningEffort:           strings.TrimSpace(provider.ReasoningEffort),
		CodexApprovalPolicy:       strings.TrimSpace(provider.CodexApprovalPolicy),
		CodexSandboxMode:          strings.TrimSpace(provider.CodexSandboxMode),
		ClaudeDefaultMode:         strings.TrimSpace(provider.ClaudeDefaultMode),
		ClaudeUseSandbox:          provider.ClaudeUseSandbox,
		ClaudeSkipDangerousPrompt: provider.ClaudeSkipDangerousPrompt,
	}
	switch record.Kind {
	case "claude":
		record.ReasoningEffort = ""
		record.CodexApprovalPolicy = ""
		record.CodexSandboxMode = ""
	case "codex":
		record.ClaudeDefaultMode = ""
		record.ClaudeUseSandbox = false
		record.ClaudeSkipDangerousPrompt = false
	}
	return record
}

func (s *Server) importProviderTransfer(payload providerTransferFile, restoreActive bool) (providerImportResult, error) {
	existingProviders, err := s.store.ListProviders("")
	if err != nil {
		return providerImportResult{}, err
	}
	activeIDs, err := s.store.ListActiveProviderIDs()
	if err != nil {
		return providerImportResult{}, err
	}

	existingByUID := make(map[string]store.Provider, len(existingProviders))
	existingByKindName := make(map[string][]store.Provider, len(existingProviders))
	for _, provider := range existingProviders {
		if provider.UID != "" {
			existingByUID[provider.UID] = provider
		}
		key := providerKindNameKey(provider.Kind, provider.Name)
		existingByKindName[key] = append(existingByKindName[key], provider)
	}

	var result providerImportResult
	changedIDs := make(map[int64]struct{}, len(payload.Providers))
	resolvedUIDToID := make(map[string]int64, len(payload.Providers))
	for _, record := range payload.Providers {
		provider, err := transferRecordToProvider(record)
		if err != nil {
			return result, err
		}

		match, matched, err := matchImportedProvider(existingByUID, existingByKindName, provider)
		if err != nil {
			return result, err
		}
		incomingUID := provider.UID
		if matched {
			provider.ID = match.ID
			if match.UID != "" {
				provider.UID = match.UID
			}
			result.Updated++
		} else {
			result.Inserted++
		}

		if err := s.store.SaveProvider(&provider); err != nil {
			return result, err
		}
		result.Imported++
		changedIDs[provider.ID] = struct{}{}

		if incomingUID != "" {
			resolvedUIDToID[incomingUID] = provider.ID
		}
		if provider.UID != "" {
			resolvedUIDToID[provider.UID] = provider.ID
		}
	}

	applyTargets := make(map[string]providerApplyTarget, len(providerKinds()))
	if restoreActive {
		for _, kind := range providerKinds() {
			uid := strings.TrimSpace(payload.Active[kind])
			if uid == "" {
				continue
			}
			if id, ok := resolvedUIDToID[uid]; ok {
				applyTargets[kind] = providerApplyTarget{ID: id, Reason: "restore"}
				continue
			}
			if provider, ok := existingByUID[uid]; ok {
				applyTargets[kind] = providerApplyTarget{ID: provider.ID, Reason: "restore"}
				continue
			}
			return result, fmt.Errorf("%s；但导出文件里的 %s 当前生效项在本机找不到对应供应商", result.summary(), kindLabel(kind))
		}
	}

	for _, kind := range providerKinds() {
		currentID := activeIDs[kind]
		if currentID == 0 {
			continue
		}
		if _, changed := changedIDs[currentID]; !changed {
			continue
		}
		if _, exists := applyTargets[kind]; exists {
			continue
		}
		applyTargets[kind] = providerApplyTarget{ID: currentID, Reason: "reload"}
	}

	var applyErrors []string
	for _, kind := range providerKinds() {
		target, ok := applyTargets[kind]
		if !ok {
			continue
		}
		if _, _, err := s.runtime.SwitchProvider(target.ID); err != nil {
			applyErrors = append(applyErrors, fmt.Sprintf("%s: %v", kindLabel(kind), err))
			continue
		}
		switch target.Reason {
		case "restore":
			result.RestoredActive = append(result.RestoredActive, kindLabel(kind))
		case "reload":
			result.ReloadedActive = append(result.ReloadedActive, kindLabel(kind))
		}
	}

	if len(applyErrors) > 0 {
		return result, fmt.Errorf("%s；但后续生效处理失败：%s", result.summary(), strings.Join(applyErrors, "；"))
	}
	return result, nil
}

func (r providerImportResult) summary() string {
	var parts []string
	if r.Imported == 0 {
		parts = append(parts, "导入完成，文件中没有供应商配置")
	} else {
		parts = append(parts, fmt.Sprintf("已导入 %d 个供应商（新增 %d，更新 %d）", r.Imported, r.Inserted, r.Updated))
	}
	if len(r.RestoredActive) > 0 {
		parts = append(parts, "已同步当前生效: "+strings.Join(r.RestoredActive, " / "))
	}
	if len(r.ReloadedActive) > 0 {
		parts = append(parts, "已热重载当前生效: "+strings.Join(r.ReloadedActive, " / "))
	}
	return strings.Join(parts, "，")
}

func normalizeProviderInput(provider store.Provider) (store.Provider, error) {
	provider.UID = strings.TrimSpace(provider.UID)
	provider.Kind = strings.TrimSpace(provider.Kind)
	provider.Name = strings.TrimSpace(provider.Name)
	provider.BaseURL = normalizeBaseURL(provider.BaseURL)
	provider.Secret = strings.TrimSpace(provider.Secret)
	provider.Model = strings.TrimSpace(provider.Model)
	provider.ReasoningEffort = strings.TrimSpace(provider.ReasoningEffort)
	provider.CodexApprovalPolicy = strings.TrimSpace(provider.CodexApprovalPolicy)
	provider.CodexSandboxMode = strings.TrimSpace(provider.CodexSandboxMode)
	provider.ClaudeDefaultMode = strings.TrimSpace(provider.ClaudeDefaultMode)
	if provider.ClaudeDefaultMode == "" {
		provider.ClaudeDefaultMode = "default"
	}

	if provider.Kind != "claude" && provider.Kind != "codex" {
		return provider, fmt.Errorf("provider kind must be claude or codex")
	}
	if provider.Kind == "codex" {
		if !isAllowedValue(provider.CodexApprovalPolicy, "", "untrusted", "on-failure", "on-request", "never") {
			return provider, fmt.Errorf("invalid approval policy")
		}
		if !isAllowedValue(provider.CodexSandboxMode, "", "read-only", "workspace-write", "danger-full-access") {
			return provider, fmt.Errorf("invalid sandbox mode")
		}
	} else {
		provider.CodexApprovalPolicy = ""
		provider.CodexSandboxMode = ""
	}
	if provider.Name == "" {
		return provider, fmt.Errorf("provider name is required")
	}
	if provider.BaseURL == "" {
		return provider, fmt.Errorf("base URL is required")
	}
	if provider.Secret == "" {
		return provider, fmt.Errorf("API key or token is required")
	}
	return provider, nil
}

func matchImportedProvider(existingByUID map[string]store.Provider, existingByKindName map[string][]store.Provider, provider store.Provider) (store.Provider, bool, error) {
	if provider.UID != "" {
		if existing, ok := existingByUID[provider.UID]; ok {
			return existing, true, nil
		}
	}

	candidates := existingByKindName[providerKindNameKey(provider.Kind, provider.Name)]
	switch len(candidates) {
	case 0:
		return store.Provider{}, false, nil
	case 1:
		return candidates[0], true, nil
	default:
		return store.Provider{}, false, fmt.Errorf("%s / %s 在当前库中存在多个同名项，无法自动导入，请先手动整理后再试", kindLabel(provider.Kind), provider.Name)
	}
}

func providerKinds() []string {
	return []string{"claude", "codex"}
}

func providerKindNameKey(kind string, name string) string {
	return strings.TrimSpace(kind) + "\n" + strings.TrimSpace(name)
}

func providerImportFormError(err error) string {
	if strings.Contains(err.Error(), "request body too large") {
		return fmt.Sprintf("导入文件过大，最大支持 %d MiB", maxProviderImportBytes>>20)
	}
	return "导入表单解析失败: " + err.Error()
}
