#!/usr/bin/env bash

set -u
set -o pipefail

SCRIPT_NAME="$(basename "$0")"
MANAGER_DIR="${HOME}/.ccp-switcher"
BACKUP_DIR="${MANAGER_DIR}/backups"
CLAUDE_BASHRC_START="# >>> AI_CLI_MANAGER_CLAUDE >>>"
CLAUDE_BASHRC_END="# <<< AI_CLI_MANAGER_CLAUDE <<<"
CODEX_BASHRC_START="# >>> AI_CLI_MANAGER_CODEX >>>"
CODEX_BASHRC_END="# <<< AI_CLI_MANAGER_CODEX <<<"
CODEX_CONFIG_START="# >>> AI_CLI_MANAGER_CODEX_PROVIDER >>>"
CODEX_CONFIG_END="# <<< AI_CLI_MANAGER_CODEX_PROVIDER <<<"

mkdir -p "$MANAGER_DIR" "$BACKUP_DIR"

INPUT_FD=0
HAS_INTERACTIVE_INPUT=1
if [ ! -t 0 ]; then
  if [ -t 1 ] && [ -r /dev/tty ]; then
    INPUT_FD=9
    exec 9</dev/tty
  else
    HAS_INTERACTIVE_INPUT=0
  fi
fi

if [ "$(id -u)" -eq 0 ]; then
  SUDO=""
elif command -v sudo >/dev/null 2>&1; then
  SUDO="sudo"
else
  echo "此脚本在安装软件时需要 root 权限。"
  exit 1
fi

log() {
  printf '%s\n' "$*"
}

success() {
  printf '[OK] %s\n' "$*"
}

warn() {
  printf '[WARN] %s\n' "$*"
}

error() {
  printf '[ERR] %s\n' "$*" >&2
}

pause() {
  printf '\n按回车继续...'
  if ! read -r -u "$INPUT_FD" _; then
    printf '\n'
    exit 0
  fi
}

backup_file() {
  local file="$1"
  if [ -f "$file" ]; then
    local base ts
    base="$(basename "$file")"
    ts="$(date +%Y%m%d-%H%M%S)"
    cp "$file" "${BACKUP_DIR}/${base}.${ts}.bak"
  fi
}

ensure_parent_dir() {
  local file="$1"
  mkdir -p "$(dirname "$file")"
}

strip_block() {
  local file="$1"
  local start_marker="$2"
  local end_marker="$3"
  [ -f "$file" ] || return 0

  awk -v start="$start_marker" -v end="$end_marker" '
    $0 == start { skip = 1; next }
    $0 == end { skip = 0; next }
    !skip { print }
  ' "$file" > "${file}.tmp" && mv "${file}.tmp" "$file"
}

append_block() {
  local file="$1"
  local start_marker="$2"
  local end_marker="$3"
  local block="$4"

  ensure_parent_dir "$file"
  touch "$file"
  strip_block "$file" "$start_marker" "$end_marker"

  if [ -s "$file" ] && [ -n "$(tail -c 1 "$file" 2>/dev/null)" ]; then
    printf '\n' >> "$file"
  fi

  {
    printf '%s\n' "$start_marker"
    printf '%s\n' "$block"
    printf '%s\n' "$end_marker"
  } >> "$file"
}

prompt_required() {
  local label="$1"
  local value=""
  while [ -z "$value" ]; do
    printf '%s: ' "$label" >&2
    read -r -u "$INPUT_FD" value
    if [ -z "$value" ]; then
      warn "$label 不能为空。" >&2
    fi
  done
  printf '%s' "$value"
}

prompt_default() {
  local label="$1"
  local default_value="$2"
  local value=""
  printf '%s [%s]: ' "$label" "$default_value" >&2
  read -r -u "$INPUT_FD" value
  if [ -z "$value" ]; then
    value="$default_value"
  fi
  printf '%s' "$value"
}

json_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  s="${s//$'\n'/\\n}"
  s="${s//$'\r'/\\r}"
  s="${s//$'\t'/\\t}"
  printf '%s' "$s"
}

shell_escape() {
  printf '%q' "$1"
}

claude_env_exports_present() {
  local file="$1"
  grep -Eq '^[[:space:]]*export ANTHROPIC_(AUTH_TOKEN|BASE_URL)=' "$file" 2>/dev/null
}

