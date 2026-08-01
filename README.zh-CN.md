# Codex Provider Switcher

[English](README.md)

Codex Provider Switcher 让 Codex Desktop Remote SSH 中的不同任务使用不同
模型供应商，同时保持同一个 SSH 主机、同一个侧边栏和同一个 Codex
app-server。

这是一个独立的社区项目，不是 OpenAI 官方产品。

> 请使用 v0.2.1 或更高版本。v0.1.0 不兼容 Desktop 的 WebSocket 传输；
> **v0.2.0 同样不兼容原版 Desktop Remote SSH**。

## 功能

本项目会安装一个名为 `codex` 的透明 wrapper。它只接管 Desktop 的
app-server proxy 连接，其他命令仍交给真正的 Codex 可执行文件处理。

```text
Codex Desktop（一个 SSH 别名）
    -> Codex Provider Switcher
    -> 现有 Codex app-server
    -> 现有任务和配置
```

Switcher 不会启动第二个 daemon，也不会修改任务数据库。没有保存供应商选择
的任务仍使用 app-server configuration。Provider 命令在本地处理，不会调用
模型。

## 环境要求

- amd64 或 arm64 架构的 Linux/macOS
- Codex CLI 0.146.0 或更高版本
- 使用 app-server Unix socket 的 Codex Desktop Remote SSH
- 已在 app-server 中配置好需要使用的供应商
- `curl`、`tar`，以及 `sha256sum` 或 `shasum`

不支持原生 Windows。

## 安装

### 1. 下载并校验 Release

将 `VERSION` 改成需要安装的版本。以下命令会自动识别操作系统和 CPU 架构。

```bash
VERSION="0.5.2"
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

PACKAGE="codex-provider-switcher-${VERSION}-${OS}-${ARCH}"
RELEASE_URL="https://github.com/wkj2333666/Codex-Provider-Switcher/releases/download/v${VERSION}"
WORK_DIR="$(mktemp -d)"
cd "$WORK_DIR"

curl -fLO "$RELEASE_URL/$PACKAGE.tar.gz"
curl -fLO "$RELEASE_URL/$PACKAGE.tar.gz.sha256"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum -c "$PACKAGE.tar.gz.sha256"
else
  shasum -a 256 -c "$PACKAGE.tar.gz.sha256"
fi
tar -xzf "$PACKAGE.tar.gz"
cd "$PACKAGE"
```

