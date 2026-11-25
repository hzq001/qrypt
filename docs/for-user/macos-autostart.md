# macOS 开机自动挂载（launchd）

在 macOS 上让 `qrypt mount` 登录后自动拉起，用系统自带的 launchd 即可——通过一个 LaunchAgent 连接到常驻 Core（`qrypt core serve`）托管本地挂载。

## 一键安装

```sh
scripts/install-launchagent-macos.sh install
```

脚本只做三件事：

1. 在 `~/Library/LaunchAgents/com.qrypt.mount.plist` 写入 job 定义，`ProgramArguments` 为 `<qrypt> mount --config <配置文件> --stop-core`；
2. 若同名 job 已在运行，先 `launchctl bootout` 让旧进程干净卸载挂载点并等待退出，随后执行 `<qrypt> core stop --config <配置文件>`，确保使用旧二进制或旧配置的常驻 Core 完全释放锁和挂载点；
3. `launchctl bootstrap gui/$UID` 装载并立即启动。

校验：

```sh
mount | grep Qrypt
launchctl print gui/$(id -u)/com.qrypt.mount | grep -E "state|pid|runs"
ls ~/Qrypt/local
```

## job 行为

| 键 | 值 | 含义 |
| --- | --- | --- |
| `RunAtLoad` | `true` | 登录后立刻启动 |
| `KeepAlive` | `SuccessfulExit=false` | 只有非零退出才重启；`Ctrl+C`、服务停止或用户主动推出卷（退出码 0）不会被拉起 |
| `ThrottleInterval` | `30` | 反复失败时最小重启间隔 30 秒 |
| `ProcessType` | `Background` | 不与应用争抢前台调度 |
| `StandardOutPath` / `StandardErrorPath` | `~/.qrypt/logs/launchd.{out,err}.log` | launchd 捕获的标准输出与错误 |

LaunchAgent 属于用户登录会话，因此是**登录后**自动挂载。plist 放在 `~/Library/LaunchAgents` 即生效，不需要 `sudo`。

由于 `ProgramArguments` 携带了 `--stop-core`，停止 LaunchAgent 或执行升级重载时，会连同该配置对应的常驻 Core 一并平稳停止，避免新版本与旧版 Core 版本或配置指纹不匹配。

## 查看状态与卸载

```sh
scripts/install-launchagent-macos.sh status
scripts/install-launchagent-macos.sh uninstall
```

`uninstall` 会先停止 job（挂载点随之卸载）再删除 plist。

## 自定义路径

| 变量 | 默认值 |
| --- | --- |
| `QRYPT_BIN` | `~/bin/qrypt`，不存在时取 `PATH` 中的 `qrypt` |
| `QRYPT_CONFIG` | `~/.qrypt/qrypt.toml` |
| `QRYPT_LOG_DIR` | `~/.qrypt/logs` |
| `QRYPT_LABEL` | `com.qrypt.mount` |

例：`QRYPT_BIN=/opt/homebrew/bin/qrypt scripts/install-launchagent-macos.sh install`

改了配置文件或升级了 qrypt 二进制后重跑一次 `install` 即可（脚本会停止旧 job 与旧 Core 并重新挂载）。

## 排错

- 没挂上：`tail -50 ~/.qrypt/logs/launchd.err.log`，再看 `~/.qrypt/logs/qrypt-error.log`。后者出现 `WARN [FUSE] … (slow)` 属正常，是网络后端较慢回调的日志。
- 若遇到 Core 版本或配置不一致报错：先运行 `qrypt core stop --config <配置文件>`，再重新启动 LaunchAgent。
- 强制重挂：`launchctl kickstart -k gui/$(id -u)/com.qrypt.mount`。
- 想临时前台观察日志：`scripts/install-launchagent-macos.sh uninstall` 后直接跑 `~/bin/qrypt mount`。

## macOS FUSE 后端注意

macOS 26 及以上可以[配置 FUSE-T FSKit 后端](macos-fskit.md)。修改配置后重新运行安装脚本，让现有 NFS 挂载和常驻 Core 正常退出，再启动 FSKit 挂载。通过 `mount` 输出确认挂载类型为 `fuse-t`，并包含 `fskit` 标志。

FSKit 挂载不保证在每次关闭文件时触发 FUSE `flush`/`release`，所以需要在 qrypt 配置里显式指定 `backend=fskit`，启用对应的写入提交适配。若使用未启用内核扩展的 macFUSE 5.4+，qrypt 的 cgofuse 默认优先加载 FUSE-T（`/usr/local/lib/libfuse-t.dylib`），原因见 [cgofuse 本地说明](../../third_party/cgofuse/README.qrypt.md)。自动挂载遇到访问授权错误时，先前台运行同一条命令完成系统提示的授权，再启动 LaunchAgent。
