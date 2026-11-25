# 在 macOS 上使用 FSKit 挂载

macOS 26 及以上可以通过 FUSE-T 的 FSKit 后端挂载 qrypt，让这个挂载直接使用本地文件系统接口，避开 macOS 的 NFS 客户端。读取 qrypt 文件时遇到 `com.apple.filesystems.nfs` 内核崩溃，可以使用这条路线。

## 安装并启用 FUSE-T

安装当前版本；已通过 Homebrew 安装的用户使用升级命令：

```sh
brew install --cask fuse-t
# 已安装时：brew upgrade --cask fuse-t
```

打开「应用程序」中的 `fuse-t.app`，按提示在「系统设置 → 通用 → 登录项与扩展 → 按类别 → 文件系统扩展」启用 FUSE-T。版本要求和已知问题以 [FUSE-T releases](https://github.com/macos-fuse-t/fuse-t/releases) 为准。

## 切换已有挂载

先结束现有挂载与常驻 Core：前台运行时按 `Ctrl+C`（若仅需更新挂载选项，需运行 `qrypt core stop` 让常驻 Core 重新读取配置）；使用登录自动挂载时，按[自动挂载说明](macos-autostart.md)重新安装 LaunchAgent，让旧挂载和旧 Core 正常退出后再启动。

在 `qrypt.toml` 的 macOS 挂载选项数组中添加 `backend=fskit`，保留已有选项。例如：

```toml
[mount.options]
darwin = ["defer_permissions", "auto_xattr", "backend=fskit"]
```

显式配置这个选项也会启用 qrypt 对 FSKit 写入提交的适配。仅升级 FUSE-T，或者只修改 FUSE-T 的全局默认后端，都不能替代这项配置。

重新启动挂载：

```sh
qrypt mount
```

如果启动失败，先完成系统扩展授权；使用 LaunchAgent 时，还要检查系统是否允许对应命令访问所需目录。qrypt 不会自动改用 NFS。

## 确认切换成功

运行 `mount`，找到配置中的挂载目录。FUSE-T FSKit 挂载应包含文件系统类型 `fuse-t` 和标志 `fskit`，来源通常是 `file:///private/tmp/fuset-session-…/session.json`。

如果该目录仍显示 `nfs`，当前挂载或常驻 Core 还在使用旧配置，需要停止旧挂载及 Core（`qrypt core stop`）并重新挂载。

确认后可正常读取、保存文件。写入仍按 `upload.upload_delay` 的静默窗口上传；写入停顿超过该窗口时，当前内容可能先上传，后续写入再更新。FSKit 适配会增加本地磁盘同步开销。
