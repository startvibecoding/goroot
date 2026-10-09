# goroot

[English](README.md) | **中文**

`goroot` 是一个用 Go 编写的**无 root、轻量、单进程**容器运行时，可以把一个
rootfs（比如 Alpine minirootfs）直接启动起来。目标是做 proot 的现代替代：
不依赖 ptrace 拦截系统调用，而是使用 Linux **用户命名空间（user namespace）**
在普通用户权限下获得一个「假 root」，比 proot 更快、更稳。

```
$ go build -o goroot .
$ ./goroot run -r rootfs -- /bin/sh
/ # id
uid=0(root) gid=0(root) groups=65534(nobody),0(root)
```

## 特性

- **无需 root**：靠 unprivileged user namespace 把当前用户映射成容器内的 root。
- **真正的隔离**：user / mount / pid / uts / ipc / net 六个命名空间（可单独关闭）。
- **单进程**：默认直接把目标命令 exec 成容器内的 PID 1，无守护进程。
- **完整挂载视图**：`/proc`、只读 `/sys`、`/dev`（含设备节点、`/dev/pts`、
  `/dev/shm`）、`/tmp`。
- **pivot_root** 切换根文件系统（失败时回退 chroot）。
- **bind 挂载**（可只读）、额外 tmpfs、环境变量、工作目录。
- **可选微型 init**（`--init`）：PID 1 回收僵尸进程并转发信号，适合跑服务。
- 内置 **rootfs 解包**（本地文件或 `http(s)://` URL，支持 .tar/.tar.gz/.tar.bz2）。
- 交互式终端：自动接管前台进程组，Ctrl-C / 作业控制可用。
- **内核能力自检**（`goroot doctor`）与**优雅降级**：低内核/受限环境下缺哪个
  命名空间就关哪个，并给出警告，而不是直接崩溃。

## 环境要求

- Linux 内核 ≥ 3.8（用户命名空间），`/proc/sys/kernel/unprivileged_userns_clone = 1`；
- 若要挂载 `sysfs`，需能创建自己的 network namespace（默认会创建）；
- Go ≥ 1.21。

缺失的能力会被自动探测并降级（见下方「兼容性与优雅降级」）。先跑一次自检：

```sh
goroot doctor
```

## 构建

```sh
make build          # 产出 ./goroot
make install        # 安装到 /usr/local/bin
```

## 快速开始

```sh
# 1. 下载并解包一个 Alpine rootfs（不需要 root，文件归当前用户所有）
./goroot extract \
  https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/x86_64/alpine-minirootfs-3.21.3-x86_64.tar.gz \
  rootfs

# 2. 启动交互式 shell
./goroot run -r rootfs -- /bin/sh

# 3. 一次性命令
./goroot run -r rootfs -- /bin/sh -c 'cat /etc/os-release; id'
```

### 在容器里联网装包

默认是**独立的 network namespace**（只有 lo）。要与主机共享网络：

```sh
./goroot run -r rootfs --share-net \
  -b /etc/resolv.conf:/etc/resolv.conf:ro \
  -- /bin/sh -c 'apk add --no-cache curl && curl -s https://example.com | head -1'
```

## 用法

```
goroot run [options] [--] <command> [args...]
goroot extract [-c N] <tarball|url> <dest>
goroot doctor
goroot version
```

`run` 选项：

| 选项 | 说明 |
| --- | --- |
| `-r, --root DIR` | rootfs 目录（默认 `./rootfs`） |
| `--hostname NAME` | 容器主机名（默认 `goroot`） |
| `-b, --bind SRC[:DST][,ro]` | 绑定挂载主机路径（可重复） |
| `--ro-bind SRC[:DST]` | 只读绑定挂载（可重复） |
| `--tmpfs DST` | 在 DST 挂载 tmpfs（可重复） |
| `-e, --env KEY=VAL` | 设置环境变量（可重复） |
| `-w, --cwd DIR` | 容器内工作目录（默认 `/`） |
| `--share-net` | 与主机共享网络命名空间 |
| `--no-pid` / `--no-ipc` / `--no-uts` | 不创建对应命名空间 |
| `-u, --uid N` / `-g, --gid N` | 容器内运行身份（见下方限制） |
| `--init` | 以微型 init 作 PID 1（回收僵尸、转发信号） |
| `--keep-env` | 保留主机环境变量 |

`extract` 选项：`-c N` / `--strip N` 去掉 N 层路径前缀。

## 工作原理

父进程通过 `clone(2)`（Go 的 `SysProcAttr`）一次性创建命名空间，并把当前
用户/组映射为容器内的 uid/gid 0：

```
uid_map: 0 -> <你的 host uid>     (size 1)
gid_map: 0 -> <你的 host gid>     (size 1)
```

随后 `fork/exec` 自身，进入隐藏的 `__init` 阶段，在命名空间内完成：

