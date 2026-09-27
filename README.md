# rules-mcp

部署在 Debian 的 Go 单二进制 MCP 服务：编辑 Clash YAML，生成 sing-box JSON，自动提交并推送 Git。

HTTP 地址默认为 `http://127.0.0.1:8787/mcp`，不鉴权，仅允许绑定回环 IP。适合 MCP 客户端与服务在同一台 Debian 上运行。没有 Python、Node、Docker 或 Go 模块依赖；服务器需要已有的 `git`；使用 SSH Git remote 时还需要 OpenSSH `ssh`。

## 行为约定

- YAML 是规则源，位置为仓库根目录的 `<name>.yaml`，对应 `json/<name>.json`。
- 始终在配置的开发分支编辑和 commit；拒绝在 `master`、`main` 或 detached HEAD 修改。
- `publish_branch: "master"` 开启自动快进发布到 master；设为 `""` 只推送开发分支。整个过程中不 checkout master。
- 自动发布用 `git push --atomic` 同时推送开发分支和发布分支，服务端必须支持 atomic push。不强推、不 rebase、不自动解决冲突。
- 不生成 `.bak` 或旧文件副本。每个文件通过临时文件、flush 和 rename 替换；两个文件替换和 Git 发布不是一个原子事务。
- 操作日志位于 `.git/rules-mcp-journal.json`，只保存目标新内容、旧内容哈希、commit 和阶段，用于失败续传；不是旧内容备份。日志权限为 `0600`。
- 一次 `rules_apply` 操作一个规则组，新增、删除均可批量处理；同时传入 remove 和 add 可替换规则。支持创建规则组，但不删除规则文件。
- 需要专用、无其他编辑者的 checkout。已有未提交/未跟踪文件、非本服务的本地未推送提交、异常暂存内容都会阻止发布。

## 规则格式

支持现有仓库使用的受限 YAML 格式，不是通用 YAML 解析器：一个 `payload:` 顶层键，每行以两个空格和 `- ` 开头，后面是一个规则字符串，可有普通注释、行尾注释和不含转义的单/双引号。保留原注释和 LF/CRLF 风格，新增条目放在文件末尾。拒绝锚点、复杂嵌套、附加顶层字段和未知规则类型。

| Clash 类型 | sing-box 字段 |
| --- | --- |
| `DOMAIN` | `domain` |
| `DOMAIN-SUFFIX` | `domain_suffix` |
| `DOMAIN-KEYWORD` | `domain_keyword` |
| `IP-CIDR` / `IP-CIDR6` | `ip_cidr` |

JSON 保持现有 `version: 1` 源规则格式。同字段重复值去重；IP 类型可带 `no-resolve`，该 Clash 选项没有写入 sing-box 源规则集的对应字段，JSON 中只保留 CIDR 匹配值。CIDR 保留原文本；含主机位的前缀不会擅自重写。空规则组生成 `rules: []`，避免生成匹配全部流量的空对象。

仓库已有的 `- # 注释` 空条目会保留在 YAML 并报告告警，转换时跳过。已有带路径的域名规则允许读取和精确移除，但不允许生成 JSON 或作为新规则添加。

只读检查当前 `C:\Dev\rules` 时发现：17 个 YAML、11,382 条规则、85 个空注释条目；`reject.yaml` 第 123、124 行的两个域名后缀含 `/v2`、`/v1`。这些实际文件没有修改。该组必须先经用户决定删除或替换异常条目，才能发布。

## 从 v0.1.0 升级

v0.2.0 移除了内置 OpenWrt 同步，应用职责止于 YAML/JSON 更新及 Git 提交、推送。需要在其他地方实现分发时，由外部程序处理。

- 从已有配置中移除整个 `openwrt` 对象；新配置只有下方示例中的六个字段。严格配置校验会拒绝旧的 `openwrt` 字段，不会静默忽略它。
- MCP 不再提供 `rules_sync`；重连客户端以刷新工具列表。`rules_apply` 和 `rules_resume` 在 Git 推送成功后结束，返回值不再包含 `openwrt_synced`、`openwrt_enabled`。
- 旧操作日志中的 `synced` 字段仅用于兼容读取，后续保存时不再写出。旧日志处于 `pushed` 阶段时，`rules_resume` 检查本地提交后直接完成；不会重复推送，也不会连接路由器。
- Git 使用 SSH remote 的认证方式保持不变；删除路由器同步配置不影响 Git 的 SSH 密钥和 `known_hosts`。
- 升级前核对新配置并运行 `-check`，再由管理员安排该 MCP 服务重启。如果回退到 v0.1.0，必须提供其所需的旧版配置；新旧二进制和配置应配套，不覆盖已有 Release。

## 构建