strip_claude_env_exports() {
  local file="$1"
  [ -f "$file" ] || return 0

  awk '
    /^[[:space:]]*export ANTHROPIC_AUTH_TOKEN=/ { next }
    /^[[:space:]]*export ANTHROPIC_BASE_URL=/ { next }
    { print }
  ' "$file" > "${file}.tmp" && mv "${file}.tmp" "$file"
}

reload_bashrc() {
  local had_nounset=0
  if [[ $- == *u* ]]; then
    had_nounset=1
    set +u
  fi

  # shellcheck disable=SC1090
  if . "${HOME}/.bashrc"; then
    [ "$had_nounset" -eq 0 ] || set -u
    success "~/.bashrc 已重新加载"
    return 0
  fi

  [ "$had_nounset" -eq 0 ] || set -u
  warn "已写入 ~/.bashrc，但重新加载失败，请手动执行: source ~/.bashrc"
  return 1
}

NODEJS_MAJOR=22

command_exists() {
  command -v "$1" >/dev/null 2>&1
}

download_to_stdout() {
  local url="$1"
  if command_exists curl; then
    curl -fsSL "$url"
    return $?
  fi
  if command_exists wget; then
    wget -qO- "$url"
    return $?
  fi
  return 127
}

download_to_file() {
  local url="$1"
  local output="$2"
  if command_exists curl; then
    curl -fsSL "$url" -o "$output"
    return $?
  fi
  if command_exists wget; then
    wget -qO "$output" "$url"
    return $?
  fi
  return 127
}

detect_package_manager() {
  local pm=""
  for pm in apt-get dnf yum apk pacman zypper; do
    if command_exists "$pm"; then
      printf '%s' "$pm"
      return 0
    fi
  done
  return 1
}

install_prerequisites() {
  local pm="$1"
  case "$pm" in
    apt-get)
      $SUDO apt-get update
      $SUDO apt-get install -y ca-certificates curl gnupg xz-utils
      ;;
    dnf)
      $SUDO dnf install -y ca-certificates curl gnupg2 xz
      ;;
    yum)
      $SUDO yum install -y ca-certificates curl gnupg2 xz
      ;;
    apk)
      $SUDO apk add --no-cache ca-certificates curl xz
      ;;
    pacman)
      $SUDO pacman -Sy --noconfirm ca-certificates curl xz
      ;;
    zypper)
      $SUDO zypper --non-interactive install ca-certificates curl xz
      ;;
    *)
      return 1
      ;;
  esac
}

install_node_via_package_manager() {
  local pm="$1"
  case "$pm" in
    apt-get)
      install_prerequisites "$pm" || return 1
      download_to_stdout "https://deb.nodesource.com/setup_${NODEJS_MAJOR}.x" | $SUDO bash -
      $SUDO apt-get install -y nodejs
      ;;
    dnf)
      install_prerequisites "$pm" || return 1
      download_to_stdout "https://rpm.nodesource.com/setup_${NODEJS_MAJOR}.x" | $SUDO bash -
      $SUDO dnf install -y nodejs
      ;;
    yum)
      install_prerequisites "$pm" || return 1
      download_to_stdout "https://rpm.nodesource.com/setup_${NODEJS_MAJOR}.x" | $SUDO bash -
      $SUDO yum install -y nodejs
      ;;
    apk)
      install_prerequisites "$pm" || return 1
      $SUDO apk add --no-cache nodejs npm
      ;;
    pacman)
      install_prerequisites "$pm" || return 1
      $SUDO pacman -Sy --noconfirm nodejs npm
      ;;
    zypper)
      install_prerequisites "$pm" || return 1
      $SUDO zypper --non-interactive install nodejs22 npm22 || \
        $SUDO zypper --non-interactive install nodejs npm
      ;;
    *)
      return 1
      ;;
  esac
}

get_node_arch() {
  case "$(uname -m)" in
    x86_64|amd64)
      printf 'x64'
      ;;
    aarch64|arm64)
      printf 'arm64'
      ;;
    armv7l)
      printf 'armv7l'
      ;;
    *)
      return 1
      ;;
  esac
}

