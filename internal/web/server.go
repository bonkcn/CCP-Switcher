package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bonkcn/ccp-switcher/internal/app"
	runtimecfg "github.com/bonkcn/ccp-switcher/internal/runtime"
	"github.com/bonkcn/ccp-switcher/internal/store"
)

const sessionCookieName = "ccp_switcher_session"

//go:embed templates/*.html static/*
var assets embed.FS

type Server struct {
	cfg      app.Config
	store    *store.Store
	runtime  *runtimecfg.Manager
	sessions *SessionManager
	logger   *log.Logger
	tmpl     *template.Template
	repoDir  string
}

type viewData struct {
	Title           string
	CurrentPath     string
	ContentTemplate string
	Content         template.HTML
	Notice          string
	Error           string
	Provider        store.Provider
	Providers       []store.Provider
	ClaudeProviders []store.Provider
	CodexProviders  []store.Provider
	ActiveIDs       map[string]int64
	ActiveClaude    *store.Provider
	ActiveCodex     *store.Provider
	ClaudeManaged   runtimecfg.ManagedConfigStatus
	CodexManaged    runtimecfg.ManagedConfigStatus
	Logs            []store.SwitchLog
	GeneratedToken  string
	BootstrapPath   string
	ListenAddr      string
	ClaudePath      string
	CodexConfigPath string
	CodexAuthPath   string
	Probe           *providerProbeView
	Version         *versionStatusView
}

type providerProbeView struct {
	Provider      *store.Provider
	TestBaseURL   string
	SelectedModel string
	Models        []string
	FetchResult   *runtimecfg.TestResult
	APIResult     *runtimecfg.TestResult
	CLIResult     *runtimecfg.TestResult
}

