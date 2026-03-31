package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bonkcn/ccp-switcher/internal/app"
)

var semverPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

type versionStatusView struct {
	RepoDir           string
	OriginURL         string
	InstallScriptPath string
	VersionFilePath   string
	CanUpdate         bool
	Ready             bool
	RepoError         string
	LocalBranch       string
	LocalVersion      string
	LocalDirty        bool
	RemoteChecked     bool
	RemoteVersion     string
	RemoteError       string
	UpdateAvailable   bool
}

type semanticVersion struct {
	Major int
	Minor int
	Patch int
	Raw   string
}

func inspectVersionStatus(repoDir string, checkRemote bool) *versionStatusView {
	trimmedRepoDir := strings.TrimSpace(repoDir)
	status := &versionStatusView{
		RepoDir:           trimmedRepoDir,
		InstallScriptPath: filepath.Join(trimmedRepoDir, "install.sh"),
		VersionFilePath:   filepath.Join(trimmedRepoDir, "VERSION"),
	}
	if trimmedRepoDir == "" {
		status.RepoError = "未识别当前代码目录，无法执行版本检测。"
		return status
	}

	status.CanUpdate = fileExists(status.InstallScriptPath)
	status.LocalVersion = readVersionValue(status.VersionFilePath)
	if status.LocalVersion == "" {
		status.RepoError = "未找到有效的 VERSION 文件。"
		return status
	}
	if _, err := parseSemanticVersion(status.LocalVersion); err != nil {
		status.RepoError = "本地 VERSION 文件格式无效: " + err.Error()
		return status
	}

	if _, err := os.Stat(filepath.Join(trimmedRepoDir, ".git")); err != nil {
		status.RepoError = "当前代码目录不是 git 仓库，无法执行在线更新。"
		return status
	}

	status.Ready = true
	if output, err := gitOutput(status.RepoDir, 3*time.Second, "branch", "--show-current"); err == nil {
		status.LocalBranch = output
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
	remoteVersion, err := fetchRemoteVersion(status.RepoDir)
	if err != nil {
		status.RemoteError = err.Error()
		return status
	}
	status.RemoteVersion = remoteVersion

	cmp, err := compareSemanticVersion(remoteVersion, status.LocalVersion)
	if err != nil {
		status.RemoteError = "比较版本失败: " + err.Error()
		return status
	}
	status.UpdateAvailable = cmp > 0
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

func fetchRemoteVersion(repoDir string) (string, error) {
	if _, err := gitOutput(repoDir, 12*time.Second, "fetch", "--depth=1", "origin", "main"); err != nil {
		return "", fmt.Errorf("检查远端版本失败: %w", err)
	}
	output, err := gitOutput(repoDir, 5*time.Second, "show", "FETCH_HEAD:VERSION")
	if err != nil {
		return "", fmt.Errorf("读取远端 VERSION 失败: %w", err)
	}
	version := strings.TrimSpace(output)
	if version == "" {
		return "", errors.New("远端 VERSION 文件为空")
	}
	if _, err := parseSemanticVersion(version); err != nil {
		return "", fmt.Errorf("远端 VERSION 文件格式无效: %w", err)
	}
	return version, nil
}

func readVersionValue(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}

func parseSemanticVersion(value string) (semanticVersion, error) {
	matches := semverPattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(matches) != 4 {
		return semanticVersion{}, errors.New("必须采用 v<major>.<minor>.<patch> 格式")
	}
	major, _ := strconv.Atoi(matches[1])
	minor, _ := strconv.Atoi(matches[2])
	patch, _ := strconv.Atoi(matches[3])
	return semanticVersion{
		Major: major,
		Minor: minor,
		Patch: patch,
		Raw:   matches[0],
	}, nil
}

func compareSemanticVersion(left string, right string) (int, error) {
	leftVersion, err := parseSemanticVersion(left)
	if err != nil {
		return 0, err
	}
	rightVersion, err := parseSemanticVersion(right)
	if err != nil {
		return 0, err
	}

	switch {
	case leftVersion.Major != rightVersion.Major:
		return compareInt(leftVersion.Major, rightVersion.Major), nil
	case leftVersion.Minor != rightVersion.Minor:
		return compareInt(leftVersion.Minor, rightVersion.Minor), nil
	default:
		return compareInt(leftVersion.Patch, rightVersion.Patch), nil
	}
}

func compareInt(left int, right int) int {
	switch {
	case left > right:
		return 1
	case left < right:
		return -1
	default:
		return 0
	}
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
