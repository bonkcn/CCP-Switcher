package web

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bonkcn/ccp-switcher/internal/store"
)

var codexLoginURLPattern = regexp.MustCompile(`https://auth\.openai\.com/oauth/authorize\S+`)

type codexOfficialLoginSession struct {
	ProviderID   int64
	ProviderName string
	StartedAt    time.Time
	FinishedAt   time.Time

	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan struct{}

	mu        sync.RWMutex
	status    string
	loginURL  string
	output    strings.Builder
	lastError string
}

type CodexOfficialAccountStateView struct {
	HomePath          string
	AuthPath          string
	AuthExists        bool
	LoginInProgress   bool
	LoginStatus       string
	LoginURL          string
	LoginOutput       string
	LoginStartedAt    time.Time
	AwaitingCallback  bool
	CanStartLogin     bool
	CanCancelLogin    bool
	CanSubmitCallback bool
}

func (s *Server) startCodexOfficialLogin(provider store.Provider) error {
	if provider.Kind != "codex" || provider.Source != store.ProviderSourceOfficial {
		return fmt.Errorf("仅 Codex 官方账号支持浏览器登录编排")
	}
	if err := s.runtime.PrepareCodexOfficialProvider(provider); err != nil {
		return err
	}

	s.codexLoginMu.Lock()
	if s.codexLoginActiveProviderID != 0 && s.codexLoginActiveProviderID != provider.ID {
		active := s.codexLoginSessions[s.codexLoginActiveProviderID]
		name := "另一条会话"
		if active != nil && strings.TrimSpace(active.ProviderName) != "" {
			name = active.ProviderName
		}
		s.codexLoginMu.Unlock()
		return fmt.Errorf("当前已有官方 Codex 登录进行中：%s。请先完成或取消它", name)
	}
	if existing := s.codexLoginSessions[provider.ID]; existing != nil && existing.isRunning() {
		s.codexLoginMu.Unlock()
		return fmt.Errorf("该账号的登录流程已在进行中")
	}
	delete(s.codexLoginSessions, provider.ID)
	s.codexLoginMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(
		ctx,
		s.cfg.CodexCommand,
		"-c", `cli_auth_credentials_store="file"`,
		"login",
	)
	cmd.Dir = s.cfg.DefaultWorkdir
	cmd.Env = append(os.Environ(), "CODEX_HOME="+s.runtime.CodexOfficialHome(provider))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("create codex login stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("create codex login stderr pipe: %w", err)
	}

	session := &codexOfficialLoginSession{
		ProviderID:   provider.ID,
		ProviderName: provider.Name,
		StartedAt:    time.Now().UTC(),
		cmd:          cmd,
		cancel:       cancel,
		done:         make(chan struct{}),
		status:       "starting",
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start codex login: %w", err)
	}

	s.codexLoginMu.Lock()
	if s.codexLoginSessions == nil {
		s.codexLoginSessions = make(map[int64]*codexOfficialLoginSession)
	}
	s.codexLoginSessions[provider.ID] = session
	s.codexLoginActiveProviderID = provider.ID
	s.codexLoginMu.Unlock()

	go s.consumeCodexOfficialLoginOutput(session, stdout)
	go s.consumeCodexOfficialLoginOutput(session, stderr)
	go s.waitCodexOfficialLogin(session)

	if err := s.waitForCodexOfficialLoginURL(session, 8*time.Second); err != nil {
		s.cancelCodexOfficialLogin(provider.ID)
		return err
	}
	return nil
}