func NewServer(cfg app.Config, st *store.Store, manager *runtimecfg.Manager, logger *log.Logger) (*Server, error) {
	tmpl, err := template.New("layout.html").Funcs(template.FuncMap{
		"maskSecret":      maskSecret,
		"formatTime":      formatTime,
		"isActive":        isActive,
		"kindLabel":       kindLabel,
		"optionalLabel":   optionalLabel,
		"statusClass":     statusClass,
		"testStatusLabel": testStatusLabel,
		"firstLine":       firstLine,
		"hasMoreText":     hasMoreText,
		"truncateText":    truncateText,
		"providerCount":   providerCount,
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	repoDir, _ := os.Getwd()

	return &Server{
		cfg:      cfg,
		store:    st,
		runtime:  manager,
		sessions: NewSessionManager(),
		logger:   logger,
		tmpl:     tmpl,
		repoDir:  repoDir,
	}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(mustSubFS("static"))))
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.authenticated(s.handleLogout))
	mux.HandleFunc("GET /", s.authenticated(s.handleDashboard))
	mux.HandleFunc("GET /providers", s.authenticated(s.handleProviders))
	mux.HandleFunc("GET /providers/export", s.authenticated(s.handleProvidersExport))
	mux.HandleFunc("GET /providers/{id}/probe", s.authenticated(s.handleProviderProbe))
	mux.HandleFunc("GET /providers/new", s.authenticated(s.handleProviderNew))
	mux.HandleFunc("POST /providers", s.authenticated(s.handleProviderCreate))
	mux.HandleFunc("POST /providers/import", s.authenticated(s.handleProvidersImport))
	mux.HandleFunc("GET /providers/{id}/edit", s.authenticated(s.handleProviderEdit))
	mux.HandleFunc("POST /providers/{id}", s.authenticated(s.handleProviderUpdate))
	mux.HandleFunc("POST /providers/{id}/delete", s.authenticated(s.handleProviderDelete))
	mux.HandleFunc("POST /providers/{id}/test/connectivity", s.authenticated(s.handleTestConnectivity))
	mux.HandleFunc("POST /providers/{id}/probe/models", s.authenticated(s.handleProbeModels))
	mux.HandleFunc("POST /providers/{id}/probe/model", s.authenticated(s.handleProbeSaveModel))
	mux.HandleFunc("POST /providers/{id}/probe/api", s.authenticated(s.handleProbeAPITest))
	mux.HandleFunc("POST /providers/{id}/probe/cli", s.authenticated(s.handleProbeCLITest))
	mux.HandleFunc("POST /providers/{id}/switch", s.authenticated(s.handleSwitchProvider))
	mux.HandleFunc("GET /settings", s.authenticated(s.handleSettings))
	mux.HandleFunc("POST /settings/password", s.authenticated(s.handlePasswordUpdate))
	mux.HandleFunc("POST /settings/token", s.authenticated(s.handleTokenRotate))
	mux.HandleFunc("POST /settings/version/check", s.authenticated(s.handleVersionCheck))
	mux.HandleFunc("POST /settings/update", s.authenticated(s.handleSelfUpdate))
	mux.HandleFunc("GET /history", s.authenticated(s.handleHistory))
	return mux
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.isAuthenticated(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	s.render(w, http.StatusOK, viewData{
		Title:           "Login",
		CurrentPath:     "/login",
		ContentTemplate: "login.html",
		Notice:          r.URL.Query().Get("notice"),
		Error:           r.URL.Query().Get("error"),
		BootstrapPath:   s.cfg.BootstrapCredentialsPath,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	password := strings.TrimSpace(r.FormValue("password"))
	ok, err := s.store.AuthenticatePassword(password)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "验证密码失败")
		return
	}
	if !ok {
		s.redirectWithMessage(w, r, "/login", "", "密码错误")
		return
	}

	fingerprint := loginFingerprint(r)
	sessionToken, ttl, err := s.sessions.Create(fingerprint)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "创建会话失败")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	activeIDs, err := s.store.ListActiveProviderIDs()
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err.Error())
		return
	}
	activeClaude, err := s.store.GetActiveProvider("claude")
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err.Error())
		return
	}
	activeCodex, err := s.store.GetActiveProvider("codex")
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err.Error())
		return
	}
	providers, err := s.store.ListProviders("")
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.render(w, http.StatusOK, viewData{
		Title:           "Dashboard",
		CurrentPath:     "/",
		ContentTemplate: "dashboard.html",
		Notice:          r.URL.Query().Get("notice"),
		Error:           r.URL.Query().Get("error"),
		Providers:       providers,
		ActiveIDs:       activeIDs,
		ActiveClaude:    activeClaude,
		ActiveCodex:     activeCodex,
		ClaudeManaged:   s.runtime.ManagedStatus("claude"),
		CodexManaged:    s.runtime.ManagedStatus("codex"),
		ListenAddr:      s.cfg.ListenAddr,
		ClaudePath:      s.cfg.ClaudeSettingsPath,
		CodexConfigPath: s.cfg.CodexConfigPath,
		CodexAuthPath:   s.cfg.CodexAuthPath,
	})
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	s.renderProvidersPage(w, r, http.StatusOK, nil, r.URL.Query().Get("notice"), r.URL.Query().Get("error"))
}

func (s *Server) handleProviderProbe(w http.ResponseWriter, r *http.Request) {
	provider, err := s.store.GetProvider(mustID(r.PathValue("id")))
	if err != nil {
		s.renderError(w, http.StatusNotFound, "provider not found")
		return
	}
	s.renderProvidersPage(w, r, http.StatusOK, newProviderProbeView(provider, provider.BaseURL, provider.Model, nil), "", "")
}

func (s *Server) handleProviderNew(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind != "claude" && kind != "codex" {
		kind = "claude"
	}

	s.render(w, http.StatusOK, viewData{
		Title:           "New Provider",
		CurrentPath:     "/providers",
		ContentTemplate: "provider_form.html",
		Provider: store.Provider{
			Kind:              kind,
			ClaudeDefaultMode: "default",
		},
	})
}

