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

配置针对普通 URL/API 密钥提供商的文本模型，覆盖 Chat Completions、Responses 和 Anthropic Messages。订阅账号、专有 API、决策模型和图片生成保持各自原有的检测机制。

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

本次部署采用 Linux 无桌面程序和 `magpie.service`，避免容器自动更新替换定制程序。程序在 `/opt/magpie/current/magpie`，配置和数据在 `/var/lib/magpie`，管理访问密钥在 `/etc/magpie.env`。登录链接和网关调用密钥另存于服务器 `/root/magpie-access.txt`，不要提交这些文件。

```sh
systemctl status magpie
journalctl -u magpie --since '1 hour ago'
sudo -u magpie env HOME=/var/lib/magpie XDG_CONFIG_HOME=/var/lib/magpie/.config \
  XDG_CACHE_HOME=/var/lib/magpie/.cache /opt/magpie/current/magpie provider health status
```

添加中转站后，等下一次自动扫描或通过同样的服务用户环境执行 `provider health scan`。服务开机启动，配置持久保存；重启不会清空检测结果或访问密钥。更新时先构建新的定制版本，备份配置，再切换 `/opt/magpie/current` 到新的发布目录并重启服务；旧目录可以用于回滚。