func (s *Server) completeCodexOfficialLogin(provider store.Provider, callbackURL string) error {
	session := s.getCodexLoginSession(provider.ID)
	if session == nil || !session.isRunning() {
		return fmt.Errorf("该账号当前没有等待中的浏览器登录会话")
	}

	callbackURL = strings.TrimSpace(callbackURL)
	parsed, err := url.Parse(callbackURL)
	if err != nil {
		return fmt.Errorf("回调地址格式无效: %w", err)
	}
	if parsed.Path != "/auth/callback" {
		return fmt.Errorf("回调地址必须是 /auth/callback")
	}
	if strings.TrimSpace(parsed.RawQuery) == "" {
		return fmt.Errorf("回调地址缺少查询参数")
	}
	query := parsed.Query()
	if query.Get("code") == "" || query.Get("state") == "" {
		return fmt.Errorf("回调地址必须包含 code 和 state")
	}

	forwardURL := "http://127.0.0.1:1455/auth/callback?" + parsed.RawQuery
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Get(forwardURL)
	if err != nil {
		return fmt.Errorf("向本机 Codex 登录回调口转发失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		return fmt.Errorf("官方回调转发失败: %s", message)
	}

	if err := s.waitForCodexOfficialLoginCompletion(session, 20*time.Second); err != nil {
		return err
	}
	if _, err := os.Stat(s.runtime.CodexOfficialAuthPath(provider)); err != nil {
		return fmt.Errorf("浏览器认证已结束，但未发现官方账号 auth.json: %w", err)
	}
	if sessionStatus, message := session.snapshotStatus(); sessionStatus != "completed" {
		return fmt.Errorf("%s", message)
	}
	s.discardCodexOfficialLogin(provider.ID)
	return nil
}

func (s *Server) cancelCodexOfficialLogin(providerID int64) {
	session := s.getCodexLoginSession(providerID)
	if session == nil {
		return
	}
	session.cancel()
	_ = s.waitForCodexOfficialLoginCompletion(session, 3*time.Second)
	s.discardCodexOfficialLogin(providerID)
}

func (s *Server) discardCodexOfficialLogin(providerID int64) {
	s.codexLoginMu.Lock()
	defer s.codexLoginMu.Unlock()
	delete(s.codexLoginSessions, providerID)
	if s.codexLoginActiveProviderID == providerID {
		s.codexLoginActiveProviderID = 0
	}
}

func (s *Server) getCodexLoginSession(providerID int64) *codexOfficialLoginSession {
	s.codexLoginMu.Lock()
	defer s.codexLoginMu.Unlock()
	if s.codexLoginSessions == nil {
		return nil
	}
	return s.codexLoginSessions[providerID]
}

func (s *Server) codexOfficialAccountState(provider store.Provider) CodexOfficialAccountStateView {
	state := CodexOfficialAccountStateView{
		HomePath:      s.runtime.CodexOfficialHome(provider),
		AuthPath:      s.runtime.CodexOfficialAuthPath(provider),
		AuthExists:    authFileExists(s.runtime.CodexOfficialAuthPath(provider)),
		CanStartLogin: true,
	}

	session := s.getCodexLoginSession(provider.ID)
	if session == nil {
		return state
	}

	snapshot := session.snapshot()
	state.LoginInProgress = snapshot.Status == "starting" || snapshot.Status == "awaiting_callback"
	state.LoginStatus = snapshot.Status
	state.LoginURL = snapshot.LoginURL
	state.LoginOutput = snapshot.Output
	state.LoginStartedAt = snapshot.StartedAt
	state.AwaitingCallback = snapshot.Status == "awaiting_callback" && snapshot.LoginURL != ""
	state.CanCancelLogin = state.LoginInProgress
	state.CanSubmitCallback = state.AwaitingCallback
	state.CanStartLogin = !state.LoginInProgress
	return state
}

func (s *Server) checkCodexOfficialLogin(provider store.Provider) (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(
		ctx,
		s.cfg.CodexCommand,
		"-c", `cli_auth_credentials_store="file"`,
		"login", "status",
	)
	cmd.Dir = s.cfg.DefaultWorkdir
	cmd.Env = append(os.Environ(), "CODEX_HOME="+s.runtime.CodexOfficialHome(provider))
	output, err := cmd.CombinedOutput()

	message := compactWhitespace(string(output))
	if message == "" {
		message = "未返回登录状态输出"
	}
	if ctx.Err() == context.DeadlineExceeded {
		return "error", "官方账号登录状态检查超时"
	}
	if err != nil {
		return "error", message
	}
	return "ok", message
}

func (s *Server) consumeCodexOfficialLoginOutput(session *codexOfficialLoginSession, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		session.recordLine(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		session.recordLine("scanner error: " + err.Error())
	}
}

func (s *Server) waitCodexOfficialLogin(session *codexOfficialLoginSession) {
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

	s.codexLoginMu.Lock()
	if s.codexLoginActiveProviderID == session.ProviderID {
		s.codexLoginActiveProviderID = 0
	}
	s.codexLoginMu.Unlock()
}

func (s *Server) waitForCodexOfficialLoginURL(session *codexOfficialLoginSession, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, loginURL := session.currentPrompt()
		switch status {
		case "awaiting_callback":
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
	return fmt.Errorf("未能在限定时间内获取官方登录授权链接")
}

func (s *Server) waitForCodexOfficialLoginCompletion(session *codexOfficialLoginSession, timeout time.Duration) error {
	select {
	case <-session.done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("等待官方账号登录完成超时")
	}
}

func (s *codexOfficialLoginSession) isRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status == "starting" || s.status == "awaiting_callback"
}

func (s *codexOfficialLoginSession) currentPrompt() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status, s.loginURL
}

func (s *codexOfficialLoginSession) snapshotStatus() (string, string) {
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

func (s *codexOfficialLoginSession) snapshot() struct {
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

func (s *codexOfficialLoginSession) recordLine(line string) {
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

	if match := codexLoginURLPattern.FindString(line); match != "" {
		s.loginURL = match
		s.status = "awaiting_callback"
	}
	if strings.Contains(line, "Login cancelled") {
		s.lastError = "登录流程已取消"
	}
	if strings.Contains(strings.ToLower(line), "error") && s.lastError == "" {
		s.lastError = line
	}
}

func errorsIsContextCanceled(err error) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), "signal: killed") {
		return true
	}
	return false
}

func authFileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return true
}

func compactWhitespace(value string) string {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return ""
	}
	return strings.Join(fields, " ")
}