func (s *Server) handleProviderCreate(w http.ResponseWriter, r *http.Request) {
	provider, err := providerFromRequest(r, store.Provider{})
	if err != nil {
		s.redirectWithMessage(w, r, "/providers", "", err.Error())
		return
	}
	if err := s.store.SaveProvider(&provider); err != nil {
		s.redirectWithMessage(w, r, "/providers", "", err.Error())
		return
	}
	s.redirectWithMessage(w, r, "/providers", "供应商配置已保存", "")
}

func (s *Server) handleProviderEdit(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "invalid provider id")
		return
	}
	provider, err := s.store.GetProvider(id)
	if err != nil {
		s.renderError(w, http.StatusNotFound, "provider not found")
		return
	}
	s.render(w, http.StatusOK, viewData{
		Title:           "Edit Provider",
		CurrentPath:     "/providers",
		ContentTemplate: "provider_form.html",
		Provider:        provider,
	})
}

func (s *Server) handleProviderUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "invalid provider id")
		return
	}

	existing, err := s.store.GetProvider(id)
	if err != nil {
		s.renderError(w, http.StatusNotFound, "provider not found")
		return
	}

	provider, err := providerFromRequest(r, existing)
	if err != nil {
		s.redirectWithMessage(w, r, "/providers", "", err.Error())
		return
	}
	provider.ID = id

	if err := s.store.SaveProvider(&provider); err != nil {
		s.redirectWithMessage(w, r, "/providers", "", err.Error())
		return
	}

	// If this provider is currently active, hot-reload the config files.
	activeIDs, _ := s.store.ListActiveProviderIDs()
	notice := "供应商配置已更新"
	if activeIDs[provider.Kind] == id {
		if _, _, applyErr := s.runtime.SwitchProvider(id); applyErr != nil {
			s.redirectWithMessage(w, r, "/providers", "", "配置已保存，但热切换失败："+applyErr.Error())
			return
		}
		notice = "供应商配置已更新并热切换生效"
	}
	s.redirectWithMessage(w, r, "/providers", notice, "")
}

func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "invalid provider id")
		return
	}
	if err := s.store.DeleteProvider(id); err != nil {
		s.redirectWithMessage(w, r, "/providers", "", err.Error())
		return
	}
	s.redirectWithMessage(w, r, "/providers", "供应商配置已删除", "")
}

func (s *Server) handleTestConnectivity(w http.ResponseWriter, r *http.Request) {
	provider, result, err := s.runtime.TestConnectivity(mustID(r.PathValue("id")))
	if err != nil {
		s.redirectWithMessage(w, r, "/providers", "", err.Error())
		return
	}
	_ = s.store.RecordTestResult(provider.ID, result.Status, result.Message)
	s.redirectWithMessage(w, r, "/providers", provider.Name+" 连通性测试 (Ping) 完成", "")
}

func (s *Server) handleProbeModels(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, http.StatusBadRequest, "invalid form")
		return
	}

	provider, err := s.store.GetProvider(mustID(r.PathValue("id")))
	if err != nil {
		s.renderError(w, http.StatusNotFound, "provider not found")
		return
	}
	probe := probeViewFromRequest(r, provider)
	provider.BaseURL = probe.TestBaseURL
	result := s.runtime.FetchModelsForProvider(provider)
	_ = s.store.RecordTestResult(provider.ID, result.Status, result.Message)
	fetchResult := runtimecfg.TestResult{Status: result.Status, Message: result.Message}
	probe = newProviderProbeView(provider, probe.TestBaseURL, probe.SelectedModel, result.Models)
	probe.APIResult = probeResultFromRequest(r, "probe_api")
	probe.CLIResult = probeResultFromRequest(r, "probe_cli")
	probe.FetchResult = &fetchResult
	s.renderProvidersPage(w, r, http.StatusOK, probe, provider.Name+" 模型列表拉取完成", "")
}

