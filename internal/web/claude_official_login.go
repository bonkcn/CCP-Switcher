package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bonkcn/ccp-switcher/internal/store"
)

var claudeLoginURLPattern = regexp.MustCompile(`https://claude\.com/cai/oauth/authorize\S+`)

type claudeOfficialLoginSession struct {
	ProviderID   int64
	ProviderName string
	StartedAt    time.Time
	FinishedAt   time.Time

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	cancel context.CancelFunc
	done   chan struct{}

	mu        sync.RWMutex
	status    string
	loginURL  string
	output    strings.Builder
	lastError string
}

type ClaudeOfficialAccountStateView struct {
	HomePath        string
	StatePath       string
	CredentialsPath string
	SettingsPath    string
	AuthExists      bool
	LoginInProgress bool
	LoginStatus     string
	LoginURL        string
	LoginOutput     string
	LoginStartedAt  time.Time
	AwaitingCode    bool
	CanStartLogin   bool
	CanCancelLogin  bool
	CanSubmitCode   bool
}

func (s *Server) startClaudeOfficialLogin(provider store.Provider) error {
	if provider.Kind != "claude" || provider.Source != store.ProviderSourceOfficial {
		return fmt.Errorf("仅 Claude Code 官方账号支持浏览器登录编排")
	}
	if err := s.runtime.PrepareClaudeOfficialProvider(provider); err != nil {
		return err
	}

	s.claudeLoginMu.Lock()
	if s.claudeLoginActiveProviderID != 0 && s.claudeLoginActiveProviderID != provider.ID {
		active := s.claudeLoginSessions[s.claudeLoginActiveProviderID]
		name := "另一条会话"
		if active != nil && strings.TrimSpace(active.ProviderName) != "" {
			name = active.ProviderName
		}
		s.claudeLoginMu.Unlock()
		return fmt.Errorf("当前已有官方 Claude 登录进行中：%s。请先完成或取消它", name)
	}
	if existing := s.claudeLoginSessions[provider.ID]; existing != nil && existing.isRunning() {
		s.claudeLoginMu.Unlock()
		return fmt.Errorf("该账号的登录流程已在进行中")
	}
	delete(s.claudeLoginSessions, provider.ID)
	s.claudeLoginMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	homePath := s.runtime.ClaudeOfficialHome(provider)
	configDir := s.runtime.ClaudeOfficialConfigDir(provider)
	loginCommand := shellQuote(s.cfg.ClaudeCommand) + " auth login --claudeai"
	if email := strings.TrimSpace(provider.Name); looksLikeEmail(email) {
		loginCommand += " --email " + shellQuote(email)
	}
	wrappedCommand := "env HOME=" + shellQuote(homePath) + " CLAUDE_CONFIG_DIR=" + shellQuote(configDir) + " " + loginCommand
	cmd := exec.CommandContext(ctx, "script", "-qefc", wrappedCommand, "/dev/null")
	cmd.Dir = s.cfg.DefaultWorkdir
	cmd.Env = append(os.Environ())

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("create claude login stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("create claude login stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("create claude login stderr pipe: %w", err)
	}

	session := &claudeOfficialLoginSession{
		ProviderID:   provider.ID,
		ProviderName: provider.Name,
		StartedAt:    time.Now().UTC(),
		cmd:          cmd,
		stdin:        stdin,
		cancel:       cancel,
		done:         make(chan struct{}),
		status:       "starting",
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start claude login: %w", err)
	}

	s.claudeLoginMu.Lock()
	if s.claudeLoginSessions == nil {
		s.claudeLoginSessions = make(map[int64]*claudeOfficialLoginSession)
	}
	s.claudeLoginSessions[provider.ID] = session
	s.claudeLoginActiveProviderID = provider.ID
	s.claudeLoginMu.Unlock()

	go s.consumeClaudeOfficialLoginOutput(session, stdout)
	go s.consumeClaudeOfficialLoginOutput(session, stderr)
	go s.waitClaudeOfficialLogin(session)

	if err := s.waitForClaudeOfficialLoginURL(session, 8*time.Second); err != nil {
		s.cancelClaudeOfficialLogin(provider.ID)
		return err
	}
	return nil
}