install_node_via_tarball() {
  local arch tmp_dir shasums_url tarball_name tarball_url tarball_path extract_dir target_dir node_version
  arch="$(get_node_arch)" || {
    error "当前系统架构 $(uname -m) 暂不支持自动安装 Node.js。"
    return 1
  }

  if ! command_exists tar; then
    error "缺少 tar，无法解压 Node.js 安装包。"
    return 1
  fi

  if ! command_exists xz; then
    local pm
    pm="$(detect_package_manager 2>/dev/null || true)"
    if [ -n "$pm" ]; then
      install_prerequisites "$pm" || return 1
    fi
  fi

  tmp_dir="$(mktemp -d)"
  shasums_url="https://nodejs.org/dist/latest-v${NODEJS_MAJOR}.x/SHASUMS256.txt"
  tarball_name="$(download_to_stdout "$shasums_url" | awk "/linux-${arch}\\.tar\\.xz$/ {print \$2; exit}")"
  if [ -z "$tarball_name" ]; then
    rm -rf "$tmp_dir"
    error "无法获取 Node.js ${NODEJS_MAJOR} 的官方下载信息。"
    return 1
  fi

  tarball_url="https://nodejs.org/dist/latest-v${NODEJS_MAJOR}.x/${tarball_name}"
  tarball_path="${tmp_dir}/${tarball_name}"
  log "未发现可用包管理器安装方案，回退到 Node.js 官方二进制包 ..."
  download_to_file "$tarball_url" "$tarball_path" || {
    rm -rf "$tmp_dir"
    error "下载 Node.js 官方安装包失败。"
    return 1
  }

  extract_dir="${tmp_dir}/extract"
  mkdir -p "$extract_dir"
  tar -xJf "$tarball_path" -C "$extract_dir" || {
    rm -rf "$tmp_dir"
    error "解压 Node.js 官方安装包失败。"
    return 1
  }

  node_version="${tarball_name%.tar.xz}"
  target_dir="/usr/local/lib/${node_version}"
  $SUDO mkdir -p /usr/local/lib /usr/local/bin
  $SUDO rm -rf "$target_dir"
  $SUDO cp -R "${extract_dir}/${node_version}" "$target_dir"
  $SUDO ln -sf "${target_dir}/bin/node" /usr/local/bin/node
  $SUDO ln -sf "${target_dir}/bin/npm" /usr/local/bin/npm
  $SUDO ln -sf "${target_dir}/bin/npx" /usr/local/bin/npx
  $SUDO ln -sf "${target_dir}/bin/corepack" /usr/local/bin/corepack
  rm -rf "$tmp_dir"
}

ensure_node() {
  if command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then
    success "Node.js 已可用: $(node -v), npm $(npm -v)"
    return 0
  fi

  local pm=""
  pm="$(detect_package_manager 2>/dev/null || true)"

  log "正在安装 Node.js ${NODEJS_MAJOR} ..."
  if [ -n "$pm" ]; then
    log "检测到包管理器: ${pm}"
    install_node_via_package_manager "$pm" || warn "通过 ${pm} 安装 Node.js 失败，准备尝试官方二进制安装。"
  else
    warn "未检测到受支持的包管理器，准备尝试官方二进制安装。"
  fi

  if ! command_exists node || ! command_exists npm; then
    install_node_via_tarball || return 1
  fi

  hash -r

  if command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then
    success "Node.js 安装完成: $(node -v), npm $(npm -v)"
    return 0
  fi

  error "Node.js 安装未成功完成。"
  return 1
}

install_claude_code() {
  ensure_node || return 1
  log "正在安装 Claude Code ..."
  $SUDO npm install -g @anthropic-ai/claude-code
  if command -v claude >/dev/null 2>&1; then
    success "Claude Code 安装完成: $(claude --version 2>/dev/null || echo "版本检测失败")"
  else
    error "Claude Code 安装完成，但 PATH 中未找到 'claude' 命令。"
    return 1
  fi
}

install_codex() {
  ensure_node || return 1
  log "正在安装 Codex ..."
  $SUDO npm install -g @openai/codex
  if command -v codex >/dev/null 2>&1; then
    success "Codex 安装完成: $(codex --version 2>/dev/null || echo "版本检测失败")"
  else
    error "Codex 安装完成，但 PATH 中未找到 'codex' 命令。"
    return 1
  fi
}

