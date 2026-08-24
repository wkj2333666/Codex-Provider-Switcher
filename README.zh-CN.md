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
VERSION="0.5.7"
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

### 3. 配置 provider 到 model 的路由

在 switcher 状态目录创建严格校验的 `models.json`，为每个 provider 配置默认
model。要选择同一 provider 的多个模型，使用 `default` + `models` allowlist；
字符串写法仍表示“只允许这一个模型”：

```bash
STATE_ROOT="${CODEX_HOME:-$HOME/.codex}/codex-provider-switcher"
install -d -m 0700 "$STATE_ROOT"
install -m 0600 /dev/null "$STATE_ROOT/models.json"
printf '%s\n' '{"openai":{"default":"gpt-5.6-sol","models":["gpt-5.6-sol","gpt-5.6-terra","gpt-5.6-luna","gpt-5.5","gpt-5.4","gpt-5.4-mini","gpt-5.3-codex-spark"]},"sub2api":"gpt-5.6-sol","glm":{"default":"glm-5.3","models":["glm-5.3","glm-5.2"]},"kimi":"k3","deepseek":{"default":"deepseek-v4-flash","models":["deepseek-v4-flash","deepseek-v4-pro"]},"openrouter":{"default":"stealth/ox-alpha","models":["stealth/ox-alpha"]}}' \
  > "$STATE_ROOT/models.json"
```

切换 provider 时使用默认 model。Desktop 随后选择 allowlist 内的模型时，switcher
会保存该任务的精确 model；同一 provider 内换模型直接在下一轮生效，不执行 provider
handoff。跨 provider 或未列出的值不会改变路由。JSON 格式错误、
无效值、符号链接和非常规文件都会使 switcher 在代理 Desktop 前关闭请求。

要让第三方模型出现在 Desktop picker 中，还需在 `~/.codex/config.toml` 顶层加载
Codex 模型目录，然后重新连接 Desktop SSH：

```toml
model_catalog_json = "/home/your-user/.codex/models-override.json"

[model_providers.openrouter]
name = "openrouter"
base_url = "https://openrouter.ai/api/v1"
wire_api = "responses"
env_key = "OPENROUTER_API_KEY"
```

OpenRouter 使用 `OPENROUTER_API_KEY` 环境变量。当前配置的模型是
`stealth/ox-alpha`，同时作为任务主模型和自动 reviewer；Desktop 侧启用图像输入，
上下文上限设为 10000 token。

### 4. 配置登录 Shell

将以下内容加入 Desktop 使用的远程登录 Shell 配置文件。第一个值必须替换成
上一步输出的绝对路径。

```bash
export CODEX_PROVIDER_SWITCHER_CODEX="/absolute/path/to/real/codex"
export PATH="$HOME/.local/lib/codex-provider-switcher/bin:$PATH"
```

普通 Bash 登录通常使用 `~/.profile`。

精确的空闲 provider 不一致和 `systemError` 终止状态都会自动恢复。Switcher
会短暂归档并还原受影响的任务，以卸载陈旧运行时；任务 ID、历史和子任务保持
不变，也不会重新发送用户消息。订阅同一 app-server 任务的客户端都必须使用
switcher wrapper；不支持绕过 wrapper 直接订阅 app-server。

Desktop 或 Android SSH 的短连接结束后，旧 proxy 有时仍会在本地误记任务处于
`active`，但 app-server 中的任务其实已经空闲。Switcher 在返回 handoff 错误
`-32090` 前，现在会跨所有 provider 查询 app-server 的权威任务状态；只有状态
精确为 `idle` 或 `systemError` 时才清除 peer 的陈旧本地状态。本连接确有活动
turn、服务端状态仍为 active、响应异常或 peer 协调失败时，仍会在发送用户消息前
关闭失败。新 turn 转发后，每任务 fence 会一直保留到 app-server 返回确认，因此
确认到达前的真实在途请求不会被误判成陈旧状态。

重连和显式取消任务订阅也可能清空 proxy 的内存路由，但 app-server 仍然加载着
该任务。Switcher 在决定下一条 turn 是否需要 provider handoff 前，会从
`thread/list` 恢复权威运行时 provider 和 model。provider 相同时走轻量恢复；
运行时状态缺失、异常或确实不同时，仍按情况关闭失败或执行完整 handoff。

任务转交给其他 provider 时，Switcher 还会检查 rollout 中遗留的、与旧 provider
绑定的 reasoning ID。它会在 app-server 加载历史前删除非法 reasoning 记录，并从
普通消息和工具调用中移除可选的过期 item ID；同一 provider 内恢复任务会直接透传，
不会重写仍然有效的 provider 绑定历史。跨 provider 重写采用原子替换，并保持用户
可见消息和工具调用配对不变。JSONL 损坏，或确需重写时 Codex 仍持有 writer lock，
都会直接拒绝操作；已经干净的 rollout 只做无副作用检查，也绝不会伪造 `rs_` ID。