func (s *Server) handleProbeSaveModel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, http.StatusBadRequest, "invalid form")
		return
	}

	id := mustID(r.PathValue("id"))
	provider, err := s.store.GetProvider(id)
	if err != nil {
		s.renderError(w, http.StatusNotFound, "provider not found")
		return
	}

	probe := probeViewFromRequest(r, provider)
	model := strings.TrimSpace(probe.SelectedModel)
	models := probe.Models
	if model == "" {
		s.renderProvidersPage(w, r, http.StatusBadRequest, probe, "", "请先填入要保存的模型名称")
		return
	}
	if !probeModelInList(model, models) {
		s.renderProvidersPage(w, r, http.StatusBadRequest, probe, "", "请先拉取模型列表，再从当前站点返回的模型中选择一个")
		return
	}

	provider.Model = model
	if err := s.store.SaveProvider(&provider); err != nil {
		s.renderProvidersPage(w, r, http.StatusInternalServerError, probe, "", err.Error())
		return
	}

	probe = probeViewFromRequest(r, provider)
	s.renderProvidersPage(w, r, http.StatusOK, probe, provider.Name+" 默认模型已更新", "")
}

func (s *Server) handleProbeAPITest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, http.StatusBadRequest, "invalid form")
		return
	}

	id := mustID(r.PathValue("id"))
	provider, err := s.store.GetProvider(id)
	if err != nil {
		s.renderError(w, http.StatusNotFound, "provider not found")
		return
	}
	probe := probeViewFromRequest(r, provider)
	model := strings.TrimSpace(probe.SelectedModel)
	models := probe.Models
	if !probeModelInList(model, models) {
		s.renderProvidersPage(w, r, http.StatusBadRequest, probe, "", "请先拉取模型列表，再从列表中选择要测试的模型")
		return
	}
	provider.BaseURL = probe.TestBaseURL
	result := s.runtime.TestModelAPIForProvider(provider, model)
	_ = s.store.RecordTestResult(provider.ID, result.Status, result.Message)
	probe.APIResult = &result
	s.renderProvidersPage(w, r, http.StatusOK, probe, provider.Name+" API 测试完成", "")
}

func (s *Server) handleProbeCLITest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, http.StatusBadRequest, "invalid form")
		return
	}

	id := mustID(r.PathValue("id"))
	provider, err := s.store.GetProvider(id)
	if err != nil {
		s.renderError(w, http.StatusNotFound, "provider not found")
		return
	}
	probe := probeViewFromRequest(r, provider)
	model := strings.TrimSpace(probe.SelectedModel)
	models := probe.Models
	if !probeModelInList(model, models) {
		s.renderProvidersPage(w, r, http.StatusBadRequest, probe, "", "请先拉取模型列表，再从列表中选择要测试的模型")
		return
	}
	provider.BaseURL = probe.TestBaseURL
	result := s.runtime.TestModelCLIForProvider(provider, model)
	_ = s.store.RecordTestResult(provider.ID, result.Status, result.Message)
	probe.CLIResult = &result
	s.renderProvidersPage(w, r, http.StatusOK, probe, provider.Name+" CLI 测试完成", "")
}

func (s *Server) handleTestModels(w http.ResponseWriter, r *http.Request) {
	s.handleProbeModels(w, r)
}

func (s *Server) handleTestCall(w http.ResponseWriter, r *http.Request) {
	s.handleProbeCLITest(w, r)
}

func (s *Server) handleSwitchProvider(w http.ResponseWriter, r *http.Request) {
	provider, backupDir, err := s.runtime.SwitchProvider(mustID(r.PathValue("id")))
	if err != nil {
		s.redirectWithMessage(w, r, "/providers", "", err.Error())
		return
	}
	s.redirectWithMessage(w, r, "/providers", provider.Name+" 切换成功，旧配置已备份至: "+backupDir, "")
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettingsPage(w, http.StatusOK, r.URL.Query().Get("notice"), r.URL.Query().Get("error"), "", false)
}

