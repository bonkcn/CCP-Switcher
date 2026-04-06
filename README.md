# CCP Switcher

![CI](https://github.com/bonkcn/CCP-Switcher/actions/workflows/ci.yml/badge.svg)
![Release](https://github.com/bonkcn/CCP-Switcher/actions/workflows/release.yml/badge.svg)

CCP Switcher 是一款集中化管理 Claude Code 与 Codex 编程代理的控制台。基于 Go + SQLite 构建，采用服务端渲染与嵌入式静态资源架构，以极低的资源占用提供中转站/官方账号热切换、环境探测、云端同步及全站配置管理功能，适配 Linux VPS 等无头/边缘部署场景。

## 安装与部署

系统以 `systemd` 守护进程形态运行。请以 `root` 权限执行一键部署脚本：

```bash
curl -fsSL https://raw.githubusercontent.com/bonkcn/CCP-Switcher/main/install.sh | bash
```

安装完成后，服务默认监听 `127.0.0.1:4680`，初始登录凭证生成于：

```
/root/.ccp-switcher/bootstrap-credentials.txt
```

### 远程访问

服务默认仅监听本地回环地址 `127.0.0.1:4680`，提供两种远程暴露方式：

**方式一：内置 HTTPS 自动证书（推荐，适用于干净环境）**

在 WebUI 设置页中配置域名并启用 HTTPS，系统将通过 Let's Encrypt 自动签发 TLS 证书，同时监听 `:80`（ACME challenge + 跳转）和 `:443`（HTTPS），无需额外安装 Nginx / Caddy。要求 80 和 443 端口可用且域名 DNS 已解析到本机。

**方式二：外部反向代理**

适用于已有 Web 服务器的环境，通过 Nginx / Caddy 反代 `127.0.0.1:4680` 并配置 TLS：

```nginx
server {
    listen 80;
    server_name your-domain.example;

    location / {
        proxy_pass http://127.0.0.1:4680;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

> 监听地址可在设置页面动态修改（如改为 `0.0.0.0:4680` 直接暴露），修改后自动重写 systemd 配置并重启服务。

### API Token 鉴权

自动化脚本调用支持 Bearer Token 鉴权：

```bash
curl -H "Authorization: Bearer <API_TOKEN>" http://127.0.0.1:4680/providers
```

## 核心能力

### 四类配置与热切换

- 同时管理四组配置：`Codex 中转站`、`Codex 官方`、`Claude Code 中转站`、`Claude Code 官方`
- 中转站配置持久化保存 Base URL、密钥、默认模型与安全策略
- 官方账号为每个账号维护独立隔离目录，避免多账号登录态互相覆盖
- 基于 AJAX 的无刷新即时切换，活跃供应商状态实时高亮
- 切换时自动备份当前全局配置；Claude 会额外备份 `.claude.json` 与 `.claude/.credentials.json`

### 官方账号网页登录编排

- **Codex 官方账号**：WebUI 启动 `codex login`，本地浏览器完成 OpenAI 授权后，将 `localhost:1455/auth/callback?...` 的完整回调链接粘贴回页面，服务端代转发到服务器本机登录回调口
- **Claude Code 官方账号**：WebUI 启动 `claude auth login --claudeai`，浏览器完成授权后，将页面展示的授权码或完整 URL 粘贴回页面，系统自动提取 `code`
- 官方账号登录成功后即可执行“检查登录”与“切换使用”；切换时同步该账号独立目录中的认证文件到全局 CLI 配置目录
- Claude Code 与 Codex 配置完全隔离，独立管理权限策略、模型和运行环境变量

### 环境探测管线

中转站配置提供三段式验证管线，用于校验异构 API 路由下的供应商可用性：

1. **模型资产拉取** — 向下游网关端点枚举可用模型对象树
2. **API 握手校验** — 以最小化上下文向目标模型发送探测请求
3. **CLI 沙箱验证** — 基于临时隔离配置目录拉起底层二进制组件进行端到端模拟

> 官方订阅账号不走 Base URL / 模型探测，改用登录状态检查后直接切换。

### 云端数据同步

- 支持 WebDAV 与 S3 兼容存储后端（AWS S3 / Cloudflare R2 / MinIO）
- 可配置自动推送间隔，支持手动推送与拉取覆盖
- 同步数据采用与全站备份相同的快照格式，包含供应商、当前生效引用、云同步设置与站点配置

### 全站配置管理

- **网络与接入**：面板内可配监听地址和内置 ACME HTTPS 自动证书，修改后自动联动 systemd 重启
- **站点品牌定制**：自定义站点名称，影响导航栏标识与浏览器标签页标题
- **数据导入导出**：支持供应商级和全站级 JSON 导入导出；保留供应商 `source`、`uid` 与当前生效状态；云同步拉取兼容旧版仅供应商 payload
- **版本监测与热更新**：内置远端源码版本比对与强制覆盖更新机制

## 技术规范

### 配置合并策略

接管执行环境时采用 Merge Patch 策略，仅重写声明字段，保留非冲突原生配置：

| 环境 | 接管目标 | 核心重写字段 |
|------|---------|------------|
| Claude 中转站 | `~/.claude/settings.json` | `env.ANTHROPIC_BASE_URL`、`env.ANTHROPIC_AUTH_TOKEN`、`model`、工作模式修饰符 |
| Claude 官方 | `~/.claude/settings.json` + `~/.claude/.credentials.json` + `~/.claude.json` | 独立 HOME 下的官方登录态、模型与权限配置 |
| Codex 中转站 | `~/.codex/config.toml` + `auth.json` | `OPENAI_API_KEY`、`custom.base_url`、模型/审批/沙箱 |
| Codex 官方 | `~/.codex/config.toml` + `auth.json` | 独立 `CODEX_HOME` 的官方登录态与模型/审批/沙箱 |

### 官方账号隔离目录

- Codex 官方账号目录默认位于 `/root/.ccp-switcher/accounts/codex/<provider-uid>`
- Claude 官方账号目录默认位于 `/root/.ccp-switcher/accounts/claude/<provider-uid>`
- 可通过 `CCP_SWITCHER_CODEX_ACCOUNTS_DIR` 与 `CCP_SWITCHER_CLAUDE_ACCOUNTS_DIR` 覆盖默认位置

### GitHub Actions

- `ci.yml`：在 `main` 分支 push、Pull Request 和手动触发时执行 `gofmt`、`go test ./...`、`go build ./...`
- `release.yml`：在推送匹配 `VERSION` 的 `v*` tag 时构建 Linux `amd64/arm64` 发行包，生成 `tar.gz` 与 `SHA256SUMS.txt` 后发布 GitHub Release

### Codex 安全策略

Codex 供应端支持细粒度安全边界干预：

- **审批策略**：从完全主动审核（`untrusted`）至无人干预执行（`never`）
- **隔离级别**：从强制全局只读（`read-only`）至提权执行（`danger-full-access`）

> 高危组合（`never` + `danger-full-access`）构成全量 Yolo 模式，应当谨慎下发。

---

*系统主数据持久化于 `/root/.ccp-switcher`，官方账号隔离目录默认位于其 `accounts/` 子目录；核心挂载目录与环境变量定义参见 `internal/runtime`。*

### Community
本项目与 [LINUX DO](https://linux.do/) 社区共享。
