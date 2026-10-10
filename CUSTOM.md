# Magpie 定制分支

本仓库的 `custom` 分支长期保留模型可用性检测。部署和更新都使用此分支构建的程序。

## 自动检测

```sh
magpie provider health on          # 开启并立即扫描：每 30 分钟，连续失败 2 次屏蔽
magpie provider health on 60 3      # 每 60 分钟，连续失败 3 次屏蔽
magpie provider health scan        # 手动重新扫描
magpie provider health status      # 查看每个中转站、密钥、模型的检测结果
magpie provider health off         # 停用检测，恢复原来的路由行为
```

配置针对普通 URL/API 密钥提供商的文本模型，覆盖 Chat Completions、Responses 和 Anthropic Messages。名称或上游名称包含 embedding/embed、rerank 的检索模型，以及元数据明确仅支持 Gemini 的模型，不使用聊天探测或因此被屏蔽。订阅账号、专有 API、决策模型和图片生成保持各自原有的检测机制。

开启后，新增或修改地址、密钥、认证头、模型映射等配置，需要成功检测才能用于请求。检测分别记录每个中转站、密钥和模型：一个密钥失败不会屏蔽另一个可用密钥。模型列表、路由组、备用模型和直接指定模型均遵守屏蔽结果，只使用检测通过的接口协议。

基础测试发送 **“不要问为什么，只回复ok”**。必须收到实际文本或推理内容才通过；HTTP 200 的空对象、HTML 和无内容的流不算通过。回复不必严格等于 `ok`。一个模型有多个接口时，任一接口通过即可，后续请求转到该接口。

首次检测失败的模型保持隐藏；已通过的模型达到连续失败阈值后隐藏。每次扫描也检测隐藏模型，恢复后自动重新使用。检测取消不会记作失败。扫描刷新中转站模型列表，但保留手动选择的模型与密钥。

网关运行时每分钟检查是否到期。默认扫描间隔 30 分钟，每个模型总超时 20 秒，最多同时检测 4 个；每次扫描每个已启用密钥都会产生一次模型请求，可能消耗额度。新增模型后可执行 `health scan` 提前检测。

状态保存在配置目录的 `model-health.json`，Linux 通常为 `$XDG_CONFIG_HOME/magpie/model-health.json` 或 `~/.config/magpie/model-health.json`。文件损坏会拒绝使用受检测管理的模型并报告错误，不会自动覆盖损坏文件。状态中的标识通过摘要生成，错误中的密钥及认证头会脱敏。

## 合并作者更新

首次克隆：

```sh
git clone -b custom https://github.com/hamajun-tao/magpie.git
cd magpie
git remote add upstream https://github.com/yetone/magpie.git
```

后续更新：

```sh
git switch custom
git fetch upstream
git merge upstream/main
# 有冲突时保留定制检测逻辑，解决冲突后提交；然后重新验证
go test -tags nogui ./...
go vet -tags nogui ./...
go build -tags nogui -ldflags "-X main.version=custom-$(git rev-parse --short HEAD)" -o magpie .
git push origin custom
```

正常合并会保留定制提交；作者修改同一段代码时可能产生冲突，需要解决。不要将 `custom` 强制重置到作者分支。

定制构建的版本使用 `custom-` 前缀；官方内置更新器仅更新正式版本，因此不会用官方发布包替换定制程序。不要手动安装官方二进制或直接部署官方 Docker 镜像，否则运行的程序将没有这项功能。

## 服务器运维

### 在 100.100.1.4 一步更新

先在 GitHub 本仓库的 `main` 点击“同步复刻”，随后执行：

```sh
ssh root@100.100.1.4 magpie-update
```

已登录服务器时直接运行 `magpie-update`。它读取本仓库的 `custom` 和已同步的 `main`，在独立目录合并；检查回滚流程、完整 Go 测试、模型检测并发测试（20 次）、Linux/Windows `go vet`，运行本次变化的网页测试；涉及界面资源时运行全部网页测试和各语言用例（Chromium 和 WebKit），并构建 Linux 服务和 Windows 程序。网页测试使用两个并行文件和独立的 4 小时上限，其他步骤为 30 分钟；超时会停止该步骤及其子进程，包括独立进程组中的浏览器。合并或检查失败时退出，线上服务继续运行。

检查通过后，流程暂停服务，备份完整配置、密钥、健康状态和启动环境，再切换版本。验收检查管理页面和网关认证、模型列表、实际运行版本，以及中转站、调用密钥、设置、模型检测开关和策略是否保留。验收失败会恢复旧程序和完整配置，并重新检查旧服务。停止命令报错时会核实服务已停止，再恢复配置；若新版仍在运行，则保留备份并停止更新。同一份代码重复执行只验收服务，无需重启。并发更新会被拒绝。

```sh
magpie-update --check-only    # 只合并、测试、构建，不切换线上程序
cat /opt/magpie/last-update.json
```

每次执行的详细日志和报告在 `/opt/magpie/maintenance/runs/<执行编号>/`；切换前的备份在 `/opt/magpie/backups/<执行编号>/`。这些目录仅供服务器管理员和构建账号使用，不要公开，其中备份包含密钥。旧发布目录保留在 `/opt/magpie/releases/`。

服务器不保存 GitHub 写入凭据。该命令会把未来的合并部署到服务器；合并提交保留在服务器本地，不会自动推回 GitHub。GitHub 的 `custom` 可通过上一节的合并命令同步保存，不能强制重置。更新程序为 [`scripts/update_server.py`](scripts/update_server.py)，使用服务器已校验安装的 Go 1.26.9 编译器，仅适用于本次部署的目录、服务和工具版本；其他服务器需调整常量并单独安装。

本次部署采用 Linux 无桌面程序和 `magpie.service`，避免容器自动更新替换定制程序。程序在 `/opt/magpie/current/magpie`，配置和数据在 `/var/lib/magpie`，管理访问密钥在 `/etc/magpie.env`。登录链接和网关调用密钥另存于服务器 `/root/magpie-access.txt`，不要提交这些文件。

```sh
systemctl status magpie
journalctl -u magpie --since '1 hour ago'
sudo -u magpie env HOME=/var/lib/magpie XDG_CONFIG_HOME=/var/lib/magpie/.config \
  XDG_CACHE_HOME=/var/lib/magpie/.cache /opt/magpie/current/magpie provider health status
```

添加中转站后，等下一次自动扫描或通过同样的服务用户环境执行 `provider health scan`。服务开机启动，配置持久保存；重启不会清空检测结果或访问密钥。更新时先构建新的定制版本，备份配置，再切换 `/opt/magpie/current` 到新的发布目录并重启服务；旧目录可以用于回滚。
