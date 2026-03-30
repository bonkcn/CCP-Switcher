# CCP Switcher

CCP Switcher 是一个主要运行在 Linux VPS 上的单机 WebUI，用来管理并一键切换 Claude Code / Codex 的供应商配置。

技术栈：

- Go
- SQLite
- 服务端渲染 HTML
- systemd 常驻服务

核心能力：

- Web 密码登录
- Bearer API Token
- Claude / Codex 供应商分开管理
- Base URL / API Key 保存与切换
- 连通性测试
- 先拉模型列表，再选模型做单测
- API 测试 + CLI 测试
- 切换历史与配置备份
- WebUI 版本检测
- WebUI 一键更新

## 功能说明

### Claude

切换时优先定点替换：

- `ANTHROPIC_BASE_URL`
- `ANTHROPIC_AUTH_TOKEN`

按需维护：

- `model`
- `permissions.defaultMode`
- `skipDangerousModePermissionPrompt`
- `IS_SANDBOX=1`

目标文件：

- `/root/.claude/settings.json`

### Codex

切换时优先定点替换：

- `/root/.codex/auth.json` 中的 `OPENAI_API_KEY`
- `/root/.codex/config.toml` 中的 `model_providers.custom.base_url`

按需维护：

- `model`
- `model_reasoning_effort`
- `approval_policy`
- `sandbox_mode`

目标文件：

- `/root/.codex/config.toml`
- `/root/.codex/auth.json`

Codex 的 `YOLO` 等价写法：

- `approval_policy = "never"`
- `sandbox_mode = "danger-full-access"`

## 模型探测逻辑

模型探测不是批量测活。

固定流程：

1. 先请求目标站点的 `/models`
2. 从真实返回的模型列表里选一个模型
3. 对这个模型单独做 API 测试或 CLI 测试

这意味着：

- 不会批量扫几百个模型
- 不会随机拿用户手填模型名去测
- 复制按钮只针对该站点真实返回的模型名

测试说明：

- 连通性测试
  只排查 Base URL 是否写错、密钥是否明显错误、站点是否能连通
- API 测试
  只验证这个模型 ID 能否完成一次最小 API 请求
- CLI 测试
  走一次最小化真实 CLI 调用，结果更接近实际使用，但仍不代表所有高级工具调用都 100% 可用

额外说明：

- 某些站点拉取模型列表必须带 `/v1`
- 某些站点 CLI/API 测试又不接受 `/v1`
- 因此模型探测浮层允许填写一个临时测试 Base URL，不会直接改掉你保存的正式 Base URL

## 一键安装

默认面向 Debian / Ubuntu，要求 `root`。

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

首次登录 WebUI 时，用这里的 `PASSWORD` 即可。

## 手动安装

### 1. 安装依赖

```bash
apt-get update
apt-get install -y curl git tar ca-certificates
```

需要 Go `1.25+`。

### 2. 拉取代码

```bash
git clone https://github.com/bonkcn/CCP-Switcher.git /root/ccp-switcher
cd /root/ccp-switcher
```

### 3. 编译

```bash
GOTOOLCHAIN=go1.25.0 go build -o /usr/local/bin/ccp-switcher ./cmd/ai-webui
chmod 755 /usr/local/bin/ccp-switcher
```

### 4. 安装服务

```bash
cp contrib/ccp-switcher.service /etc/systemd/system/ccp-switcher.service
systemctl daemon-reload
systemctl enable --now ccp-switcher
```

### 5. 检查状态

```bash
systemctl status ccp-switcher --no-pager
journalctl -u ccp-switcher -n 100 --no-pager
```

## 访问方式与反代

默认只监听：

```bash
127.0.0.1:4680
```

这表示：

- 外网不能直接访问 4680
- 正常部署方式是用 Nginx / Caddy / 宝塔反代到 `127.0.0.1:4680`
- 只要你的反代域名能转发到这个地址即可

### Nginx 示例

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

如果你要 HTTPS，在反代层配证书即可，CCP Switcher 本身继续监听 `127.0.0.1:4680`。

## 登录与 API Token

Web 访问控制有两层：

- 登录密码
- Bearer API Token

浏览器访问时通常只需要：

1. 打开你的反代域名
2. 输入 `bootstrap-credentials.txt` 里的 `PASSWORD`

脚本调用时可以使用：

```bash
curl -H "Authorization: Bearer <API_TOKEN>" http://127.0.0.1:4680/providers
```

## 版本检测与一键更新

设置页内提供：

- 当前本地版本显示
- `origin/main` 最新版本检查
- 一键更新

一键更新实际动作：

1. 用当前仓库的 `origin` 作为更新源
2. 重新执行当前仓库里的 `install.sh`
3. 重新编译二进制
4. 自动重启 `ccp-switcher` 服务

注意：

- 更新会覆盖当前仓库里的未提交修改
- 更新期间页面可能会短暂断开
- 更新完成后建议回到设置页再点一次“检查更新”

## 默认路径

Claude 配置：

- `/root/.claude/settings.json`

Codex 配置：

- `/root/.codex/config.toml`
- `/root/.codex/auth.json`

CCP Switcher 自身数据：

- 数据目录：`/root/.ccp-switcher`
- SQLite：`/root/.ccp-switcher/app.db`
- 主密钥：`/root/.ccp-switcher/master.key`
- 启动凭据：`/root/.ccp-switcher/bootstrap-credentials.txt`

## 环境变量

所有常用环境变量已经统一为 `CCP_SWITCHER_*`：

```bash
CCP_SWITCHER_LISTEN=127.0.0.1:4680
CCP_SWITCHER_DATA_DIR=/root/.ccp-switcher
CCP_SWITCHER_DB_PATH=/root/.ccp-switcher/app.db
CCP_SWITCHER_MASTER_KEY_PATH=/root/.ccp-switcher/master.key
CCP_SWITCHER_BOOTSTRAP_PATH=/root/.ccp-switcher/bootstrap-credentials.txt
CCP_SWITCHER_CLAUDE_SETTINGS=/root/.claude/settings.json
CCP_SWITCHER_CODEX_CONFIG=/root/.codex/config.toml
CCP_SWITCHER_CODEX_AUTH=/root/.codex/auth.json
CCP_SWITCHER_CLAUDE_CMD=claude
CCP_SWITCHER_CODEX_CMD=codex
CCP_SWITCHER_WORKDIR=/root
CCP_SWITCHER_TMUX_PREFIX=ccp-switcher
```

## 目录结构

- `cmd/ai-webui`：程序入口
- `internal/store`：SQLite 和加密存储
- `internal/runtime`：Claude / Codex 配置文件适配与测试逻辑
- `internal/web`：路由、模板、静态资源
- `contrib/ccp-switcher.service`：systemd 示例
- `install.sh`：一键安装与更新脚本