### 5. 验证并重新连接 Desktop

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
/provider switch glm
/provider switch openrouter
```

面向用户的命令只有：

- `/provider status`：显示当前任务已验证的运行时供应商和已保存选择。
- `/provider switch <name>`：切换当前空闲任务并保存选择。

确认消息由 switcher 在本地生成，因此不会调用模型，也不会消耗模型 token。
成功切换到 sub2api 时，确认消息为
`Provider switched to sub2api using model gpt-5.6-sol.`。切换后任务 ID 和历史
记录保持不变。运行中的 turn 不会被中断；请等待它结束后再切换。

`models.json` 控制当前任务允许的 provider/model 组合；`model_catalog_json` 控制
Desktop picker 显示哪些模型。任务保存 provider 后，同 provider allowlist 内的 picker
选择也会保存；其他选择会被改回该任务已保存的安全路由。

## 从当前 main 部署

本机部署请使用仓库提供的受保护命令，不要从当前打开的任意 worktree 手动复制
二进制。该命令只接受干净的 `main`，并要求 `HEAD` 与 `origin/main` 完全一致；
随后自动运行测试、构建 Linux arm64、校验内置来源信息，并原子替换已安装文件，
不保留旧版本备份：

```bash
cd /home/wkj/projects/codex-provider-switcher
git switch main
git pull --ff-only origin main
scripts/deploy-local.sh
"$HOME/.local/lib/codex-provider-switcher/codex-provider-switcher" --build-info
```

输出中的 `source` 必须是 `main`，commit 必须是你刚推送的版本。脚本不会停止已有
proxy；部署后请重新连接 Desktop Remote SSH 主机，让新的 proxy 进程加载替换后的
二进制。

## 从 Release 压缩包升级

使用新的 `VERSION` 重复下载和校验步骤，进入解压后的目录，然后原子替换已安装的
二进制和 model catalog；先暂存两者，再依次启用 catalog 和二进制，最后替换
provider skill：

```bash
INSTALL_ROOT="$HOME/.local/lib/codex-provider-switcher"
BINARY_STAGE="$(mktemp "$INSTALL_ROOT/.codex-provider-switcher.XXXXXX")"
install -m 0755 codex-provider-switcher "$BINARY_STAGE"

STATE_ROOT="${CODEX_HOME:-$HOME/.codex}/codex-provider-switcher"
install -d -m 0700 "$STATE_ROOT"
MODELS_STAGE="$(mktemp "$STATE_ROOT/.models.json.XXXXXX")"
install -m 0600 /dev/null "$MODELS_STAGE"
printf '%s\n' '{"openai":{"default":"gpt-5.6-sol","models":["gpt-5.6-sol","gpt-5.6-terra","gpt-5.6-luna","gpt-5.5","gpt-5.4","gpt-5.4-mini","gpt-5.3-codex-spark"]},"sub2api":"gpt-5.6-sol","glm":{"default":"glm-5.3","models":["glm-5.3","glm-5.2"]},"kimi":"k3","deepseek":{"default":"deepseek-v4-flash","models":["deepseek-v4-flash","deepseek-v4-pro"]}}' \
  > "$MODELS_STAGE"
mv -f "$MODELS_STAGE" "$STATE_ROOT/models.json"
mv -f "$BINARY_STAGE" "$INSTALL_ROOT/codex-provider-switcher"

rm -rf "$HOME/.agents/skills/provider"
cp -R plugins/codex-provider-switcher/skills/provider "$HOME/.agents/skills/provider"
```

这些命令会直接替换当前安装，不保留旧版本备份。不要修改现有的
`CODEX_PROVIDER_SWITCHER_CODEX`：它必须指向真正的 Codex，不能指向 wrapper。
每次升级后都要重新连接 Desktop SSH 主机。重新连接前，暂存 catalog 的替换会以
0600 写入精确的受支持路由，并在新二进制启用前生效。

## 常见问题

**Desktop 中没有 provider 命令：** 确认
`$HOME/.agents/skills/provider/SKILL.md` 存在，然后重新连接 SSH 主机。

**`command -v codex` 没有显示 wrapper：** 检查远程登录 Shell 的 `PATH`，
确保 wrapper 的 `bin` 目录位于真正的 Codex 目录之前。

**Codex 委派出现循环或失败：** 确认 `CODEX_PROVIDER_SWITCHER_CODEX` 是真正
Codex 可执行文件的绝对路径。

**切换被拒绝：** 先等待当前 turn 结束，同时确认 app-server 已配置目标供应商，
并且 Codex CLI 不低于 0.146.0。升级 switcher 后请重新连接 Desktop SSH 主机，
确保所有仍在运行的 proxy 使用同一版本。

**后续消息返回 JSON-RPC `-32090`：** 升级到 v0.5.7 或更高版本，并将所有
Desktop/Android SSH 客户端重新连接一次。该版本既会在 app-server 确认任务空闲后
消除 peer 的陈旧 active 状态，也不会把同一 provider 内的 model 变化误当成需要
rollout 写锁的完整 handoff；真正运行中的任务和跨 provider 历史仍会受到保护。

**app-server socket 不可用：** 重新连接或重启 Codex Desktop 的远程主机，让
它正常启动 app-server。Switcher 会按设计关闭失败，不会自行启动第二个 daemon。

传输和 handoff 的技术细节见
[docs/architecture.md](docs/architecture.md)。

## 卸载

先从远程登录 Shell 配置文件中删除安装时加入的 switcher export，然后删除 wrapper
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
