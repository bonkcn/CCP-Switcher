package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bonkcn/ccp-switcher/internal/app"
)

type versionStatusView struct {
	RepoDir           string
	OriginURL         string
	InstallScriptPath string
	CanUpdate         bool
	Ready             bool
	RepoError         string
	LocalBranch       string
	LocalCommit       string
	LocalShort        string
	LocalCommittedAt  string
	LocalDirty        bool
	RemoteChecked     bool
	RemoteCommit      string
	RemoteShort       string
	RemoteError       string
	UpdateAvailable   bool
}

func inspectVersionStatus(repoDir string, checkRemote bool) *versionStatusView {
	status := &versionStatusView{
		RepoDir:           strings.TrimSpace(repoDir),
		InstallScriptPath: filepath.Join(strings.TrimSpace(repoDir), "install.sh"),
	}
	if status.RepoDir == "" {
		status.RepoError = "未识别当前代码目录，无法执行版本检测。"
		return status
	}
	if _, err := os.Stat(filepath.Join(status.RepoDir, ".git")); err != nil {
		status.RepoError = "当前工作目录不是 git 仓库，无法执行版本检测。"
		return status
	}

	status.Ready = true
	status.CanUpdate = fileExists(status.InstallScriptPath)
	if output, err := gitOutput(status.RepoDir, 3*time.Second, "branch", "--show-current"); err == nil {
		status.LocalBranch = output
	}
	commit, err := gitOutput(status.RepoDir, 3*time.Second, "rev-parse", "HEAD")
	if err != nil {
		status.Ready = false
		status.RepoError = "读取当前版本失败: " + err.Error()
		return status
	}
	status.LocalCommit = commit
	status.LocalShort = shortCommit(commit)

	if output, err := gitOutput(status.RepoDir, 3*time.Second, "log", "-1", "--format=%cI"); err == nil {
		status.LocalCommittedAt = formatGitTime(output)
	}
	if output, err := gitOutput(status.RepoDir, 3*time.Second, "status", "--porcelain"); err == nil {
		status.LocalDirty = strings.TrimSpace(output) != ""
	}
	if output, err := gitOutput(status.RepoDir, 3*time.Second, "config", "--get", "remote.origin.url"); err == nil {
		status.OriginURL = output
	}

	if !checkRemote {
		return status
	}

	status.RemoteChecked = true
	output, err := gitOutput(status.RepoDir, 8*time.Second, "ls-remote", "--heads", "origin", "main")
	if err != nil {
		status.RemoteError = "检查 origin/main 失败: " + err.Error()
		return status
	}

	fields := strings.Fields(output)
	if len(fields) == 0 {
		status.RemoteError = "origin/main 未返回可解析的提交信息。"
		return status
	}
	status.RemoteCommit = fields[0]
	status.RemoteShort = shortCommit(fields[0])
	status.UpdateAvailable = status.LocalCommit != "" && status.RemoteCommit != "" && status.LocalCommit != status.RemoteCommit
	return status
}

func triggerSelfUpdate(cfg app.Config, status *versionStatusView) (string, error) {
	if status == nil || !status.Ready {
		return "", errors.New("当前代码目录不可更新")
	}
	if !status.CanUpdate {
		return "", fmt.Errorf("未找到安装脚本: %s", status.InstallScriptPath)
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return "", fmt.Errorf("未找到 systemd-run: %w", err)
	}

	unitName := fmt.Sprintf("ccp-switcher-update-%d", time.Now().Unix())
	args := []string{
		"--unit=" + unitName,
		"--collect",
		"--property=WorkingDirectory=" + status.RepoDir,
		"--setenv=REPO_DIR=" + status.RepoDir,
		"--setenv=DATA_DIR=" + cfg.DataDir,
		"--setenv=LISTEN_ADDR=" + cfg.ListenAddr,
	}
	if status.OriginURL != "" {
		args = append(args, "--setenv=REPO_URL="+status.OriginURL)
	}
	args = append(args, status.InstallScriptPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "systemd-run", args...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", errors.New("更新任务提交超时")
	}
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", errors.New(message)
	}
	return unitName, nil
}

func gitOutput(repoDir string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoDir
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", errors.New("git 命令超时")
	}
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", errors.New(message)
	}
	return strings.TrimSpace(string(output)), nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func shortCommit(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func formatGitTime(value string) string {
	timestamp, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return strings.TrimSpace(value)
	}
	return timestamp.Local().Format("2006-01-02 15:04:05 MST")
}
