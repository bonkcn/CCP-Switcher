#!/usr/bin/env bash

set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/bonkcn/CCP-Switcher.git}"
REPO_DIR="${REPO_DIR:-/root/ccp-switcher}"
DATA_DIR="${DATA_DIR:-/root/.ccp-switcher}"
BIN_PATH="${BIN_PATH:-/usr/local/bin/ccp-switcher}"
SERVICE_PATH="${SERVICE_PATH:-/etc/systemd/system/ccp-switcher.service}"
LISTEN_ADDR="${LISTEN_ADDR:-127.0.0.1:4680}"
GO_VERSION="${GO_VERSION:-1.25.0}"

if [[ "${EUID}" -ne 0 ]]; then
  echo "请用 root 运行此脚本。"
  exit 1
fi

log() {
  printf '[ccp-switcher] %s\n' "$*"
}

version_ge() {
  [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | tail -n1)" == "$1" ]]
}

ensure_base_packages() {
  local missing=0
  for cmd in curl git tar systemctl; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      missing=1
      break
    fi
  done

  if [[ "${missing}" -eq 1 ]]; then
    log "安装基础依赖..."
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y curl git tar ca-certificates
  fi
}

detect_go_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *)
      echo "不支持的架构: $(uname -m)" >&2
      exit 1
      ;;
  esac
}

install_go_if_needed() {
  local current_version=""
  if command -v go >/dev/null 2>&1; then
    current_version="$(go version | awk '{print $3}' | sed 's/^go//')"
  elif [[ -x /usr/local/go/bin/go ]]; then
    current_version="$(/usr/local/go/bin/go version | awk '{print $3}' | sed 's/^go//')"
    export PATH="/usr/local/go/bin:${PATH}"
  fi

  if [[ -n "${current_version}" ]] && version_ge "${current_version}" "1.25.0"; then
    log "已检测到 Go ${current_version}"
    return
  fi

  local arch
  arch="$(detect_go_arch)"
  local tarball="go${GO_VERSION}.linux-${arch}.tar.gz"
  local download_url="https://go.dev/dl/${tarball}"

  log "安装 Go ${GO_VERSION}..."
  rm -rf /tmp/ccp-switcher-install
  mkdir -p /tmp/ccp-switcher-install
  curl -fsSL "${download_url}" -o "/tmp/ccp-switcher-install/${tarball}"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "/tmp/ccp-switcher-install/${tarball}"
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  export PATH="/usr/local/go/bin:${PATH}"
}

checkout_repo() {
  if [[ -d "${REPO_DIR}/.git" ]]; then
    log "更新现有代码目录 ${REPO_DIR}"
    git -C "${REPO_DIR}" fetch --depth=1 origin main
    git -C "${REPO_DIR}" checkout -f main
    git -C "${REPO_DIR}" reset --hard origin/main
    return
  fi

  log "拉取仓库到 ${REPO_DIR}"
  rm -rf "${REPO_DIR}"
  git clone --depth=1 "${REPO_URL}" "${REPO_DIR}"
}

build_binary() {
  log "编译二进制..."
  cd "${REPO_DIR}"
  go build -o "${BIN_PATH}" ./cmd/ai-webui
  chmod 755 "${BIN_PATH}"
}

install_service() {
  log "写入 systemd 服务..."
  mkdir -p "${DATA_DIR}"
  cat > "${SERVICE_PATH}" <<EOF
[Unit]
Description=CCP Switcher WebUI
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=${REPO_DIR}
ExecStart=${BIN_PATH}
Restart=always
RestartSec=3
Environment=CCP_SWITCHER_LISTEN=${LISTEN_ADDR}
Environment=CCP_SWITCHER_DATA_DIR=${DATA_DIR}

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable --now ccp-switcher
}

print_summary() {
  cat <<EOF

CCP Switcher 已安装完成。

服务名:
  ccp-switcher

监听地址:
  ${LISTEN_ADDR}

数据目录:
  ${DATA_DIR}

首次密码 / API Token 文件:
  ${DATA_DIR}/bootstrap-credentials.txt

查看服务状态:
  systemctl status ccp-switcher --no-pager

查看日志:
  journalctl -u ccp-switcher -n 100 --no-pager
EOF
}

ensure_base_packages
install_go_if_needed
checkout_repo
build_binary
install_service
print_summary