1. `mount --make-rprivate /`，防止挂载事件泄漏回主机；
2. 把 rootfs bind 到自身，作为 `pivot_root` 的挂载点；
3. 挂载 `/proc`、只读 `sysfs`、`/dev`(tmpfs + 设备节点 + devpts/`/dev/shm`)、`/tmp`；
4. `pivot_root` 切换根目录并卸载旧根；
5. 设置 hostname、拉起 `lo`、应用 bind/tmpfs、切换 uid/gid、chdir；
6. 最后 `execve` 目标命令（或进入微型 init）。

对于 proot，这一切通常靠 ptrace 逐条拦截并改写路径——goroot 让内核原生完成，
因此没有解释执行的开销，行为也更接近真实容器。

## 兼容性与优雅降级

`goroot` 不假设所有命名空间都存在。启动前它用 `clone(2)` 逐个**探测**能力
（注意：不能在 Go 进程内直接 `unshare()`——Go 是多线程的，内核会拒绝；必须像
真正的容器一样通过 `fork+exec` 探测），然后按优先级逐项降级，并在 stderr 给出
警告，而不是一刀切地硬失败。

```sh
goroot doctor   # 打印本机支持情况与将发生的降级
```

```
kernel          : 7.0.10-1-liquorix-amd64 (x86_64)
euid            : 1000 (unprivileged)
namespaces      :
  user   ok
  mount  ok
  pid    ok
  uts    ok
  ipc    ok
  net    ok
verdict         :
  can run: full isolation
```

| 能力 | 最低内核 | 缺失时的降级行为 |
| --- | --- | --- |
| user namespace | 3.8（且发行版未禁用） | 非 root：直接报错并给出指引；root：跳过，改为完整的真实 root 容器 |
| mount ns + pivot_root | 2.6.x | 必需（缺失无法做成容器） |
| pid namespace | 2.6.24 | 关闭，改为 bind 主机 `/proc` |
| uts / ipc / net ns | 2.6.19 / 3.0 / 2.6.24 | 逐项关闭并警告（不再改主机 hostname / 不碰主机 lo） |
| userns 内挂 procfs | 3.8 | 回退为 bind 主机 `/proc` |
| userns 内挂 sysfs | 3.8~4.x | 回退为只读 bind 主机 `/sys`；`--share-net` 下只能 bind 主机 `/sys` |
| devpts `newinstance` | 4.7 | 回退为 bind 主机 `/dev/pts` |
| 在 userns/tmpfs 建普通文件 | 3.8 | 必需（`/dev` 设备节点用 bind 而非 `mknod`） |

关键安全点：当某个命名空间探测失败被降级时，会同步更新传给子进程的标志，避免
副作用落到主机上——例如 UTS 不可用就**不**调用 `sethostname`（否则会改到主机
hostname），网络命名空间不可用就**不**去拉起主机的 `lo`。

如果内核太老、且既没有 user namespace 又没有 root 权限，则任何「真」隔离都无从
谈起——这正是 proot 存在的理由（用 ptrace 在用户态伪造）。此时 `goroot` 会明确
提示你去启用 userns 或改用 proot/bwrap，而不是给出一个晦涩的 `EPERM`。

调试/强制降级：设置 `GOROOT_DISABLE_NS=pid,uts,net`（逗号分隔）可让 `goroot` 假装
这些命名空间不可用，用于验证降级路径。

## 限制与已知问题

- **单 uid 映射**：无特权 user namespace 只能把「当前用户 ↔ 容器 root」做一对一
  映射。因此容器内只有 uid/gid 0 可用；`-u/--gid` 指定非 0 会报错。若要支持
  多用户，需要借助 `/etc/subuid` + `newuidmap`（尚未实现）。
- **补充组**：因为 `setgroups` 在 user namespace 内被禁止，主机的补充组会显示为
  `nobody(65534)`（仅显示影响，文件权限不受影响）。
- **`--share-net` 下 `/sys`**：sysfs 绑定在 network namespace 上，共享主机 netns
  时只能 bind 主机 `/sys`（只读），会带上主机的子挂载。
- **尚无 cgroup 资源限制 / seccomp 过滤**：当前只做命名空间隔离。
- rootfs 内的文件最好由当前用户拥有（用 `goroot extract` 解包即可）；否则那些
  不属于你的文件在容器内会显示为 `nobody` 且可能不可读。

## 开发

```sh
make fmt vet        # 格式化和静态检查
make test           # 冒烟测试（自动准备 alpine rootfs，需能联网）
ROOTFS=./myrootfs ./scripts/smoke.sh   # 复用已有 rootfs
```

代码结构：

| 文件 | 职责 |
| --- | --- |
| `main.go` | CLI 解析与命令分发 |
| `spec.go` | 容器描述结构（JSON 传给子进程） |
| `run.go` | 父进程侧：能力探测、降级、flags/uid 映射、启动 |
| `probe_linux.go` | 命名空间能力探测、`doctor` 自检 |
| `init_linux.go` | 子进程侧：挂载、pivot_root、exec、微型 init |
| `net_linux.go` | 拉起 lo、终端检测 |
| `extract.go` | tar 解包（本地/URL） |