开发机使用已有 Go 1.23 或更新版本。没有第三方模块，不需要执行依赖安装。

Linux 构建机，在项目根目录：

```bash
go version
GOTOOLCHAIN=local GOPROXY=off go test ./...
GOTOOLCHAIN=local GOPROXY=off go vet ./...
make build
```

Windows PowerShell，在 `C:\Dev\rules-mcp`：

```powershell
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
go test ./...
go vet ./...
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
go build -trimpath -ldflags='-s -w' -o dist/rules-mcp-linux-amd64 ./cmd/rules-mcp
$env:GOARCH = 'arm64'
go build -trimpath -ldflags='-s -w' -o dist/rules-mcp-linux-arm64 ./cmd/rules-mcp
# 构建后请重新打开终端运行本机测试，避免继续使用上述交叉编译环境变量。
```

产物：`dist/rules-mcp-linux-amd64`、`dist/rules-mcp-linux-arm64`。`CGO_ENABLED=0` 生成不依赖动态 C 运行库的可执行文件。Debian 上无需安装 Go；按 `uname -m` 选择 `x86_64` 对应 amd64、`aarch64` 对应 arm64。

可选的本机仓库兼容性检查（只读取文件）：

```powershell
$env:RULES_MCP_TEST_CORPUS = 'C:\Dev\rules'
go test ./internal/rules -run TestExistingCorpusReadOnly -v
```

此检查同时报告校验阻断项；测试 PASS 不代表所有原始数据都能发布。

## Debian 首次部署

以下操作由管理员在 Debian 执行，未由开发过程自动执行。示例使用服务用户 `rules-mcp`，程序目录 `/opt/rules-mcp`，配置目录 `/etc/rules-mcp`，专用 Git checkout `/srv/rules`，SSH 资料目录 `/var/lib/rules-mcp/.ssh`。请替换占位符和部署所用的实际 SSH 端口。

先检查已有工具、用户、目录和端口：

```bash
uname -m
command -v git
command -v ssh
id rules-mcp
ss -ltn '( sport = :8787 )'
```

缺少工具时先确认依赖安装方案，不自动安装。服务用户及上述目录应由管理员按机器现有规范创建：程序和配置由管理员维护；`/srv/rules` 由服务用户拥有并可写；SSH 私钥只有服务用户可读，SSH 目录权限为 `0700`、私钥为 `0600`。不使用递归放宽权限。

将选定架构二进制和示例配置上传到 Debian 的暂存目录，例如 `/var/tmp/rules-mcp-upload`。上传可使用工作站已有 `scp`，必须明确实际 SSH 端口：

```bash
scp -P <DEBIAN_SSH_PORT> dist/rules-mcp-linux-amd64 <DEPLOY_USER>@<DEBIAN_HOST>:/var/tmp/rules-mcp-upload/rules-mcp
```

在 Debian 暂存目录核对校验和后，首次安装（这两条命令拒绝覆盖已有文件）：

```bash
cd /var/tmp/rules-mcp-upload
sha256sum rules-mcp
test ! -e /opt/rules-mcp/rules-mcp && sudo install -m 0755 rules-mcp /opt/rules-mcp/rules-mcp
test ! -e /etc/rules-mcp/config.json && sudo install -m 0644 config.example.json /etc/rules-mcp/config.json
```

配置文件不要放入 rules checkout，避免作为未跟踪文件阻止发布。按实际环境编辑配置，所有示例内容都是占位符：

```json
{
  "listen": "127.0.0.1:8787",
  "repository": "/srv/rules",
  "branch": "dev",
  "remote": "origin",
  "publish_branch": "master",
  "timeout_seconds": 120
}
```

Git 的 SSH 身份使用服务用户的 SSH 配置/代理。不要在配置、Git remote URL、README 或命令参数中写入密码和 Token。提前通过可信渠道核对 Git 服务器的主机公钥指纹，并写入服务用户的 `known_hosts`。服务强制 `BatchMode=yes` 和 `StrictHostKeyChecking=yes`，不绕过主机校验。

准备一个专用的 rules checkout，服务用户需要对开发分支和发布分支都有推送权限。若远端已有 dev，在 Debian `/srv` 下执行：

```bash
sudo -u rules-mcp git clone --branch dev <RULES_GIT_SSH_URL> /srv/rules
sudo -u rules-mcp git -C /srv/rules branch --show-current
sudo -u rules-mcp git -C /srv/rules status --short
sudo -u rules-mcp git -C /srv/rules config user.name 'Rules Publisher'
sudo -u rules-mcp git -C /srv/rules config user.email 'rules-publisher@example.invalid'
```

如果远端还没有 dev，需要先从 master 新建开发分支。下面创建分支，不编辑规则文件；最后一步会在远端新增 dev 分支：

