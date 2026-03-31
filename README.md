# CCP Switcher

![CI](https://github.com/bonkcn/CCP-Switcher/actions/workflows/ci.yml/badge.svg)
![Release](https://github.com/bonkcn/CCP-Switcher/actions/workflows/release.yml/badge.svg)

CCP Switcher 是一款专为集中化管理 AI 编程助手（Claude Code 与 Codex）设计的高级网关控制台。系统通过 Go 与 SQLite 构建并采用服务端渲染，以极低的资源占用提供高密度的供应商管理、热插拔切换及环境隔离测试功能，非常适合部署在 Linux VPS 等无头或边缘受限环境中。

当前版本：`v0.2.0`

## 📦 安装与部署

CCP Switcher 默认以后端守护进程形态运行，安装过程会自动注册 `systemd` 面板服务。

### 一键安装 (面向 Debian / Ubuntu)

请以 `root` 权限执行：

```bash
curl -fsSL https://raw.githubusercontent.com/bonkcn/CCP-Switcher/main/install.sh | bash
```

安装完成后，系统将默认在 `127.0.0.1:4680` 启动服务，并在以下路径生成初始系统登录凭证（包含登录密码与 API Token）：
`/root/.ccp-switcher/bootstrap-credentials.txt`

### 访问控制与反向代理

为了保障安全性，服务默认仅监听本地环回地址 `127.0.0.1:4680`。建议通过 Nginx、Caddy 等反向代理接入并配置 TLS 证书。

**Nginx 反代示例：**

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

### API 鉴权

除 Web 面板的密码登录外，脚本自动化调用支持 Bearer Token 鉴权：

```bash
curl -H "Authorization: Bearer <API_TOKEN>" http://127.0.0.1:4680/providers
```

## ✨ 核心特性

- **网关热切与资产管理**：支持多个供应商环境配置（Base URL, API Key, 默认模型）的持久化存储与无缝切换。
- **环境探测器**：提供模型连通性（Ping）、API 握手校验与沙箱 CLI 深度检测的三段式验证管线。
- **配置分离**：Claude 与 Codex 的底层配置彼此独立，实现权限策略与环境变量的精准下发。
- **配置快照与冷备份**：支持全量配置导出为 JSON 及带有冲突重载机制的高可用回放导入。
- **自动化生命周期**：内置版本监测与基于远端源代码的热覆盖更新体系。

## 🔄 持续集成与发布

本项目已接入完整的 GitHub Actions 流水线自动化：

- **CI 工作流**：对 `main` 分支的提交及 Pull Request 操作自动触发代码规范检查与安全构建测试。
- **Release 工作流**：当推送符合 `v*` 标准的语义化标签时，将自动交叉编译 Linux Amd64 与 Arm64 架构的生产级二进制包，并生成 `SHA256SUMS.txt` 校验和发布至 GitHub Releases，支持手动重触发特定版本的交叉编译。

---

## 🛠 技术规范与底层路由

### 模型探测管线

探测功能旨在应对复杂多变的异构 API 路由结构。为避免无效的全面并发探索，采取标准处理管线：

1. **拉取资产**：向下游网关注册端点拉取实际可用的模型对象树。
2. **API 校验**：依靠所选模型发送最小化上下文探测，验证模型标号在目标网关路由下的准确度。
3. **CLI 沙箱验证**：脱离 Web 运行时，基于临时隔离配置目录拉起底层的被管二进制组件，进行端到端的模拟测试。

### 核心配置合并策略

接管执行环境时，CCP Switcher 采用“明确字段重写（Merge Patch）”策略介入：

**Claude 环境：**  
接管注入 `/root/.claude/settings.json`。重写 `env.ANTHROPIC_BASE_URL`、`env.ANTHROPIC_AUTH_TOKEN`、`model` 及相关工作模式修饰符等特定节点，非冲突原生配置项将被自动保留。

**Codex 环境：**  
接管介入 `/root/.codex/config.toml` 与 `/root/.codex/auth.json`。核心接管 `OPENAI_API_KEY` 与配置文件结构中的 `base_url` 路由。

### Codex 安全策略审计

Codex 供应端表单支持细粒度干预安全边界：

- **审批策略 (Approval Policy)**：从需要完全主动审核 (`untrusted`) 至无人干预无头执行 (`never`)。
- **隔离级别 (Sandbox Mode)**：从强制全局只读 (`read-only`)，至工作区读写约束 (`workspace-write`) 以及高危的提权执行 (`danger-full-access`)。
- 注：高危险级别的越权和免密组合应当谨慎在服务端下发。

*内部核心挂载目录、环境变量定义可通过解构源码 `internal/runtime` 获悉。系统主数据库持久化于 `/root/.ccp-switcher`。*
