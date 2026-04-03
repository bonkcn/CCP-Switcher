# CCP Switcher

![CI](https://github.com/bonkcn/CCP-Switcher/actions/workflows/ci.yml/badge.svg)
![Release](https://github.com/bonkcn/CCP-Switcher/actions/workflows/release.yml/badge.svg)

CCP Switcher 是一款集中化管理 Claude Code 与 Codex 编程代理的网关控制台。基于 Go + SQLite 构建，采用服务端渲染与嵌入式静态资源架构，以极低的资源占用提供供应商热切换、环境探测、云端同步及全站配置管理功能，适配 Linux VPS 等无头/边缘部署场景。

## 安装与部署

系统以 `systemd` 守护进程形态运行。请以 `root` 权限执行一键部署脚本：

```bash
curl -fsSL https://raw.githubusercontent.com/bonkcn/CCP-Switcher/main/install.sh | bash
```

安装完成后，服务默认监听 `127.0.0.1:4680`，初始登录凭证生成于：

```
/root/.ccp-switcher/bootstrap-credentials.txt
```

### 反向代理接入

服务仅监听本地回环地址，生产环境需通过 Nginx / Caddy 等反向代理暴露并配置 TLS。

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

### API Token 鉴权

自动化脚本调用支持 Bearer Token 鉴权：

```bash
curl -H "Authorization: Bearer <API_TOKEN>" http://127.0.0.1:4680/providers
```

## 核心能力

### 供应商管理与热切换

- 多供应商配置持久化（Base URL、API Key、默认模型、安全策略）
- 基于 AJAX 的无刷新即时切换，活跃供应商状态实时高亮
- Claude Code 与 Codex 配置完全隔离，独立管理权限策略与环境变量

### 环境探测管线

三段式验证管线，用于校验异构 API 路由下的供应商可用性：

1. **模型资产拉取** — 向下游网关端点枚举可用模型对象树
2. **API 握手校验** — 以最小化上下文向目标模型发送探测请求
3. **CLI 沙箱验证** — 基于临时隔离配置目录拉起底层二进制组件进行端到端模拟

### 云端数据同步

- 支持 WebDAV 与 S3 兼容存储后端（AWS S3 / Cloudflare R2 / MinIO）
- 可配置自动推送间隔，支持手动推送与拉取覆盖
- 同步数据包含完整供应商配置，适用于多设备间状态共享

### 全站配置管理

- **站点品牌定制**：自定义站点名称，影响导航栏标识与浏览器标签页标题
- **数据导入导出**：支持供应商级和全站级（含供应商、云同步设置、站点配置）的 JSON 格式导入导出
- **版本监测与热更新**：内置远端源码版本比对与强制覆盖更新机制

## 技术规范

### 配置合并策略

接管执行环境时采用 Merge Patch 策略，仅重写声明字段，保留非冲突原生配置：

| 环境 | 接管目标 | 核心重写字段 |
|------|---------|------------|
| Claude | `~/.claude/settings.json` | `env.ANTHROPIC_BASE_URL`、`env.ANTHROPIC_AUTH_TOKEN`、`model`、工作模式修饰符 |
| Codex | `~/.codex/config.toml` + `auth.json` | `OPENAI_API_KEY`、`base_url` 路由 |

### Codex 安全策略

Codex 供应端支持细粒度安全边界干预：

- **审批策略**：从完全主动审核（`untrusted`）至无人干预执行（`never`）
- **隔离级别**：从强制全局只读（`read-only`）至提权执行（`danger-full-access`）

> 高危组合（`never` + `danger-full-access`）构成全量 Yolo 模式，应当谨慎下发。

---

*系统主数据持久化于 `/root/.ccp-switcher`，核心挂载目录与环境变量定义参见 `internal/runtime`。*
