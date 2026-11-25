# 卸载状态从内核挂载表判断，不用 stat(2)

挂载会话结束时有两种完全相反的原因，需要相反的处置。挂载点仍在但无人服务（FUSE 会话死掉，FUSE-T 日志 `short read on fuse device`）会让每次访问永久卡住，必须让服务管理器重启挂载；挂载点已经不存在则说明用户从 qrypt 外部主动推出了卷（Finder 里推出、`diskutil unmount`、`umount`），再重启就是与用户对抗。

此前两者都落到 `FUSE session ended unexpectedly` 并返回非零退出码，于是 launchd 的 `KeepAlive.SuccessfulExit=false` 把用户刚推出去的卷又挂了回来，表现为"又出现断开的提示"；而在 Resident Core 架构下，若 Core 只把会话从活跃列表删除，附着的 `qrypt mount` 还会误判成正常 `unmount` 而全部以 0 退出。

最初的判定方式是在挂载点上做 stat(2) 探测。这在死挂载上是错的：stat(2) 会一直等到内核放弃死掉的 NFS 对端（实测数秒），而且它回答的是"这个挂载能不能用"，不是"这个挂载是否仍然注册"。两个问题在死挂载上给出的答案恰好相反。

决定：用内核挂载表回答"是否仍然注册"（darwin `getfsstat(2)` 带 `MNT_NOWAIT`，Linux 读 `/proc/self/mounts`），挂载表读的是内核缓存，不触碰被挂载的文件系统，因此在死挂载上也是即时返回；`pkg/mount.IsMounted` 暴露这一判定，`pkg/core` 通过 `MountSessionState` 接口读取该判定与会话错误并在 `MountOutcome` 中记录 `detached` / `external` / `unexpected`，`internal/cli/mount` 据此决定退出码（`detached` 与 `external` 为 0，`unexpected` 为非零）。`forceUnmount` 的"卸载是否已生效"验证同样改用它。

## Considered Options

- **继续用 stat(2) 探测，加超时**：超时只能把"卡住"换成"判定不确定"，而判定不确定时保守地当作"仍注册"就会重新引入重复挂载。拒绝。
- **把 `short read on fuse device` 文本当作外部卸载信号**：该消息在两种结束方式下都会打印，且来自 FUSE-T 内部实现，不是稳定契约。拒绝。
- **外部卸载一律按失败处理并让服务重启**：这正是被修复的行为；用户在 Finder 里推出卷后卷会立刻回来。拒绝。
- **只在启动时记录挂载，之后不做判断**：退出时无法区分，等于没解决。拒绝。

## Consequences

- `Session.Done()` 之后先看挂载表：挂载点仍在才按非零退出（触发重启），否则记录 `Unmounted externally` 并以 0 退出；主动调用 `qrypt unmount` / `DetachMount` 则直接记为 `detached` 并以 0 退出。
- 外部卸载会以 0 退出，因此注册为 LaunchAgent 的挂载不会自动回来；用户需要重新执行 `qrypt mount`（或 `launchctl kickstart -k gui/$(id -u)/com.qrypt.mount`），这是推出卷这一动作的预期后果。
- 挂载表判定与 stat(2) 判定在"挂载点路径是符号链接"等边界上可能有差异；`mountTableHas` 先做绝对路径归一，且永不把 `/` 与家目录当作挂载点，避免误卸载系统卷。
- Linux 的 `/proc/self/mounts` 用八进制转义路径（`\040` 等），读取时必须还原，否则含空格的挂载点永远匹配不上。

## 补充：FUSE-T 的"死挂载"要靠 helper 存活判断

同一个 `mountedAt` 还承担 `ensureMountPoint` 的陈旧挂载检测，这里对 FUSE-T 有两处特殊事实：

- **FUSE-T 的 statfs 类型是 `nfs`，不是 `fuse*`**。它把文件系统以本地 NFS 导出给内核，`fstypename` 因此是 `nfs`，只有挂载源保留了身份（`fuse-t:/Qrypt`）。如果只按类型名匹配，FUSE-T 挂载永远不会被识别为 FUSE 挂载，`ensureMountPoint` 的陈旧挂载清理在 macOS 上对 FUSE-T 完全失效。因此改为类型与来源同时匹配。
- **helper 进程消失才是可靠判据**。FUSE-T 每个挂载 fork 一个 `go-nfsv4` helper（命令行末位是挂载点），挂载随之生死。helper 已死时，探测 readdir 返回 `EIO`——与"live 挂载后端失败"的 EIO 无法区分，或者干脆阻塞到内核放弃。因此 FUSE-T 分支改用 helper 存活判断，不再依赖 errno。

另有一个实测结论影响了测试设计：FUSE-T 会在挂载进程被杀后**主动卸载**（实测 1 秒内挂载表条目消失），所以在 FUSE-T 环境下无法制造"留在挂载表里的死挂载"。`ensureMountPoint` 在该路径上要做的只是确认目录可重建；macFUSE 才会留下死挂载条目（issue #10）。

FSKit 后端同样保留每个挂载的 `go-nfsv4` helper，但 `statfs` 类型为 `fuse-t`，来源为 `file:///private/tmp/fuset-session-…/session.json`。识别时同时接受这个类型；检查子目录时以挂载根匹配 helper，保留路径中的空格。FSKit 卷从挂载表消失后，helper 仍可能花几秒退出；正常卸载需要等待它结束，避免立刻重挂时撞上旧会话锁。
