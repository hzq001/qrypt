# 挂载对外提供的属性必须稳定

macOS 的 NFS 客户端把"属性变了"当作"内容可能变了"的证据，每次判定变化都会对 vnode 跑一遍页缓存失效（`nfs_vinvalbuf2`）。当属性持续抖动、而同一个 vnode 上又有活跃的读写者时，失效与 vnode 回收会撞在一起；`com.apple.filesystems.nfs` 在这条竞态上会内核 panic（`Kernel data abort`，栈里只有 `com.apple.filesystems.nfs`，见 FUSE-T issue #109 与 Apple FB23527406）。这条竞态在 Apple 的一侧，用户态关不掉；我们能决定的是**多久掷一次骰子**——即让客户端少看见几次"属性变了"。

FUSE-T 的 NFS 后端用本地 NFSv4 导出文件系统，这类 panic 会在这条挂载路径出现。实测：2026-10-22 16:01 的一次 `Kernel data abort` 发生在 `com.apple.filesystems.nfs` 内，panic 时被调度的任务是正在读挂载内文件的普通进程（`OmniReader`），qrypt 与 FUSE-T helper 都还活着——这不是"死挂载"，也不是 qrypt 崩溃，是内核在失效路径上自己踩空。macOS 26 及以上可以[显式使用 FUSE-T FSKit 后端](../for-user/macos-fskit.md)避开这条 NFS 路径；属性稳定性的约束仍然保留。

qrypt 侧曾有两处制造抖动的地方：

- **无 mtime 的合成条目每次 GETATTR 都取 `time.Now()`**。命名空间根、根下的 `/.DS_Store`、`/.fseventsd` 等 Finder 元数据路径，其 `drive.Entry` 没有后端 mtime，`fillStatWithCreds` 于是回落到当前时间。Finder 会持续探测这些路径，等于给内核提供一个"周期性变化"的 vnode。
- **inode 取自 `drive.Entry.ID`**。待传记录的 ID 是暂存代（`name-<纳秒基36>`），每写一代换一次；上传完成后条目又切换到远端对象 ID。同一个路径在一次编辑过程中会换两三次 inode，而写入者此时正持有该路径的句柄。这条只在客户端要求保留文件系统给的 inode 时才致命：默认的 FUSE-T NFS 导出自己编号（内核侧 ino 是 1、2、3…），而 `-o use_ino` 会把 `st_ino` 原样交给内核（实测：旧版本每代换一次，修复后恒定）。

决定：挂载层对外提供的属性只由路径决定，不由"此刻"或"当前上传代"决定。`stableInode` 以虚拟路径为身份（路径为空时才退回 entry ID），`fillStat` 在 mtime 缺失时用 inode 派生一个固定的合成时间戳（`syntheticModTime`，2000-01-01 起一年内），不再调用 `time.Now()`。

## Considered Options

- **给 NFS 客户端加 `noattrcache`，让属性永不缓存**：方向相反。缓存越少，客户端越频繁回源比较属性，抖动被放大的次数更多。拒绝。
- **在 qrypt 里给 FUSE 回调加超时或熔断，避开长回调**：panic 发生在内核失效路径，与回调耗时无关；[ADR 0004](0004-fuse-session-death-ends-the-process.md) 已拒绝回调级超时。拒绝。
- **把 FUSE-T 换成 macFUSE**：macFUSE 5.4 的 FSKit 后端不投递 `flush`/`release`，暂存写入永远不会提交（见 [cgofuse 本地说明](../../third_party/cgofuse/README.qrypt.md)）。用不可写换 panic，不是修复。拒绝。
- **只修 inode 或只修 mtime**：两者是同一类抖动，都能独立触发失效。只修一半等于留着同一个陷阱。拒绝。

## Consequences

- `stableInode` 的语义从"远端对象身份"变为"路径身份"。重命名后 inode 会变（路径变了），这是可接受的：改名本来就伴随一次失效，且旧路径已消失。
- 合成时间戳落在 2000-01-01 起一年内，读起来偏旧，但稳定且可复现；不要在展示层"修正"它，否则又引入抖动。
- 后端提供的真实 mtime 仍原样透出；本决定只接管"没有 mtime"的回退路径。`O_TRUNC` 重写导致的 size/mtime 变化属于用户可见的真实变更，不在收敛范围内。
- 抖动只影响触发概率，不关闭内核竞态：仍需跟进 Apple 的 kext 修复。降频手段是保证每个路径的属性在两次 GETATTR 之间不变。
