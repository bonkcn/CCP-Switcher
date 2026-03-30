# CCP Switcher

CCP Switcher 是一个运行在 Linux VPS 上的单机 WebUI，用于管理和一键切换 Claude Code / Codex 的供应商配置。

它的目标不是“清环境变量再重写一遍 shell”，而是尽量保持配置文件干净、字段兼容，只在切换时替换必要字段：

- Claude: `/root/.claude/settings.json`
- Codex: `/root/.codex/config.toml`
- Codex: `/root/.codex/auth.json`

当前实现：

- Go HTTP 服务
- SQLite 存储
- 服务端渲染 HTML
- systemd 常驻
- Web 密码登录
- Bearer API Token 访问
- 连通性测试
- 单模型探测
- 一键切换
- 切换历史与备份

## 功能概览

- 供应商增删改查
- 启动时自动导入当前本机 Claude / Codex 配置
- 连通性测试只做地址 / 密钥排错
- 模型探测采用两步流：
  1. 先拉取该站点真实模型列表
  2. 再从列表里选择一个模型做 API / CLI 测试
- 探测浮层支持临时自定义测试 Base URL
  适合某些站点 `拉模型要带 /v1`，但 `API / CLI 测试又不能带 /v1` 的场景
- Codex 支持持久化写入：
  - `approval_policy`
  - `sandbox_mode`
  - `model_reasoning_effort`
- Codex 的 `YOLO` 等价配置为：
  - `approval_policy = "never"`
  - `sandbox_mode = "danger-full-access"`

## 一键安装

默认面向 Debian / Ubuntu 类系统，要求 root。

```bash
curl -fsSL https://raw.githubusercontent.com/bonkcn/CCP-Switcher/main/install.sh | bash
```

默认安装结果：

- 代码目录：`/root/ccp-switcher`
- 数据目录：`/root/.ccp-switcher`
- 二进制：`/usr/local/bin/ccp-switcher`
- 服务名：`ccp-switcher`
- 监听地址：`127.0.0.1:4680`

安装完成后会生成初始凭据文件：

```bash
/root/.ccp-switcher/bootstrap-credentials.txt
```

其中包含：

- `PASSWORD=...`
- `API_TOKEN=...`

首次登录 WebUI 时，直接用这里的 `PASSWORD`。

## 手动安装

### 1. 安装依赖

```bash
apt-get update
apt-get install -y curl git tar ca-certificates
```

安装 Go 1.25+。

### 2. 拉取代码

```bash
git clone https://github.com/bonkcn/CCP-Switcher.git /root/ccp-switcher
cd /root/ccp-switcher
```

### 3. 编译

```bash
go build -o /usr/local/bin/ccp-switcher ./cmd/ai-webui
chmod 755 /usr/local/bin/ccp-switcher
```

### 4. 安装 systemd

```bash
cp contrib/ccp-switcher.service /etc/systemd/system/ccp-switcher.service
systemctl daemon-reload
systemctl enable --now ccp-switcher
```

### 5. 查看状态

```bash
systemctl status ccp-switcher --no-pager
journalctl -u ccp-switcher -n 100 --no-pager
```

## 端口与反向代理

默认服务只监听：

```bash
127.0.0.1:4680
```

这意味着：

- 直接外网无法访问 4680
- 应该由 Nginx / Caddy / 宝塔反代到 `127.0.0.1:4680`
- 如果你已经有反代站点，通常只需要把目标地址指向 `127.0.0.1:4680`

### Nginx 例子

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
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

如果要上 HTTPS，请在反代层处理证书即可，CCP Switcher 本身继续监听 `127.0.0.1:4680`。

## 登录与访问方式

Web 访问控制分两层：

- Web 登录密码
- Bearer API Token

默认情况下你主要会用：

- 浏览器打开反代域名
- 输入 `bootstrap-credentials.txt` 里的 `PASSWORD`

如果你要脚本化访问，也可以用：

```bash
curl -H "Authorization: Bearer <API_TOKEN>" http://127.0.0.1:4680/providers
```

## 默认路径

配置文件：

- Claude: `/root/.claude/settings.json`
- Codex: `/root/.codex/config.toml`
- Codex: `/root/.codex/auth.json`

CCP Switcher 自身数据：

- 数据目录：`/root/.ccp-switcher`
- SQLite：`/root/.ccp-switcher/app.db`
- 主密钥：`/root/.ccp-switcher/master.key`
- 启动凭据：`/root/.ccp-switcher/bootstrap-credentials.txt`

## 常用环境变量

兼容性原因，当前仍沿用 `AI_CLI_MANAGER_*` 变量名：

```bash
AI_CLI_MANAGER_LISTEN=127.0.0.1:4680
AI_CLI_MANAGER_DATA_DIR=/root/.ccp-switcher
AI_CLI_MANAGER_CLAUDE_SETTINGS=/root/.claude/settings.json
AI_CLI_MANAGER_CODEX_CONFIG=/root/.codex/config.toml
AI_CLI_MANAGER_CODEX_AUTH=/root/.codex/auth.json
AI_CLI_MANAGER_CLAUDE_CMD=claude
AI_CLI_MANAGER_CODEX_CMD=codex
AI_CLI_MANAGER_WORKDIR=/root
AI_CLI_MANAGER_TMUX_PREFIX=ccp-switcher
```

## 配置切换原则

Claude：

- 优先只替换 `ANTHROPIC_BASE_URL`
- 优先只替换 `ANTHROPIC_AUTH_TOKEN`
- 按需替换已有 `model`
- 按需替换已有 `permissions.defaultMode`

Codex：

- 优先只替换 `OPENAI_API_KEY`
- 优先只替换 `model_providers.custom.base_url`
- 按需替换已有 `model`
- 按需替换已有 `model_reasoning_effort`
- 按需替换已有 `approval_policy`
- 按需替换已有 `sandbox_mode`

如果目标文件不存在，或关键字段不兼容，首次切换会初始化一份托管配置；后续再走定点替换。

## 模型探测说明

模型探测不是批量测活。

它的设计是：

1. 先拉该站点真实返回的 `/models`
2. 从已返回的列表中点选一个模型
3. 对这个模型分别做：
   - API 测试
   - CLI 测试

区别：

- API 测试通过
  只能说明该模型 ID 能完成一次最小 API 请求
- CLI 测试通过
  更接近真实 CLI 调用，但仍不等于所有交互 / 工具调用都绝对可用

## 目录结构

- `cmd/ai-webui`：程序入口
- `internal/store`：SQLite 和加密存储
- `internal/runtime`：Claude / Codex 配置文件适配器
- `internal/web`：路由、登录、模板、静态资源
- `contrib/ccp-switcher.service`：systemd 示例
- `install.sh`：一键安装脚本