func (s *Server) handlePasswordUpdate(w http.ResponseWriter, r *http.Request) {
	password := strings.TrimSpace(r.FormValue("password"))
	confirm := strings.TrimSpace(r.FormValue("confirm_password"))
	if len(password) < 10 {
		s.redirectWithMessage(w, r, "/settings", "", "密码至少需要 10 个字符")
		return
	}
	if password != confirm {
		s.redirectWithMessage(w, r, "/settings", "", "两次输入的密码不一致")
		return
	}
	if err := s.store.UpdatePassword(password); err != nil {
		s.redirectWithMessage(w, r, "/settings", "", err.Error())
		return
	}
	s.redirectWithMessage(w, r, "/settings", "管理密码已更新", "")
}

func (s *Server) handleTokenRotate(w http.ResponseWriter, r *http.Request) {
	token, err := s.store.RotateAPIToken()
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.renderSettingsPage(w, http.StatusOK, "API Token 已轮换重置。请立即复制，刷新后将不再显示。", "", token, false)
}

func (s *Server) handleVersionCheck(w http.ResponseWriter, r *http.Request) {
	s.renderSettingsPage(w, http.StatusOK, "版本检查已完成", "", "", true)
}

func (s *Server) handleSelfUpdate(w http.ResponseWriter, r *http.Request) {
	version := inspectVersionStatus(s.repoDir, false)
	unitName, err := triggerSelfUpdate(s.cfg, version)
	if err != nil {
		s.renderSettingsPage(w, http.StatusInternalServerError, "", "启动更新失败: "+err.Error(), "", true)
		return
	}
	s.renderSettingsPage(
		w,
		http.StatusOK,
		"更新任务已启动: "+unitName+"。服务将在重新编译后自动重启，页面可能短暂断开，请稍后刷新并再次检查版本。",
		"",
		"",
		true,
	)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	logs, err := s.store.ListSwitchLogs(100)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.render(w, http.StatusOK, viewData{
		Title:           "History",
		CurrentPath:     "/history",
		ContentTemplate: "history.html",
		Notice:          r.URL.Query().Get("notice"),
		Error:           r.URL.Query().Get("error"),
		Logs:            logs,
	})
}

func (s *Server) renderSettingsPage(w http.ResponseWriter, status int, notice string, errorMessage string, generatedToken string, checkRemote bool) {
	s.render(w, status, s.settingsViewData(notice, errorMessage, generatedToken, checkRemote))
}

func (s *Server) settingsViewData(notice string, errorMessage string, generatedToken string, checkRemote bool) viewData {
	return viewData{
		Title:           "Settings",
		CurrentPath:     "/settings",
		ContentTemplate: "settings.html",
		Notice:          notice,
		Error:           errorMessage,
		GeneratedToken:  generatedToken,
		BootstrapPath:   s.cfg.BootstrapCredentialsPath,
		Version:         inspectVersionStatus(s.repoDir, checkRemote),
	}
}