install_all() {
  install_claude_code || return 1
  install_codex || return 1

  log ""
  log "开始配置 Claude Code 提供商"
  configure_claude_provider || return 1

  log ""
  log "开始配置 Codex 提供商"
  configure_codex_provider || return 1

  success "全部组件已安装并完成配置。"
}

configure_claude_provider() {
  local base_url api_key block bashrc_file has_existing_config auth_export_line base_export_line
  bashrc_file="${HOME}/.bashrc"
  has_existing_config=0
  if grep -q "$CLAUDE_BASHRC_START" "$bashrc_file" 2>/dev/null || claude_env_exports_present "$bashrc_file"; then
    has_existing_config=1
  fi

  base_url="$(prompt_required "请输入 Claude 提供商 base URL")"
  api_key="$(prompt_required "请输入 Claude 提供商 API Key")"

  auth_export_line="export ANTHROPIC_AUTH_TOKEN=$(shell_escape "$api_key")"
  base_export_line="export ANTHROPIC_BASE_URL=$(shell_escape "$base_url")"

  backup_file "$bashrc_file"
  strip_block "$bashrc_file" "$CLAUDE_BASHRC_START" "$CLAUDE_BASHRC_END"
  strip_claude_env_exports "$bashrc_file"
  block=$(cat <<EOF
$auth_export_line
$base_export_line
EOF
  )
  append_block "$bashrc_file" "$CLAUDE_BASHRC_START" "$CLAUDE_BASHRC_END" "$block"
  success "Claude 提供商配置已写入 ${HOME}/.bashrc"

  if [ "$has_existing_config" -ne 0 ]; then
    log "检测到旧的 Claude ANTHROPIC_* 配置，已先清除再覆盖。"
  fi
  reload_bashrc
}

configure_codex_provider() {
  local base_url api_key model auth_file config_file block api_key_json
  base_url="$(prompt_required "请输入 Codex 提供商 base URL")"
  api_key="$(prompt_required "请输入 Codex 提供商 API Key")"
  model="$(prompt_default "请输入 Codex 默认模型" "gpt-5.4")"

  auth_file="${HOME}/.codex/auth.json"
  config_file="${HOME}/.codex/config.toml"

  ensure_parent_dir "$auth_file"
  ensure_parent_dir "$config_file"
  backup_file "$auth_file"
  backup_file "$config_file"

  api_key_json="$(json_escape "$api_key")"
  cat > "$auth_file" <<EOF
{
  "OPENAI_API_KEY": "$api_key_json"
}
EOF

  block=$(cat <<EOF
model_provider = "ai_cli_manager_custom"
model = "$(printf '%s' "$model")"
model_reasoning_effort = "medium"

[model_providers.ai_cli_manager_custom]
name = "ai_cli_manager_custom"
wire_api = "responses"
requires_openai_auth = true
base_url = "$(printf '%s' "$base_url")"
EOF
)
  append_block "$config_file" "$CODEX_CONFIG_START" "$CODEX_CONFIG_END" "$block"

  backup_file "${HOME}/.bashrc"
  block=$(cat <<EOF
export OPENAI_BASE_URL="$(printf '%s' "$base_url")"
export OPENAI_API_KEY="$(printf '%s' "$api_key")"
EOF
)
  append_block "${HOME}/.bashrc" "$CODEX_BASHRC_START" "$CODEX_BASHRC_END" "$block"

  success "Codex 提供商配置已写入 ${HOME}/.codex/auth.json 和 ${HOME}/.codex/config.toml"
  log "请执行: source ~/.bashrc"
}

uninstall_claude_code() {
  if command -v npm >/dev/null 2>&1; then
    log "正在卸载 Claude Code ..."
    npm uninstall -g @anthropic-ai/claude-code || warn "npm uninstall 返回了非 0 状态码。"
  else
    warn "未找到 npm，跳过包卸载。"
  fi

  backup_file "${HOME}/.bashrc"
  strip_block "${HOME}/.bashrc" "$CLAUDE_BASHRC_START" "$CLAUDE_BASHRC_END"
  strip_claude_env_exports "${HOME}/.bashrc"
  success "Claude Code 卸载已执行，并从 ${HOME}/.bashrc 中移除了 Claude ANTHROPIC 配置"
}