```bash
sudo -u rules-mcp git clone --branch master <RULES_GIT_SSH_URL> /srv/rules
sudo -u rules-mcp git -C /srv/rules switch -c dev
sudo -u rules-mcp git -C /srv/rules push -u origin dev
```

两种 clone 方案二选一，只用于尚未存在的目标目录。Git 作者配置同样需要完成。仓库必须已有至少一个提交和 `json` 目录。保持 LF/CRLF 原内容，避免在这个专用 checkout 设置改变内容的 Git clean filters 或 `core.autocrlf=true`；提交前会比对暂存内容与已校验文件，发生自动转换会拒绝提交。服务禁用仓库 Git hooks 和提交签名。

## 启动和验证

先以服务用户运行只读配置/本地仓库检查；它不连接 Git remote，但会创建内部锁文件：

```bash
sudo -u rules-mcp /opt/rules-mcp/rules-mcp -config /etc/rules-mcp/config.json -check
```

前台启动：

```bash
sudo -u rules-mcp /opt/rules-mcp/rules-mcp -config /etc/rules-mcp/config.json
```

或者使用 `deploy/rules-mcp.service`。先核对 unit 中的用户、路径及可写目录；安装和启用只影响这个新 HTTP 服务。管理员确认后在 Debian 执行：

```bash
test ! -e /etc/systemd/system/rules-mcp.service && sudo install -m 0644 /var/tmp/rules-mcp-upload/rules-mcp.service /etc/systemd/system/rules-mcp.service
sudo systemctl daemon-reload
sudo systemctl enable --now rules-mcp.service
systemctl status rules-mcp.service --no-pager
journalctl -u rules-mcp.service -n 50 --no-pager
```

同一台 Debian 上的 MCP 客户端填写 Streamable HTTP URL：`http://127.0.0.1:8787/mcp`，不设置鉴权。其他机器或独立网络命名空间中的容器不能直接使用这个回环地址。不要用反向代理把无鉴权端点公开；同机进程能调用这些写入工具。

协议使用 JSON 响应模式，支持版本 `2025-03-26`、`2025-06-18`、`2025-11-25`，无 session、无 SSE 长连接，不支持旧 `/sse` 传输和 OAuth。`GET /mcp` 返回 405 是预期行为。客户端若只能使用更新协议且不接受协商回退，需要先验证兼容性。

连通性与工具发现：

