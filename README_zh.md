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

- **开箱即用**：二进制内置一个 Alpine minirootfs，不传 `--root` 时直接使用。
- **合理默认**：默认共享主机网络、只读挂载主机 `/etc/resolv.conf`、自动挑选
  shell，所以 `goroot run` 直接可用（`apk`/`curl` 开箱能跑）。
- **无需 root**：靠 unprivileged user namespace 把当前用户映射成容器内的 root。
- **真正的隔离**：user / mount / pid / uts / ipc / net 六个命名空间（可单独关闭）。
- **后台任务**：一对 `server`/`client` 守护进程子命令（`~/.goroot` 下的 unix socket），
  以分离方式运行容器并管理日志。
- **单进程**：默认直接把目标命令 exec 成容器内的 PID 1，每个容器无额外守护进程。
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

内置 rootfs 位于 `assets/`（`alpine-minirootfs-3.20.0-x86_64.tar.gz`），通过
`go:embed` 编译进二进制。要更新它，把新的 tarball 放进 `assets/` 并修改
`assets.go` 里的常量即可。

## 快速开始

二进制内置了一个精简的 Alpine rootfs，可以立即启动 —— 不传参数会直接进入一个 shell：

```sh
./goroot run -- /bin/sh        # 或直接：./goroot run
```

默认共享主机网络、并可用主机 DNS 解析，所以下面这条开箱即用：

```sh
./goroot run -- /bin/sh -c 'apk add --no-cache curl && curl -s https://example.com | head -1'
```

首次使用会把内置 rootfs 解包到 `~/.cache/goroot/alpine-3.20.0-x86_64`，之后复用。
要用自己的 rootfs，传 `-r DIR` 即可：

```sh
# 解包一个你下载的 rootfs（不需要 root，文件归当前用户所有）
./goroot extract \
  https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/x86_64/alpine-minirootfs-3.21.3-x86_64.tar.gz \
  rootfs

./goroot run -r rootfs -- /bin/sh
```

### 网络

默认容器**共享主机网络**，且主机的 `/etc/resolv.conf` 以只读方式挂入，所以包管理
器和 `curl` 无需额外参数即可工作：

```sh
./goroot run -r rootfs -- /bin/sh -c 'apk add --no-cache curl && curl -s https://example.com | head -1'
```

若需要独立的网络命名空间（只有 `lo`），传 `--isolate-net`。`--no-resolv` 可关闭
主机 DNS 挂载，`--share-net`（与默认相同）可强制共享。

## 用法

```
goroot run [options] [--] <command> [args...]
goroot extract [-c N] <tarball|url> <dest>
goroot server [-d|--daemon]
goroot client <子命令>       # run | ps | logs | stop | rm | status | shutdown
goroot doctor
goroot version
```

`run` 选项：

| 选项 | 说明 |
| --- | --- |
| `-r, --root DIR` | rootfs 目录（默认：内置 Alpine minirootfs） |
| `--hostname NAME` | 容器主机名（默认 `goroot`） |
| `-b, --bind SRC[:DST][,ro]` | 绑定挂载主机路径（可重复） |
| `--ro-bind SRC[:DST]` | 只读绑定挂载（可重复） |
| `--tmpfs DST` | 在 DST 挂载 tmpfs（可重复） |
| `-e, --env KEY=VAL` | 设置环境变量（可重复） |
| `-w, --cwd DIR` | 容器内工作目录（默认 `/`） |
| `--share-net` | 与主机共享网络命名空间（默认） |
| `--isolate-net` | 使用独立网络命名空间（只有 `lo`） |
| `--no-resolv` | 不自动挂载主机 `/etc/resolv.conf` |
| `--no-pid` / `--no-ipc` / `--no-uts` | 不创建对应命名空间 |
| `-u, --uid N` / `-g, --gid N` | 容器内运行身份（见下方限制） |
| `--init` | 以微型 init 作 PID 1（回收僵尸、转发信号） |
| `--keep-env` | 保留主机环境变量 |

`extract` 选项：`-c N` / `--strip N` 去掉 N 层路径前缀。

默认值：`--root` = 内置 Alpine minirootfs；共享主机网络；主机 `/etc/resolv.conf`
只读挂入；shell 按 `/bin/sh`、`/bin/bash`、`/bin/zsh`、`/bin/fish` 顺序自动选择。

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

## 后台任务（守护进程）

`goroot server` 运行一个小守护进程（监听 `~/.goroot/goroot.sock`），可以启动
**分离式**（detached）容器；`goroot client` 与它通讯。客户端首次需要时会自动拉起
守护进程（设置 `GOROOT_NO_AUTOSTART=1` 可禁用自动拉起）。

```sh
goroot server -d                          # 后台启动守护进程
goroot client status                      # 查看是否在运行

id=$(goroot client run -- /bin/sh -c 'while :; do date; sleep 5; done')
goroot client ps                          # 列出任务
goroot client logs -f $id                 # 跟踪日志
goroot client stop $id                    # 先 SIGTERM，必要时升级为 SIGKILL
goroot client rm $id                      # 删除已完成任务
goroot client shutdown                    # 关闭守护进程及其所有任务
```

`client run` 支持与 `run` 相同的全部选项。分离任务的 PID 1 是微型 init，因此
`stop` 是优雅的 SIGTERM（退出码 143），孤儿进程也会被回收。任务状态在内存中：
守护进程停止后其任务会被杀掉且列表丢失。日志位于 `~/.goroot/logs/<id>.log`。

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
| `assets.go` | 内置默认 rootfs（`assets/*.tar.gz`）、缓存解包 |
| `protocol.go` | 守护进程通讯协议 + `~/.goroot` 路径辅助 |
| `daemon.go` | `server`：任务管理、unix socket 监听、日志流式输出 |
| `client.go` | `client`：run/ps/logs/stop/rm/status/shutdown |