func (s *Server) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAuthenticated(r) {
			http.Redirect(w, r, "/login?error="+url.QueryEscape("login required"), http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (s *Server) renderProvidersPage(w http.ResponseWriter, r *http.Request, status int, probe *providerProbeView, notice string, errorMessage string) {
	data, err := s.providersViewData(notice, errorMessage, probe)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.render(w, status, data)
}

func (s *Server) providersViewData(notice string, errorMessage string, probe *providerProbeView) (viewData, error) {
	providers, err := s.store.ListProviders("")
	if err != nil {
		return viewData{}, err
	}
	activeIDs, err := s.store.ListActiveProviderIDs()
	if err != nil {
		return viewData{}, err
	}
	activeClaude, err := s.store.GetActiveProvider("claude")
	if err != nil {
		return viewData{}, err
	}
	activeCodex, err := s.store.GetActiveProvider("codex")
	if err != nil {
		return viewData{}, err
	}
	claudeProviders, codexProviders := splitProvidersByKind(providers)
	return viewData{
		Title:           "Providers",
		CurrentPath:     "/providers",
		ContentTemplate: "providers.html",
		Notice:          notice,
		Error:           errorMessage,
		Providers:       providers,
		ClaudeProviders: claudeProviders,
		CodexProviders:  codexProviders,
		ActiveIDs:       activeIDs,
		ActiveClaude:    activeClaude,
		ActiveCodex:     activeCodex,
		Probe:           probe,
	}, nil
}

func (s *Server) isAuthenticated(r *http.Request) bool {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		token := strings.TrimSpace(authorization[7:])
		ok, err := s.store.AuthenticateAPIToken(token)
		return err == nil && ok
	}

	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	return s.sessions.Valid(cookie.Value)
}

func (s *Server) render(w http.ResponseWriter, status int, data viewData) {
	var body strings.Builder
	if data.ContentTemplate != "" {
		if err := s.tmpl.ExecuteTemplate(&body, data.ContentTemplate, data); err != nil {
			s.logger.Printf("render content template: %v", err)
			http.Error(w, "template render error", http.StatusInternalServerError)
			return
		}
		data.Content = template.HTML(body.String())
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		s.logger.Printf("render template: %v", err)
	}
}

func (s *Server) renderError(w http.ResponseWriter, status int, message string) {
	s.render(w, status, viewData{
		Title:           "Error",
		CurrentPath:     "",
		ContentTemplate: "error.html",
		Error:           message,
	})
}

func (s *Server) redirectWithMessage(w http.ResponseWriter, r *http.Request, path string, notice string, errorMessage string) {
	values := url.Values{}
	if notice != "" {
		values.Set("notice", notice)
	}
	if errorMessage != "" {
		values.Set("error", errorMessage)
	}
	location := path
	if encoded := values.Encode(); encoded != "" {
		location = path + "?" + encoded
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func providerFromRequest(r *http.Request, existing store.Provider) (store.Provider, error) {
	provider := existing
	provider.Kind = strings.TrimSpace(r.FormValue("kind"))
	provider.Name = strings.TrimSpace(r.FormValue("name"))
	provider.BaseURL = r.FormValue("base_url")
	if secret := strings.TrimSpace(r.FormValue("secret")); secret != "" {
		provider.Secret = secret
	}
	provider.Model = strings.TrimSpace(r.FormValue("model"))
	provider.ReasoningEffort = strings.TrimSpace(r.FormValue("reasoning_effort"))
	provider.CodexApprovalPolicy = strings.TrimSpace(r.FormValue("codex_approval_policy"))
	provider.CodexSandboxMode = strings.TrimSpace(r.FormValue("codex_sandbox_mode"))
	provider.ClaudeDefaultMode = strings.TrimSpace(r.FormValue("claude_default_mode"))
	if provider.ClaudeDefaultMode == "" {
		provider.ClaudeDefaultMode = "default"
	}
	provider.ClaudeUseSandbox = r.FormValue("claude_use_sandbox") == "on"
	provider.ClaudeSkipDangerousPrompt = r.FormValue("claude_skip_dangerous_prompt") == "on"
	return normalizeProviderInput(provider)
}

func parseID(raw string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

func mustID(raw string) int64 {
	id, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return id
}

func normalizeBaseURL(value string) string {
	trimmed := strings.TrimSpace(value)
	return strings.TrimRight(trimmed, "/")
}

func maskSecret(secret string) string {
	if secret == "" {
		return ""
	}
	// Show prefix up to 10 chars (e.g. "sk-ant-api") so users can confirm which key is active
	show := 10
	if len(secret) <= show+4 {
		return secret[:2] + strings.Repeat("*", len(secret)-2)
	}
	return secret[:show] + "..."
}

func loginFingerprint(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	return ip + "|" + r.UserAgent()
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Local().Format("2006-01-02 15:04:05")
}

func isActive(activeIDs map[string]int64, provider store.Provider) bool {
	if activeIDs == nil {
		return false
	}
	return activeIDs[provider.Kind] == provider.ID
}

func kindLabel(kind string) string {
	switch kind {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	default:
		return kind
	}
}

func statusClass(status string) string {
	switch status {
	case "ok":
		return "status-ok"
	case "error":
		return "status-error"
	default:
		return "status-neutral"
	}
}

func testStatusLabel(status string, message string) string {
	if strings.TrimSpace(message) == "" {
		return "未执行"
	}
	switch status {
	case "ok":
		return "通过"
	case "error":
		return "失败"
	default:
		return "已测试"
	}
}

func firstLine(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "-"
	}
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 {
		return strings.TrimSpace(trimmed[:index])
	}
	return trimmed
}

func hasMoreText(value string) bool {
	trimmed := strings.TrimSpace(value)
	return strings.Contains(trimmed, "\n") || len(trimmed) > 220
}

func truncateText(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "-"
	}
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "..."
}

func providerCount(list []store.Provider) string {
	switch len(list) {
	case 0:
		return "暂无"
	case 1:
		return "1 个"
	default:
		return fmt.Sprintf("%d 个", len(list))
	}
}

func optionalLabel(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func isAllowedValue(value string, allowed ...string) bool {
	for _, item := range allowed {
		if strings.TrimSpace(value) == item {
			return true
		}
	}
	return false
}

func splitProvidersByKind(providers []store.Provider) ([]store.Provider, []store.Provider) {
	var claudeProviders []store.Provider
	var codexProviders []store.Provider
	for _, provider := range providers {
		switch provider.Kind {
		case "claude":
			claudeProviders = append(claudeProviders, provider)
		case "codex":
			codexProviders = append(codexProviders, provider)
		}
	}
	return claudeProviders, codexProviders
}

func newProviderProbeView(provider store.Provider, testBaseURL string, selectedModel string, models []string) *providerProbeView {
	models = normalizeProbeModels(models)
	testBaseURL = normalizeBaseURL(firstNonEmpty(testBaseURL, provider.BaseURL))
	selectedModel = chooseProbeModel(selectedModel, provider.Model, models)
	providerCopy := provider
	return &providerProbeView{
		Provider:      &providerCopy,
		TestBaseURL:   testBaseURL,
		SelectedModel: selectedModel,
		Models:        models,
	}
}

func probeViewFromRequest(r *http.Request, provider store.Provider) *providerProbeView {
	probe := newProviderProbeView(provider, r.FormValue("probe_base_url"), r.FormValue("probe_model"), probeModelsFromRequest(r))
	probe.FetchResult = probeResultFromRequest(r, "probe_fetch")
	probe.APIResult = probeResultFromRequest(r, "probe_api")
	probe.CLIResult = probeResultFromRequest(r, "probe_cli")
	return probe
}

func probeModelsFromRequest(r *http.Request) []string {
	return normalizeProbeModels(r.Form["probe_models"])
}

func probeResultFromRequest(r *http.Request, prefix string) *runtimecfg.TestResult {
	status := strings.TrimSpace(r.FormValue(prefix + "_status"))
	message := strings.TrimSpace(r.FormValue(prefix + "_message"))
	if status == "" && message == "" {
		return nil
	}
	return &runtimecfg.TestResult{
		Status:  status,
		Message: message,
	}
}

func normalizeProbeModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	var result []string
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		result = append(result, model)
	}
	return result
}

func chooseProbeModel(explicit string, fallback string, models []string) string {
	explicit = strings.TrimSpace(explicit)
	fallback = strings.TrimSpace(fallback)
	if len(models) == 0 {
		return firstNonEmpty(explicit, fallback)
	}
	if explicit != "" && probeModelInList(explicit, models) {
		return explicit
	}
	if fallback != "" && probeModelInList(fallback, models) {
		return fallback
	}
	return models[0]
}

func probeModelInList(model string, models []string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, item := range models {
		if item == model {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func mustSubFS(path string) fs.FS {
	sub, err := fsSub(path)
	if err != nil {
		panic(err)
	}
	return sub
}