```bash
curl --fail-with-body http://127.0.0.1:8787/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"manual-check","version":"1"}}}'

curl --fail-with-body http://127.0.0.1:8787/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

## MCP 工具

| 工具 | 用途 |
| --- | --- |
| `rules_list` | 列出规则组 |
| `rules_read` | 读取规则、revision、校验告警和 JSON 一致性；支持 offset、limit |
| `rules_preview` | 预览 add/remove；返回用于应用的 revision，不写规则 |
| `rules_apply` | 拉取、校验 revision、修改、生成 JSON、commit、atomic push |
| `rules_status` | 最近操作阶段和 commit；不查询远端实时状态 |
| `rules_resume` | 继续最近一次失败的操作 |

预览工具参数示例：

```json
{"name":"chatgpt","add":["DOMAIN-SUFFIX,example.com"],"remove":[]}
```

将返回的 `revision` 原样填入应用参数：

```json
{"name":"chatgpt","add":["DOMAIN-SUFFIX,example.com"],"remove":[],"expected_revision":"<REVISION_FROM_PREVIEW>"}
```

`add: []`、`remove: []` 可重新生成已有 YAML 对应的 JSON。文件名参数不带扩展名，禁止路径穿越。一次最多 2,000 个增删操作，请求体最大 1 MiB，源文件最大 8 MiB。

自动发布顺序：

1. 检查专用 checkout、开发分支、干净状态和未完成操作。
2. fetch 两条远端分支，并 `pull --ff-only` 开发分支。拒绝非服务产生的本地未推送提交。
3. 确认发布分支是开发分支的祖先；master 领先或分叉时要求先在 dev 人工整合并推送。
4. 核对预览 revision，校验规则，记录目标内容，然后逐文件原子替换 YAML、JSON。
5. 只暂存两个目标文件，检查暂存路径、内容及敏感信息特征后提交。
6. 普通快进 `push --atomic` 更新远端 dev 和 master。master 的保护规则若禁止直接 push，会明确失败，不绕过仓库策略。
7. 保存已推送状态并返回 `complete`，本次操作结束。

原有规则中疑似凭据的注释也会阻止预览/提交；扫描是额外防护，不是通用密钥检测器。子进程错误不回传可能含凭据的原始 stderr。

## 失败恢复与回滚

`rules_status` 的阶段可为 `prepared`、`written`、`committed`、`pushed`、`complete`。工具失败返回 `isError: true`，发布失败还返回已到达阶段和 commit。不要仅凭 HTTP 200 判断写入成功。

- commit 失败：检查服务用户的 Git 作者设置、权限及暂存内容，修复后 `rules_resume`。
- push 失败：提交保留在本地。凭据或网络恢复后 `rules_resume`，不重新调用 apply。
- 进程在两个文件替换之间中断：日志允许补齐剩余内容；若发现外部编辑，则拒绝覆盖并要求人工检查。
- Git 冲突、分叉、发布期间远端前进：服务不会猜测合并结果。暂停调用写工具，人工审查并在 dev 整合；涉及替换旧的待续传操作时，先保留 `.git/rules-mcp-journal.json` 供检查，再由管理员决定恢复方案。没有自动丢弃操作的工具。

需要撤销已发布的规则时，优先用新的 preview/apply 做反向增删；这样产生新的审计提交并走相同发布流程。复杂回滚先查看 `git log` 和具体提交，在 dev 准备恢复修改并审查，再正常提交、推送。没有旧文件备份，不使用 reset --hard、force push 或删除目录恢复。

自动化测试使用临时本地 Git 仓库与 bare remote，验证编辑、双分支原子推送、冲突、失败续传与协议。上线验收还应使用服务用户验证 Git 远端认证、主机公钥校验和分支推送权限。

## GitHub 构建与分发

`.github/workflows/ci.yml` 在分支 push 和 PR 上执行 Linux 测试（含 race 检查）、vet、amd64/arm64 交叉编译与打包。Actions 使用固定提交版本。Go 工具链由 GitHub runner 的 setup-go 准备，不安装到 Debian；Go 项目仍无第三方模块。

`.github/workflows/release.yml` 在推送 `v0.2.0` 这类标签后复用同一 CI，成功后自动发布 GitHub Release。预发布标签如 `v0.2.0-rc.1` 会标为 prerelease。Release 附件包括：

- `rules-mcp-linux-amd64`、`rules-mcp-linux-arm64`，可直接部署的 ELF 文件。
- `rules-mcp_<版本>_linux_amd64.tar.gz`、`rules-mcp_<版本>_linux_arm64.tar.gz`，包含二进制、README、MIT LICENSE、配置示例、systemd unit。
- `LICENSE` 与 `SHA256SUMS`，校验文件覆盖两个二进制、两个压缩包及许可证。

工作流使用 GitHub 自动提供的短期 `GITHUB_TOKEN`，无需配置个人 Token，不保存服务器 SSH 密钥。CI 只有读权限，发布 job 才有 `contents: write`。GitHub 负责构建分发，下载后仍部署到 Debian，不会在 GitHub 上托管常驻 MCP 服务。

项目仓库：[rshun/rules-mcp](https://github.com/rshun/rules-mcp)，[版本下载](https://github.com/rshun/rules-mcp/releases)。发布前必须检查分支、工作树与暂存内容中的敏感信息。不要把真实 `config.json` 或 SSH 资料加入仓库。

已有已审核、已提交并推送的开发分支后，在工作站项目目录检查：

```bash
git branch --show-current
git status --short
git diff --cached
git log -1 --oneline
```

确认当前提交就是需要公开分发的版本后，下面两条命令创建并推送版本标签；推送会触发公开或私有 GitHub Release（随仓库可见性）：

```bash
git tag -a v0.2.0 -m 'Release v0.2.0'
git push origin v0.2.0
```

不要重复或移动已发布标签。上传失败时可能留下 draft release，应先在 GitHub 检查附件再决定补传/发布；工作流不覆盖已有 Release 附件。首个远端 Actions 运行结果才是 Linux CI 的实际证据，本地交叉编译不等于 GitHub 发布成功。

在 Debian 下载某个明确版本（按需要替换版本和架构）：

```bash
curl --fail --location --output rules-mcp_v0.2.0_linux_amd64.tar.gz \
  https://github.com/rshun/rules-mcp/releases/download/v0.2.0/rules-mcp_v0.2.0_linux_amd64.tar.gz
curl --fail --location --output SHA256SUMS \
  https://github.com/rshun/rules-mcp/releases/download/v0.2.0/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
```

应确认输出中目标压缩包为 `OK`，再解压到新的暂存目录，按前面的首次部署步骤核对和安装。私有仓库需要现有已授权的 GitHub CLI 等下载方式，不要把访问 Token 写进 URL。更新生产服务时需要单独安排该 MCP 服务的重启；分发流程不会替你停止任何服务。

## 协议与格式参考

- [MCP Streamable HTTP 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
- [MCP tools](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)
- [sing-box source rule-set format](https://sing-box.sagernet.org/configuration/rule-set/source-format/)