uninstall_codex() {
  if command -v npm >/dev/null 2>&1; then
    log "正在卸载 Codex ..."
    npm uninstall -g @openai/codex || warn "npm uninstall 返回了非 0 状态码。"
  else
    warn "未找到 npm，跳过包卸载。"
  fi

  backup_file "${HOME}/.bashrc"
  strip_block "${HOME}/.bashrc" "$CODEX_BASHRC_START" "$CODEX_BASHRC_END"

  backup_file "${HOME}/.codex/config.toml"
  strip_block "${HOME}/.codex/config.toml" "$CODEX_CONFIG_START" "$CODEX_CONFIG_END"

  if [ -f "${HOME}/.codex/auth.json" ]; then
    backup_file "${HOME}/.codex/auth.json"
    rm -f "${HOME}/.codex/auth.json"
  fi

  success "Codex 卸载已执行，并移除了受控 Codex 配置"
}

print_status() {
  log ""
  log "当前状态"
  log "--------"

  if command -v claude >/dev/null 2>&1; then
    log "Claude Code: 已安装 ($(claude --version 2>/dev/null || echo "版本未知"))"
  else
    log "Claude Code: 未安装"
  fi

  if command -v codex >/dev/null 2>&1; then
    log "Codex: 已安装 ($(codex --version 2>/dev/null || echo "版本未知"))"
  else
    log "Codex: 未安装"
  fi

  if grep -q "$CLAUDE_BASHRC_START" "${HOME}/.bashrc" 2>/dev/null || claude_env_exports_present "${HOME}/.bashrc"; then
    log "Claude 提供商: 已存在 ~/.bashrc 中的 ANTHROPIC 配置"
  else
    log "Claude 提供商: 未发现 ~/.bashrc 中的 ANTHROPIC 配置"
  fi

  if grep -q "$CODEX_CONFIG_START" "${HOME}/.codex/config.toml" 2>/dev/null; then
    log "Codex 提供商: 已存在受控 ~/.codex/config.toml 配置块"
  else
    log "Codex 提供商: 未发现受控 ~/.codex/config.toml 配置块"
  fi
}

show_menu() {
  if [ -t 1 ] && command -v tput >/dev/null 2>&1 && tput clear >/dev/null 2>&1; then
    tput clear
  fi
  log "AI CLI 管理脚本"
  log "================"
  print_status
  log ""
  log "Claude Code"
  log "------------"
  log "1. 安装 Claude Code"
  log "2. 更换 Claude Code 提供商"
  log "3. 卸载 Claude Code"
  log ""
  log "------------"
  log "Codex"
  log "------------"
  log "4. 安装 Codex"
  log "5. 更换 Codex 提供商"
  log "6. 卸载 Codex"
  log ""
  log "------------"
  log "7. 一键安装全部"
  log "------------"
  log "0. 退出"
  log ""
}

main_loop() {
  local choice=""
  if [ "$HAS_INTERACTIVE_INPUT" -ne 1 ]; then
    error "当前没有可交互终端，菜单模式无法读取输入。"
    error "请改用以下方式执行："
    error "1. 先下载: curl -fsSL -o ai_cli_manager.sh https://raw.githubusercontent.com/bonkcn/CCP-Switcher/main/ai_cli_manager.sh"
    error "2. 再运行: bash ai_cli_manager.sh"
    exit 1
  fi

  while true; do
    show_menu
    printf '请选择功能编号: '
    if ! read -r -u "$INPUT_FD" choice; then
      printf '\n'
      exit 0
    fi
    case "$choice" in
      1)
        install_claude_code
        pause
        ;;
      2)
        configure_claude_provider
        pause
        ;;
      3)
        uninstall_claude_code
        pause
        ;;
      4)
        install_codex
        pause
        ;;
      5)
        configure_codex_provider
        pause
        ;;
      6)
        uninstall_codex
        pause
        ;;
      7)
        install_all
        pause
        ;;
      0)
        exit 0
        ;;
      *)
        warn "无效选项: $choice"
        pause
        ;;
    esac
  done
}

main_loop