校验命令必须显示 `OK`，然后才能继续。所有版本可在
[GitHub Releases](https://github.com/wkj2333666/Codex-Provider-Switcher/releases)
下载。

### 2. 安装 wrapper 和 provider skill

在修改 `PATH` 之前，先记录真正的 Codex 路径：

```bash
REAL_CODEX="$(command -v codex)"
case "$REAL_CODEX" in /*) ;; *) echo "Codex path must be absolute" >&2; exit 1 ;; esac
printf 'Real Codex: %s\n' "$REAL_CODEX"
```

将 switcher 安装到独立目录，不覆盖真正的 Codex：

```bash
INSTALL_ROOT="$HOME/.local/lib/codex-provider-switcher"
mkdir -p "$INSTALL_ROOT/bin"
install -m 0755 codex-provider-switcher "$INSTALL_ROOT/codex-provider-switcher"
ln -sfn ../codex-provider-switcher "$INSTALL_ROOT/bin/codex"

mkdir -p "$HOME/.agents/skills"
rm -rf "$HOME/.agents/skills/provider"
cp -R plugins/codex-provider-switcher/skills/provider "$HOME/.agents/skills/provider"
```

### 3. 配置登录 Shell

将以下内容加入 Desktop 使用的远程登录 Shell 配置文件。第一个值必须替换成
上一步输出的绝对路径。

```bash
export CODEX_PROVIDER_SWITCHER_CODEX="/absolute/path/to/real/codex"
export PATH="$HOME/.local/lib/codex-provider-switcher/bin:$PATH"
```

普通 Bash 登录通常使用 `~/.profile`。

### 4. 验证并重新连接 Desktop

启动一个新的登录 Shell，然后运行：

```bash
command -v codex
codex --version
"$HOME/.local/lib/codex-provider-switcher/codex-provider-switcher" --version
```

第一条命令应显示 `$HOME/.local/lib/codex-provider-switcher/bin/codex`；第二条
应显示真正的 Codex 版本；第三条应显示 switcher 版本。

安装完成后，请重新连接 Desktop SSH 主机，让新 proxy 进程加载已安装的版本和
provider skill。

## 只使用一个 SSH 主机

一台机器只保留一个 SSH 别名。即使多个别名连接同一服务器，Desktop 也会为
每个别名创建不同的主机身份和侧边栏。

```sshconfig
Host pi
    HostName server.example.com
    User developer
```

让 Desktop 只连接这个别名。同一主机下的不同任务可以分别保存自己的供应商
选择。

## 使用 Provider 命令

从 Desktop 命令菜单中选择 provider skill，然后使用：

```text
/provider status
/provider switch openai
/provider switch sub2api
```

面向用户的命令只有：

- `/provider status`：显示当前任务已验证的运行时供应商和已保存选择。
- `/provider switch <name>`：切换当前空闲任务并保存选择。

确认消息由 switcher 在本地生成，因此不会调用模型，也不会消耗模型 token。
切换后任务 ID 和历史记录保持不变。运行中的 turn 不会被中断；请等待它结束
后再切换。

## 升级

使用新的 `VERSION` 重复下载和校验步骤，进入解压后的目录，然后替换已安装的
二进制和 skill：

```bash
INSTALL_ROOT="$HOME/.local/lib/codex-provider-switcher"
install -m 0755 codex-provider-switcher "$INSTALL_ROOT/codex-provider-switcher"

rm -rf "$HOME/.agents/skills/provider"
cp -R plugins/codex-provider-switcher/skills/provider "$HOME/.agents/skills/provider"
```

这些命令会直接替换当前安装，不保留旧版本备份。不要修改现有的
`CODEX_PROVIDER_SWITCHER_CODEX`：它必须指向真正的 Codex，不能指向 wrapper。
每次升级后都要重新连接 Desktop SSH 主机。

## 常见问题

**Desktop 中没有 provider 命令：** 确认
`$HOME/.agents/skills/provider/SKILL.md` 存在，然后重新连接 SSH 主机。

**`command -v codex` 没有显示 wrapper：** 检查远程登录 Shell 的 `PATH`，
确保 wrapper 的 `bin` 目录位于真正的 Codex 目录之前。

**Codex 委派出现循环或失败：** 确认 `CODEX_PROVIDER_SWITCHER_CODEX` 是真正
Codex 可执行文件的绝对路径。

**切换被拒绝：** 先等待当前 turn 结束，同时确认 app-server 已配置目标供应商，
并且 Codex CLI 不低于 0.146.0。

**app-server socket 不可用：** 重新连接或重启 Codex Desktop 的远程主机，让
它正常启动 app-server。Switcher 会按设计关闭失败，不会自行启动第二个 daemon。

传输和 handoff 的技术细节见
[docs/architecture.md](docs/architecture.md)。

## 卸载

先从远程登录 Shell 配置文件中删除安装时加入的两个 export，然后删除 wrapper
和 skill：

```bash
rm -rf "$HOME/.local/lib/codex-provider-switcher"
rm -rf "$HOME/.agents/skills/provider"
unset CODEX_PROVIDER_SWITCHER_CODEX
hash -r 2>/dev/null || true
command -v codex
codex --version
```

真正的 Codex 从未被覆盖，此时它应重新成为 `PATH` 中的第一个 `codex`。

## 开发

```bash
go build -trimpath -o codex-provider-switcher ./cmd/codex-provider-switcher
go test ./...
go test -race ./...
go vet ./...
```

高级 proxy 选项、路由规则、WebSocket 限制、provider handoff 和安全边界见
[docs/architecture.md](docs/architecture.md)。

依赖许可证见 [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES)。

## 许可证

MIT，详见 [LICENSE](LICENSE)。