func (s *Server) completeClaudeOfficialLogin(provider store.Provider, rawInput string) error {
	session := s.getClaudeLoginSession(provider.ID)
	if session == nil || !session.isRunning() {
		return fmt.Errorf("该账号当前没有等待中的浏览器登录会话")
	}

	authCode, err := extractClaudeAuthorizationCode(rawInput)
	if err != nil {
		return err
	}
	if err := session.submitAuthCode(authCode); err != nil {
		return err
	}
	if err := s.waitForClaudeOfficialLoginCompletion(session, 60*time.Second); err != nil {
		return err
	}

	credentialsPath := s.runtime.ClaudeOfficialCredentialsPath(provider)
	if _, err := os.Stat(credentialsPath); err != nil {
		return fmt.Errorf("浏览器认证已结束，但未发现官方账号 .credentials.json: %w", err)
	}
	if sessionStatus, message := session.snapshotStatus(); sessionStatus != "completed" {
		return fmt.Errorf("%s", message)
	}
	s.discardClaudeOfficialLogin(provider.ID)
	return nil
}

func (s *Server) cancelClaudeOfficialLogin(providerID int64) {
	session := s.getClaudeLoginSession(providerID)
	if session == nil {
		return
	}
	session.cancel()
	_ = s.waitForClaudeOfficialLoginCompletion(session, 3*time.Second)
	s.discardClaudeOfficialLogin(providerID)
}

func (s *Server) discardClaudeOfficialLogin(providerID int64) {
	s.claudeLoginMu.Lock()
	defer s.claudeLoginMu.Unlock()
	delete(s.claudeLoginSessions, providerID)
	if s.claudeLoginActiveProviderID == providerID {
		s.claudeLoginActiveProviderID = 0
	}
}

func (s *Server) getClaudeLoginSession(providerID int64) *claudeOfficialLoginSession {
	s.claudeLoginMu.Lock()
	defer s.claudeLoginMu.Unlock()
	if s.claudeLoginSessions == nil {
		return nil
	}
	return s.claudeLoginSessions[providerID]
}

func (s *Server) claudeOfficialAccountState(provider store.Provider) ClaudeOfficialAccountStateView {
	state := ClaudeOfficialAccountStateView{
		HomePath:        s.runtime.ClaudeOfficialHome(provider),
		StatePath:       s.runtime.ClaudeOfficialStatePath(provider),
		CredentialsPath: s.runtime.ClaudeOfficialCredentialsPath(provider),
		SettingsPath:    s.runtime.ClaudeOfficialSettingsPath(provider),
		AuthExists:      authFileExists(s.runtime.ClaudeOfficialCredentialsPath(provider)),
		CanStartLogin:   true,
	}

	session := s.getClaudeLoginSession(provider.ID)
	if session == nil {
		return state
	}

	snapshot := session.snapshot()
	state.LoginInProgress = snapshot.Status == "starting" || snapshot.Status == "awaiting_code" || snapshot.Status == "awaiting_completion"
	state.LoginStatus = snapshot.Status
	state.LoginURL = snapshot.LoginURL
	state.LoginOutput = snapshot.Output
	state.LoginStartedAt = snapshot.StartedAt
	state.AwaitingCode = snapshot.Status == "awaiting_code" && snapshot.LoginURL != ""
	state.CanCancelLogin = state.LoginInProgress
	state.CanSubmitCode = state.AwaitingCode
	state.CanStartLogin = !state.LoginInProgress
	return state
}

func (s *Server) checkClaudeOfficialLogin(provider store.Provider) (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.cfg.ClaudeCommand, "auth", "status")
	cmd.Dir = s.cfg.DefaultWorkdir
	cmd.Env = append(
		os.Environ(),
		"HOME="+s.runtime.ClaudeOfficialHome(provider),
		"CLAUDE_CONFIG_DIR="+s.runtime.ClaudeOfficialConfigDir(provider),
	)
	output, err := cmd.CombinedOutput()

	message := compactWhitespace(string(output))
	if message == "" {
		message = "未返回登录状态输出"
	}
	if ctx.Err() == context.DeadlineExceeded {
		return "error", "官方账号登录状态检查超时"
	}

	var payload struct {
		LoggedIn    bool   `json:"loggedIn"`
		AuthMethod  string `json:"authMethod"`
		APIProvider string `json:"apiProvider"`
	}
	if parseErr := json.Unmarshal(output, &payload); parseErr == nil {
		if payload.LoggedIn && strings.TrimSpace(payload.APIProvider) == "firstParty" {
			return "ok", message
		}
	}
	if err != nil {
		return "error", message
	}
	return "error", message
}

