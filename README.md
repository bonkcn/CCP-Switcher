# CCP Switcher

CCP Switcher 是一款面向 Linux VPS 的单机 WebUI，用于管理并切换 Claude Code 与 Codex 的供应商配置。系统采用 Go、SQLite 与服务端渲染 HTML 实现，适合部署在仅开放反向代理入口的服务器环境中。

当前版本：`v0.1.0`

## 核心能力

- 供应商配置的新增、编辑、删除与切换
- Claude 与 Codex 配置文件的分离管理
- Base URL、API Key / Token、默认模型等参数的持久化保存
- 连通性测试
- 先拉取模型列表，再选择单个模型执行 API 测试或 CLI 测试
- 切换历史记录与本机配置备份
- Web 登录密码与 Bearer API Token 双重访问控制
- 版本检测与一键更新

## 配置写入原则

### Claude

写入目标：

- `/root/.claude/settings.json`

切换时优先定点替换以下字段：

- `env.ANTHROPIC_BASE_URL`
- `env.ANTHROPIC_AUTH_TOKEN`

以下字段按供应商配置决定是否写入：

- `model`
- `permissions.defaultMode`
- `skipDangerousModePermissionPrompt`
- `env.IS_SANDBOX`

当现有文件已具备兼容字段时，系统优先保留原有文件结构，仅替换关键值；若现有文件缺少必要字段，则首次切换时生成一份托管配置，后续再按定点替换模式工作。

### Codex

写入目标：

- `/root/.codex/config.toml`
- `/root/.codex/auth.json`

切换时优先定点替换以下字段：

- `/root/.codex/auth.json` 中的 `OPENAI_API_KEY`
- `/root/.codex/config.toml` 中 `model_providers.custom.base_url`

以下字段按供应商配置决定是否写入：

- `model`
- `model_reasoning_effort`
- `approval_policy`
- `sandbox_mode`

对于 `approval_policy` 与 `sandbox_mode`，界面支持“保持当前 / 不写入”：

- 若目标配置文件中已存在该字段，则切换时保留原值
- 若目标配置文件首次由系统生成，则不主动补写该字段

## 模型探测机制

模型探测不执行批量测活。标准流程如下：

1. 请求目标站点的 `/models`
2. 从真实返回的模型列表中选择一个模型
3. 对该模型单独执行 API 测试或 CLI 测试

该设计用于避免以下问题：

- 不同网关对模型命名规则不一致
- 大量供应商禁止批量测活
- 同一服务商下不同模型别名可能指向不同路由

### 测试类型

- 连通性测试
  用于排查 Base URL、认证信息及接口可达性问题，不验证具体模型可用性。
- API 测试
  使用所选模型发起一次最小化接口调用，用于确认该模型标识在当前网关下可被正常接受。
- CLI 测试
  使用临时配置目录执行一次最小化真实 CLI 调用，更接近实际使用路径，但仍不等同于所有交互场景、工具调用或长上下文任务均可正常运行。

### 临时测试 Base URL

模型探测浮层允许填写临时测试 Base URL。该地址仅用于本次探测，不会直接覆盖数据库中保存的正式 Base URL。该设计用于兼容以下情况：

- 某些站点拉取模型列表必须使用带 `/v1` 的路径
- 某些站点的 CLI 测试或 API 测试却要求不带 `/v1`

## Codex 执行模式说明

### Approval Policy

- `保持当前 / 不写入`
  切换时不覆盖 `approval_policy`。若现有配置已包含该字段，则保留原值。
- `untrusted`
  仅对受信任命令免审批；模型提出其他命令时需要人工批准。
- `on-failure`
  先直接执行；仅当命令执行失败且需要放宽执行环境时才请求人工批准。该模式在 Codex CLI 帮助输出中已标记为 `DEPRECATED`。
- `on-request`
  是否请求人工批准由模型自行决定。
- `never`
  不请求人工批准；命令执行失败会直接返回给模型处理。

### Sandbox Mode

- `保持当前 / 不写入`
  切换时不覆盖 `sandbox_mode`。若现有配置已包含该字段，则保留原值。
- `read-only`
  模型可读取文件，但不能修改工作区内容。
- `workspace-write`
  模型可写入当前工作区及显式授权的附加目录，工作区之外仍受限制。
- `danger-full-access`
  不启用沙箱限制，模型可直接访问系统。

### 完全放开执行

在日常口语中，`approval_policy="never"` 与 `sandbox_mode="danger-full-access"` 的组合常被称为 “YOLO”。这不是独立配置项，而是上述两个参数的组合状态。

以上定义依据本机 `codex --help` 输出中的说明整理。

## 版本机制

CCP Switcher 使用语义化版本号，格式如下：

- `v<major>.<minor>.<patch>`

版本号来源于仓库根目录的 `VERSION` 文件。设置页中的版本检测逻辑如下：

- 当前版本：读取本地 `VERSION`
- 在线版本：读取 `origin/main` 分支中的 `VERSION`
- 一键更新：重新执行当前仓库中的 `install.sh`，完成代码更新、二进制重建与服务重启

## 一键安装

默认面向 Debian / Ubuntu，要求以 `root` 身份执行：

```bash
curl -fsSL https://raw.githubusercontent.com/bonkcn/CCP-Switcher/main/install.sh | bash
```

默认安装结果：

- 代码目录：`/root/ccp-switcher`
- 数据目录：`/root/.ccp-switcher`
- 二进制：`/usr/local/bin/ccp-switcher`
- systemd 服务名：`ccp-switcher`
- 监听地址：`127.0.0.1:4680`

安装完成后会生成初始凭据文件：

```bash
/root/.ccp-switcher/bootstrap-credentials.txt
```

文件中包含以下两项：

- `PASSWORD`
- `API_TOKEN`

## 手动安装

### 1. 安装基础依赖

```bash
apt-get update
apt-get install -y curl git tar ca-certificates
```

需要 Go `1.25` 或更高版本。

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

### 4. 安装 systemd 服务

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

## 访问方式与反向代理

默认监听地址：

```bash
127.0.0.1:4680
```

该端口默认不直接对外暴露，推荐通过 Nginx、Caddy 或宝塔反向代理访问。

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

如需 HTTPS，应在反向代理层配置证书，CCP Switcher 本身继续监听 `127.0.0.1:4680`。

## 访问控制

系统提供两种访问方式：

- Web 登录密码
- Bearer API Token

浏览器访问时，通常仅需使用 `bootstrap-credentials.txt` 中的 `PASSWORD` 登录。脚本调用可使用如下方式：

```bash
curl -H "Authorization: Bearer <API_TOKEN>" http://127.0.0.1:4680/providers
```

## 路径说明

Claude 配置文件：

- `/root/.claude/settings.json`

Codex 配置文件：

- `/root/.codex/config.toml`
- `/root/.codex/auth.json`

CCP Switcher 数据目录：

- `/root/.ccp-switcher`
- `/root/.ccp-switcher/app.db`
- `/root/.ccp-switcher/master.key`
- `/root/.ccp-switcher/bootstrap-credentials.txt`

## 环境变量

支持以下环境变量：

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
- `internal/store`：SQLite 与加密存储
- `internal/runtime`：Claude / Codex 配置读写、探测与切换逻辑
- `internal/web`：路由、模板与静态资源
- `contrib/ccp-switcher.service`：systemd 服务示例
- `install.sh`：安装与更新脚本
- `VERSION`：当前语义化版本号
