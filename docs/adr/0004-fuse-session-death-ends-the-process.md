# FUSE 会话结束时进程必须退出，而不是继续等待

fuse-t 把 FUSE 文件系统以本地 NFS 导出给内核。当导出端会话死掉（libfuse 通道关闭、`host.Mount` 返回）时，内核 NFS 客户端仍在按 `hard` 语义重试，挂载点会永久卡住并显示 `not responding`。此前挂载调用方只阻塞在 `<-ctx.Done()` 上：进程不退出，launchd 不会重启，也没有任何一侧再应答导出请求，于是挂载只能靠人工清理。

决定：挂载生命周期以"会话"为单位显式建模（`mount.Session`）。`host.Mount` 返回即视为会话结束，`Session.Done()` 关闭并带上原因（`Session.Err()`）。在 Resident Core 模式下，`mountManager` 在会话意外退出后记录终止原因（`MountOutcome`），附着的 CLI 客户端（`qrypt mount`）以及 `--isolated` 直挂分支收到非外部卸载的会话结束时立即以非零码退出，把恢复交给进程管理器（launchd `KeepAlive.SuccessfulExit=false`）与下一次挂载的陈旧挂载清理（`ensureMountPoint`）。信号触发的中断走正常路径：先卸载、再以 0 退出，避免被反复重启。

## Considered Options

- **给每个 FUSE 回调加超时，超时后用 `-ETIMEDOUT` 回复**：cgofuse 不支持取消，回调返回后底层可能仍在执行；超时回复会造成重复回复与缓存不一致，且慢调用本身并不代表会话已死。拒绝。
- **保持进程存活，由内部定时器重连 fuse-t**：libfuse 会话无法在进程内重建，重连等价于重启挂载会话，收益为零。拒绝。
- **只调大 NFS `timeo`/`retrans` 或改成 `soft` 挂载**：把"卡死"换成"静默 I/O 错误"，用户看到的是数据不一致而不是挂载失效，且 fuse-t 的导出参数不由 qrypt 完全控制。拒绝。

## Consequences

- `mount.Mounter.Mount` 返回的 `*Session` 是唯一生命周期句柄；调用方必须监听 `Session.Done()`，不能只等自己的 ctx。
- 会话意外结束（仍注册于挂载表且非主动 `unmount`）的退出码为非零，因此 launchd 会自动重启；重启后 `ensureMountPoint` 负责清掉上一次遗留的挂载。`umount -f` 在 fuse-t NFS 挂载上会失败，darwin 需要 `diskutil unmount force` 兜底。区分外部卸载与意外死亡的挂载表判定见 [ADR 0005](0005-mount-state-from-mount-table.md)。
- 会话死亡前仍在执行的慢回调不会被取消，可能拖住进程退出；这是 cgofuse 的固有限制，接受为已知代价，而不是引入有风险的回调级超时。
- 卸载失败不再被吞掉：`Unmount` 返回系统卸载错误，CLI 至少把它写进日志与 stderr。

只读元数据查询的协作式 context 预算见 [ADR 0007](0007-bound-read-only-mount-metadata.md)。它仍同步等待底层返回，不采用上文拒绝的“底层继续执行、提前回复 FUSE”方案，也不改变会话死亡后的退出策略。