func (s *Server) consumeClaudeOfficialLoginOutput(session *claudeOfficialLoginSession, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		session.recordLine(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		session.recordLine("scanner error: " + err.Error())
	}
}

func (s *Server) waitClaudeOfficialLogin(session *claudeOfficialLoginSession) {
	err := session.cmd.Wait()

	session.mu.Lock()
	defer session.mu.Unlock()

	session.FinishedAt = time.Now().UTC()
	if err == nil {
		session.status = "completed"
	} else if errorsIsContextCanceled(err) {
		session.status = "cancelled"
		session.lastError = "登录流程已取消"
	} else {
		session.status = "error"
		if session.lastError == "" {
			session.lastError = "官方账号登录失败: " + err.Error()
		}
	}

	close(session.done)

	s.claudeLoginMu.Lock()
	if s.claudeLoginActiveProviderID == session.ProviderID {
		s.claudeLoginActiveProviderID = 0
	}
	s.claudeLoginMu.Unlock()
}

func (s *Server) waitForClaudeOfficialLoginURL(session *claudeOfficialLoginSession, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, loginURL := session.currentPrompt()
		switch status {
		case "awaiting_code":
			return nil
		case "error", "cancelled", "completed":
			_, message := session.snapshotStatus()
			return fmt.Errorf("%s", message)
		}
		if strings.TrimSpace(loginURL) != "" {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("未能在限定时间内获取 Claude 官方登录授权链接")
}

func (s *Server) waitForClaudeOfficialLoginCompletion(session *claudeOfficialLoginSession, timeout time.Duration) error {
	select {
	case <-session.done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("等待官方账号登录完成超时")
	}
}

func (s *claudeOfficialLoginSession) isRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status == "starting" || s.status == "awaiting_code" || s.status == "awaiting_completion"
}

func (s *claudeOfficialLoginSession) submitAuthCode(authCode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stdin == nil {
		return fmt.Errorf("当前登录会话不可写入授权码")
	}
	if s.status != "awaiting_code" && s.status != "starting" {
		return fmt.Errorf("当前登录会话不在等待授权码状态")
	}
	if _, err := io.WriteString(s.stdin, strings.TrimSpace(authCode)+"\n"); err != nil {
		return fmt.Errorf("向 Claude 登录会话写入授权码失败: %w", err)
	}
	s.status = "awaiting_completion"
	return nil
}

func (s *claudeOfficialLoginSession) currentPrompt() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status, s.loginURL
}

func (s *claudeOfficialLoginSession) snapshotStatus() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch s.status {
	case "completed":
		return s.status, "官方账号登录完成"
	case "cancelled":
		if s.lastError != "" {
			return s.status, s.lastError
		}
		return s.status, "官方账号登录已取消"
	case "error":
		if s.lastError != "" {
			return s.status, s.lastError
		}
		return s.status, "官方账号登录失败"
	default:
		return s.status, "官方账号登录进行中"
	}
}

func (s *claudeOfficialLoginSession) snapshot() struct {
	Status    string
	LoginURL  string
	Output    string
	StartedAt time.Time
} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return struct {
		Status    string
		LoginURL  string
		Output    string
		StartedAt time.Time
	}{
		Status:    s.status,
		LoginURL:  s.loginURL,
		Output:    strings.TrimSpace(s.output.String()),
		StartedAt: s.StartedAt,
	}
}

func (s *claudeOfficialLoginSession) recordLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.output.Len() > 0 {
		s.output.WriteByte('\n')
	}
	s.output.WriteString(line)

	if match := claudeLoginURLPattern.FindString(line); match != "" {
		s.loginURL = match
		s.status = "awaiting_code"
	}
	if strings.Contains(strings.ToLower(line), "error") && s.lastError == "" {
		s.lastError = line
	}
}

func extractClaudeAuthorizationCode(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("请粘贴 Claude 授权页返回的授权码")
	}
	if parsed, err := url.Parse(raw); err == nil && parsed.Scheme != "" {
		code := strings.TrimSpace(parsed.Query().Get("code"))
		if code != "" {
			return code, nil
		}
	}
	return raw, nil
}

func looksLikeEmail(value string) bool {
	if value == "" {
		return false
	}
	return strings.Contains(value, "@") && !strings.ContainsAny(value, " \t\r\n")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
