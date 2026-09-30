#!/usr/bin/env bash
# First installation only. Run from an extracted, verified Linux release bundle.
set -euo pipefail

fail() { printf '错误：%s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "缺少已有工具 $1；请管理员先处理，不会自动安装依赖。"; }

usage() {
  cat <<'HELP'
rules-mcp Debian 首次部署（已有用户、已有 rules 仓库）

  sudo bash install.sh                        # 交互填写，确认后安装
  bash install.sh --plan --user USER --repo PATH --binary PATH
  sudo bash install.sh --yes --user USER --repo PATH --binary PATH

参数：
  --user NAME               已有的非 root 运行用户
  --repo PATH               已有规则仓库，默认 /srv/rules
  --binary PATH             本地 Linux 二进制，默认脚本同目录的 rules-mcp
  --install-dir PATH        程序目录，默认 /opt/rules-mcp
  --config-dir PATH         配置目录，默认 /etc/rules-mcp
  --service-name NAME       systemd 服务名（不带 .service），默认 rules-mcp
  --listen ADDRESS          127.0.0.1:PORT 或 [::1]:PORT，默认 127.0.0.1:8787
  --branch NAME             当前工作分支，默认 dev；允许 master/main
  --remote NAME             已有 Git remote 名，默认 origin
  --publish-branch NAME     自动发布目标，默认 master；传 - 表示关闭
  --timeout SECONDS         操作超时 1–600，默认 120
  --start yes|no            安装后启用并启动服务，默认 yes
  --plan                    只读检查并预览；不创建目录、不写文件、不调用 systemd
  --yes                     批准摘要中的首次安装操作，跳过交互确认
  --help                    显示帮助

不创建用户、不 clone/pull/push 仓库、不配置 Git 作者或密钥、不覆盖已有文件。
安装失败时保留已写入文件供检查，不自动删除、停止服务或放宽权限。
HELP
}

defaults() {
  BINARY="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/rules-mcp"
  RUN_USER="${SUDO_USER:-$(id -un)}"
  [[ "$RUN_USER" != root ]] || RUN_USER=''
  REPO=/srv/rules
  INSTALL_DIR=/opt/rules-mcp
  CONFIG_DIR=/etc/rules-mcp
  SERVICE_NAME=rules-mcp
  LISTEN=127.0.0.1:8787
  BRANCH=dev
  REMOTE=origin
  PUBLISH_BRANCH=master
  TIMEOUT=120
  START=yes
  PLAN=no
  YES=no
  declare -gA GIVEN=()
}

parse_args() {
  local key
  while (($#)); do
    case "$1" in
      --help|-h) usage; exit 0 ;;
      --plan) PLAN=yes; shift; continue ;;
      --yes) YES=yes; shift; continue ;;
      --user) key=RUN_USER ;;
      --repo) key=REPO ;;
      --binary) key=BINARY ;;
      --install-dir) key=INSTALL_DIR ;;
      --config-dir) key=CONFIG_DIR ;;
      --service-name) key=SERVICE_NAME ;;
      --listen) key=LISTEN ;;
      --branch) key=BRANCH ;;
      --remote) key=REMOTE ;;
      --publish-branch) key=PUBLISH_BRANCH ;;
      --timeout) key=TIMEOUT ;;
      --start) key=START ;;
      *) fail '未知参数；使用 --help 查看支持项。' ;;
    esac
    (($# >= 2)) || fail '参数缺少值。'
    printf -v "$key" '%s' "$2"
    GIVEN["$key"]=yes
    shift 2
  done
}

ask() {
  local key="$1" label="$2" value
  [[ ${GIVEN[$key]:-} != yes ]] || return 0
  printf '%s [%s]: ' "$label" "${!key}"
  read -r value || fail '输入中断，未安装。'
  [[ -z "$value" ]] || printf -v "$key" '%s' "$value"
}

collect() {
  if [[ -t 0 && "$YES" == no ]]; then
    ask RUN_USER '运行用户（必须已存在，不能为 root）'
    ask REPO '已有 rules 仓库绝对路径'
    ask BINARY '已校验的本地 Linux 二进制绝对路径'
    ask INSTALL_DIR '程序安装目录'
    ask CONFIG_DIR '配置文件目录'
    ask SERVICE_NAME 'systemd 服务名'
    ask LISTEN '本机监听地址'
    ask BRANCH '当前工作分支'
    ask REMOTE '已有 Git remote 名称'
    ask PUBLISH_BRANCH '自动发布分支（输入 - 关闭）'
    ask TIMEOUT '操作超时秒数'
    ask START '安装后启用并启动服务（yes/no）'
  elif [[ "$PLAN" != yes && "$YES" != yes ]]; then
    fail '非交互运行请明确选择 --plan 或 --yes。'
  fi
  [[ "$PUBLISH_BRANCH" != - ]] || PUBLISH_BRANCH=''
}

safe_path() {
  local value="$1"
  [[ "$value" =~ ^/[A-Za-z0-9_./-]+$ && "$value" != / ]] || fail '路径必须为绝对路径，仅允许字母、数字、下划线、点、横线和斜杠。'
  [[ "$(realpath -m -- "$value")" == "$value" ]] || fail '路径必须规范化，不能含符号链接、重复斜杠、末尾斜杠或 ..。'
}

overlap() { [[ "$1" == "$2" || "$1" == "$2/"* || "$2" == "$1/"* ]]; }

validate_parameters() {
  local value
  [[ "$RUN_USER" =~ ^[a-z_][a-z0-9_-]*$ ]] || fail '请指定有效的已有运行用户名。'
  RUN_UID=$(id -u "$RUN_USER" 2>/dev/null) || fail '运行用户不存在。'
  [[ "$RUN_UID" != 0 ]] || fail '服务必须使用已有的非 root 用户。'
  RUN_GROUP=$(id -g "$RUN_USER")
  RUN_HOME=$(getent passwd "$RUN_USER" | cut -d: -f6)
  [[ -d "$RUN_HOME" ]] || fail '运行用户的主目录不存在。'
  for value in "$REPO" "$BINARY" "$INSTALL_DIR" "$CONFIG_DIR" "$RUN_HOME"; do safe_path "$value"; done
  for value in "$REPO" "$RUN_HOME"; do
    case "$value" in /tmp|/tmp/*|/var/tmp|/var/tmp/*) fail '规则仓库和用户主目录不能放在 /tmp 或 /var/tmp；systemd PrivateTmp 会隔离这些目录。' ;; esac
  done
  for value in "$INSTALL_DIR" "$CONFIG_DIR"; do
    case "$value" in /etc|/usr|/bin|/sbin|/lib|/lib64|/var|/var/lib|/srv|/opt|/home|/root|/run|/tmp) fail '安装及配置目录必须是独立的子目录。' ;; esac
    overlap "$value" "$REPO" && fail '安装目录和配置目录不能与规则仓库相互包含。'
  done
  [[ "$SERVICE_NAME" =~ ^[a-zA-Z][a-zA-Z0-9_-]{0,63}$ ]] || fail '服务名只能包含字母、数字、下划线和横线，且不带 .service。'
  for value in "$BRANCH" "$REMOTE" "$PUBLISH_BRANCH"; do
    [[ -z "$value" && "$value" == "$PUBLISH_BRANCH" ]] && continue
    [[ "$value" =~ ^[A-Za-z0-9][A-Za-z0-9_/-]*$ && "$value" != *//* && "$value" != */ ]] || fail 'Git 分支或 remote 名称格式不受支持。'
  done
  [[ -n "$BRANCH" && -n "$REMOTE" && "$BRANCH" != "$PUBLISH_BRANCH" ]] || fail '工作分支和 remote 不能为空，发布分支必须与工作分支不同；仅使用一个分支时，请将自动发布分支设为 -。'
  [[ "$LISTEN" =~ ^(127\.0\.0\.1|\[::1\]):([0-9]{1,5})$ ]] || fail '监听地址必须是 127.0.0.1:PORT 或 [::1]:PORT。'
  local port=$((10#${BASH_REMATCH[2]}))
  ((port >= 1024 && port <= 65535)) || fail '普通用户监听端口必须在 1024–65535 范围。'
  [[ "$TIMEOUT" =~ ^[0-9]{1,3}$ ]] || fail '超时必须为 1–600 秒。'
  TIMEOUT=$((10#$TIMEOUT))
  ((TIMEOUT >= 1 && TIMEOUT <= 600)) || fail '超时必须为 1–600 秒。'
  [[ "$START" == yes || "$START" == no ]] || fail '--start 只能是 yes 或 no。'
  CONFIG_FILE="$CONFIG_DIR/config.json"
  INSTALLED_BINARY="$INSTALL_DIR/rules-mcp"
  UNIT_FILE="/etc/systemd/system/$SERVICE_NAME.service"
}

as_user() {
  # Match a system service: do not inherit caller Git overrides or SSH agent.
  local -a clean_env=(env -i "HOME=$RUN_HOME" "USER=$RUN_USER" "LOGNAME=$RUN_USER"
    'PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
    'LANG=C.UTF-8' 'GIT_OPTIONAL_LOCKS=0' 'GIT_TERMINAL_PROMPT=0')
  if [[ $(id -u) == "$RUN_UID" ]]; then
    "${clean_env[@]}" "$@"
  else
    [[ $(id -u) == 0 ]] || fail '检查其他用户的权限需要通过 sudo 运行 --plan。'
    runuser -u "$RUN_USER" -- "${clean_env[@]}" "$@"
  fi
}

check_destination() {
  local path="$1"
  [[ ! -e "$path" && ! -L "$path" ]] || fail '目标文件已存在；首次安装脚本拒绝覆盖，请按升级流程处理。'
}

check_managed_ancestors() {
  local dir="$1" permissions
  while [[ "$dir" != / ]]; do
    if [[ -e "$dir" ]]; then
      [[ -d "$dir" ]] || fail '安装或配置路径的父级不是目录。'
      permissions=$(stat -c %a -- "$dir")
      [[ "$(stat -c %u -- "$dir")" == 0 ]] && (( (8#$permissions & 022) == 0 )) || fail "安装和配置目录及其已有父目录必须由 root 持有，且组/其他用户不可写：$dir"
      as_user test -x "$dir" || fail '运行用户不能访问安装或配置目录的父级。'
    fi
    dir=$(dirname -- "$dir")
  done
}

preflight() {
  local root branch dirty author machine architecture
  [[ -f "$BINARY" && -x "$BINARY" ]] || fail '二进制必须是存在且可执行的普通文件。'
  [[ "$(od -An -tx1 -N4 -- "$BINARY" | tr -d ' \n')" == 7f454c46 ]] || fail '必须提供 Linux ELF 二进制。'
  machine=$(od -An -tu2 -j18 -N2 -- "$BINARY" | tr -d ' \n')
  architecture=$(uname -m)
  case "$architecture:$machine" in x86_64:62|aarch64:183) ;; *) fail '二进制架构与当前机器不匹配（仅支持 amd64/arm64）。' ;; esac
  VERSION=$(as_user "$BINARY" -version 2>/dev/null) || fail '运行用户无法执行源二进制，请放到其可访问的暂存目录。'
  [[ "$VERSION" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,80}$ ]] || fail '二进制版本输出不符合预期。'
  [[ -d "$REPO/.git" && ! -L "$REPO/.git" && -d "$REPO/json" && ! -L "$REPO/json" ]] || fail '需要带 .git 和 json 目录的独立 rules checkout。'
  [[ "$(stat -c %u -- "$REPO")" == "$RUN_UID" ]] || fail '规则仓库必须由所选运行用户持有，脚本不会更改所有者或绕过 Git 校验。'
  for root in "$REPO" "$REPO/.git" "$REPO/json"; do
    as_user test -r "$root" && as_user test -w "$root" && as_user test -x "$root" || fail '运行用户没有规则仓库及其 .git/json 目录的读写和访问权限。'
  done
  root=$(as_user git -C "$REPO" rev-parse --show-toplevel 2>/dev/null) || fail '运行用户不能访问该 Git 仓库。'
  [[ "$root" == "$REPO" ]] || fail 'repository 必须指向 Git 仓库根目录。'
  branch=$(as_user git -C "$REPO" branch --show-current 2>/dev/null) || fail '无法读取仓库分支。'
  [[ "$branch" == "$BRANCH" ]] || fail '仓库当前分支与配置的工作分支不同；请先自行切换，脚本不会切换分支。'
  as_user git -C "$REPO" rev-parse --verify HEAD >/dev/null 2>&1 || fail '规则仓库必须已有提交。'
  dirty=$(as_user git -C "$REPO" status --porcelain --untracked-files=all 2>/dev/null) || fail '无法检查仓库状态。'
  [[ -z "$dirty" ]] || fail '规则仓库存在未提交或未跟踪文件；请先处理。'
  as_user git -C "$REPO" remote get-url "$REMOTE" >/dev/null 2>&1 || fail '配置的 Git remote 不存在。'
  # Explicit values are required; Git must not guess an author from the hostname.
  for author in user.name user.email; do
    [[ -n "$(as_user git -C "$REPO" config --get "$author" 2>/dev/null)" ]] || fail '请先为运行用户或规则仓库配置 Git user.name 和 user.email。'
  done
  check_destination "$INSTALLED_BINARY"
  check_destination "$CONFIG_FILE"
  check_managed_ancestors "$INSTALL_DIR"
  check_managed_ancestors "$CONFIG_DIR"
  for root in /etc/systemd/system /run/systemd/system /usr/lib/systemd/system /lib/systemd/system; do
    check_destination "$root/$SERVICE_NAME.service"
    [[ ! -e "$root/$SERVICE_NAME.service.d" && ! -L "$root/$SERVICE_NAME.service.d" ]] || fail '发现同名服务的已有 drop-in 配置，拒绝接管。'
  done
}

render_config() {
  # All string values are constrained above: JSON metacharacters are rejected.
  cat <<CONFIG
{
  "listen": "$LISTEN",
  "repository": "$REPO",
  "branch": "$BRANCH",
  "remote": "$REMOTE",
  "publish_branch": "$PUBLISH_BRANCH",
  "timeout_seconds": $TIMEOUT
}
CONFIG
}

render_unit() {
  cat <<UNIT
[Unit]
Description=Local HTTP MCP for Clash and sing-box rule publishing
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=$RUN_USER
Group=$RUN_GROUP
Environment=HOME=$RUN_HOME
WorkingDirectory=$REPO
ExecStart=$INSTALLED_BINARY -config $CONFIG_FILE
Restart=on-failure
RestartSec=5
TimeoutStopSec=$((TIMEOUT + 30))
UMask=0027
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=$REPO
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6

[Install]
WantedBy=multi-user.target
UNIT
}

summary() {
  printf '\n部署摘要（版本 %s）\n运行用户：%s，主目录：%s\n规则仓库：%s\n程序：%s\n配置：%s\n服务：%s\n启用并启动：%s\n' \
    "$VERSION" "$RUN_USER" "$RUN_HOME" "$REPO" "$INSTALLED_BINARY" "$CONFIG_FILE" "$UNIT_FILE" "$START"
  printf '\n将生成的 config.json：\n'
  render_config
  printf '\n将生成的 systemd unit：\n'
  render_unit
  printf '\n检查仅覆盖本机环境；未连接 Git remote，未验证远端推送权限。\n'
}

managed_directory() {
  local dir="$1" permissions
  if [[ -d "$dir" ]]; then
    permissions=$(stat -c %a -- "$dir")
    [[ "$(stat -c %u -- "$dir")" == 0 ]] && (( (8#$permissions & 022) == 0 )) || fail '已有安装/配置目录必须由 root 持有，且组/其他用户不可写。'
  else
    install -d -m 0755 -o root -g root -- "$dir"
  fi
}

require_install_host() {
  [[ $(id -u) == 0 ]] || fail '执行安装需要 root；请使用 sudo，或使用 --plan 预览。'
  [[ -f /etc/debian_version && -d /run/systemd/system ]] || fail '安装需要运行 systemd 的 Debian 系统。'
}

write_new_file() {
  local source="$1" target="$2" mode="$3"
  # Exclusive creation closes the gap between the existence check and copying.
  (set -C; umask 077; cat -- "$source" > "$target") || fail '创建目标文件失败，可能已存在；未覆盖该文件。'
  chown 0:0 -- "$target"
  chmod "$mode" -- "$target"
}

install_service() {
  require_install_host
  need systemctl; need systemd-analyze; need mktemp; need install
  [[ "$(systemctl show --property=LoadState --value "$SERVICE_NAME.service")" == not-found ]] || fail '同名服务已经存在或无法查询 systemd；拒绝接管。'
  local confirmation
  if [[ "$YES" != yes ]]; then
    printf '\n将创建上述程序、配置和服务文件，执行 daemon-reload；start=yes 时启用并启动该新服务。输入 INSTALL 确认：'
    read -r confirmation || fail '未确认，未安装。'
    [[ "$confirmation" == INSTALL ]] || fail '未确认，未安装。'
  fi
  # A failure never triggers file deletion, account changes or service shutdown.
  local staging
  staging=$(mktemp -d /var/tmp/rules-mcp-install.XXXXXXXX)
  printf '本次暂存目录：%s（完成或失败后保留供检查）\n' "$staging"
  chmod 0755 "$staging"
  render_config > "$staging/config.json"
  render_unit > "$staging/$SERVICE_NAME.service"
  chmod 0644 "$staging/config.json" "$staging/$SERVICE_NAME.service"
  install -m 0755 -- "$BINARY" "$staging/rules-mcp"
  printf '阶段 1/4：以运行用户验证配置和规则仓库。\n'
  as_user "$staging/rules-mcp" -config "$staging/config.json" -check || fail '应用配置/仓库检查失败；尚未写入最终安装文件。'
  printf '阶段 2/4：安装程序和配置。\n'
  managed_directory "$INSTALL_DIR"
  managed_directory "$CONFIG_DIR"
  check_destination "$INSTALLED_BINARY"; check_destination "$CONFIG_FILE"; check_destination "$UNIT_FILE"
  write_new_file "$staging/rules-mcp" "$INSTALLED_BINARY" 0755
  write_new_file "$staging/config.json" "$CONFIG_FILE" 0644
  printf '阶段 3/4：校验并安装 systemd 服务文件。\n'
  systemd-analyze verify "$staging/$SERVICE_NAME.service" || fail 'systemd 校验失败；程序和配置已写入，服务尚未安装，请检查暂存目录。'
  write_new_file "$staging/$SERVICE_NAME.service" "$UNIT_FILE" 0644
  printf '阶段 4/4：加载服务配置，并按所选参数处理启动。\n'
  systemctl daemon-reload
  if [[ "$START" == yes ]]; then
    systemctl enable --now "$SERVICE_NAME.service"
    systemctl is-active --quiet "$SERVICE_NAME.service" || fail '服务未处于 active 状态，请查看 journalctl。'
  fi
  printf '\n安装完成。查看服务：systemctl status %s.service --no-pager\n查看日志：journalctl -u %s.service -n 50 --no-pager\nMCP 地址：http://%s/mcp\n' "$SERVICE_NAME" "$SERVICE_NAME" "$LISTEN"
  if [[ "$START" == no ]]; then
    printf '服务尚未启用/启动。准备好后由管理员执行：systemctl enable --now %s.service\n' "$SERVICE_NAME"
  fi
}

main() {
  defaults
  parse_args "$@"
  [[ "$(uname -s)" == Linux ]] || fail '此脚本用于 Debian/Linux；--help 可在其他系统查看。'
  for tool in id getent cut realpath stat git od tr env; do need "$tool"; done
  [[ $(id -u) != 0 ]] || need runuser
  collect
  validate_parameters
  preflight
  summary
  [[ "$PLAN" != yes ]] || { printf '\n预览完成，未执行安装或修改仓库。\n'; return; }
  install_service
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
