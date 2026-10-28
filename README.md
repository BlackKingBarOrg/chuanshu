# 传书 Chuanshu

> 给 Omarchy 小团队的局域网飞鸽：无服务器、无账号、无云端，开机即可发现同事、聊天、高速传文件。

[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](https://www.gnu.org/licenses/gpl-3.0)

传书是一个只在局域网内工作的聊天与文件传输工具，为几个人到十几个人的小团队设计。它借鉴了老牌“飞鸽传书”的产品形态（自动发现、点对点直连、大文件快传），并借鉴 [bitchat](https://github.com/permissionlesstech/bitchat) 的身份与加密设计（密钥即身份、Noise 加密）。所有通信都是同网段设备之间的点对点连接，不经过任何服务器。

## 界面预览

打开后先选人、再聊天，和一般聊天软件一致：

```text
聊天对象（选人）          Bob
▸ ● Bob                  在线 · 身份已核对 ✓
  ○ 李四                   按 Enter 开始聊天
  # 全员广播（群聊）       最近消息: …
↑↓/jk 选择 · Enter 聊天
打字搜索 · 单击选/双击进    在左侧选一位同事，按 Enter 开始聊天
```

进入聊天后，顶部明示“与 Bob 聊天 · 在线 · 已加密”，输入框前固定标注“发给 Bob:”，不会再出现“以为私聊、其实群发”的事故。

## 功能

- **零配置发现**：mDNS + UDP 广播双通道自动发现同事；广播被屏蔽时可手动按 IP 连接
- **先选人再聊天**：选人列表（在线优先、支持打字搜索和鼠标点选）、未读数、最近消息预览
- **聊天**：一对一与全员广播、送达/已读两级回执、聊天记录本地保存
- **离线即失败**：对方不在线时发送立即明确报错，不排队、不假装送达（有意的设计取舍）
- **文件传输**：文件、文件夹、图片附件；接收前必须确认才落盘；进度与速度、断点续传、整文件 SHA-256 校验
- **附件体验**：`/file` 选路径、拖文件进终端、复制图片后粘贴为附件；聊天中按一键用系统默认程序打开
- **加密与身份**：长期密钥即身份、公告签名防冒充、首次当面核对短码后钉住公钥、私聊与文件走 Noise 加密
- **桌面集成**：后台守护进程开机自启、桌面通知、框选截图直发同事

## 快速开始

环境要求：Linux（面向 Omarchy / Arch 的 Wayland 桌面）、Go 工具链（版本见 [go.mod](go.mod)）。

```sh
git clone <本仓库地址>
cd chuanshu
sh scripts/install.sh
chuanshu
```

安装脚本会把二进制装到 `~/.local/bin`，并启用 systemd 用户服务（守护进程开机自启、崩溃自拉）。

若同事互相看不见，先检查两件事：防火墙放行 **TCP 24247** 与 **UDP 24246**（例如 `sudo ufw allow 24247/tcp && sudo ufw allow 24246/udp`）；以及路由器是否开了客户端隔离或分了网段——纯局域网直连要求大家在同一平坦局域网内。

## 使用

### TUI（主要入口）

直接运行 `chuanshu`：

| 操作 | 按键 |
|---|---|
| 选择同事 | `↑`/`↓` 或 `j`/`k`，或鼠标单击 |
| 按名字搜索 | 在选人界面直接打字，`Esc` 清空 |
| 进入聊天 | `Enter`（或双击） |
| 回列表换人 | `Esc` |
| 聊天中快速切换 | `Ctrl+N` / `Ctrl+P` |
| 粘贴图片为附件 | `Ctrl+V`（复制的图片或文件直接粘） |
| 添加文件附件 | 输入 `@` 模糊选文件（`↑`/`↓` 选、`Tab`/`Enter` 确认），或把文件拖进终端；`/file <路径>` 仍可用 |
| 清空待发附件 | `Ctrl+X` |
| 打开收到的图片/文件 | 鼠标点击附件行，或 `Ctrl+O` 打开最近一个 |
| 在文件管理器里定位附件 | `Ctrl`+点击（或 `Alt`+点击）附件行 |
| 接收 / 拒绝文件 | `y` / `n` |
| 退出 | `Ctrl+C` |

### 命令行

```sh
chuanshu peers                          # 同事与在线状态
chuanshu send 张三 开会了                # 发消息（对方不在线立即失败）
chuanshu send '#all' 开会了              # 全员广播
chuanshu file 张三 ~/报告.pdf            # 发文件或文件夹
chuanshu shot 张三                       # 框选截图并发送
chuanshu offers                         # 待确认接收的文件
chuanshu accept <ID> / reject <ID>      # 接受 / 拒绝
chuanshu code 张三                       # 查看身份核对码
chuanshu trust 张三                      # 当面核对一致后标记已验证
chuanshu peer-add 192.168.1.20:24247    # 广播被屏蔽时手动连接
chuanshu history 张三                    # 聊天记录
chuanshu id                             # 本机身份信息
```

## 安全模型

- **身份是密钥，不是昵称**。每台机器首次运行生成长期密钥，身份为其指纹；改昵称冒充不了人。
- **公告带签名**，发现通道里伪造的“在线通知”会被丢弃。
- **首次通信自动记录对方公钥**（TOFU）。用 `chuanshu code <同事>` 当面或电话核对短码，一致后 `chuanshu trust <同事>` 钉住；之后公钥被替换连接会被拒绝。
- **私聊与文件全程 Noise 加密**（XX 握手、双向认证、前向保密），局域网抓包只能看到密文。
- **文件落盘前必须确认**，文件名与路径经过清洗（防路径穿越），半截文件以 `.part` 暂存，校验通过才转正。

已知边界：纯局域网没有离线投递——对方关机就发不过去，这是刻意取舍；跨网段、客户端隔离的网络第一版不支持（需另加中继，不在当前范围）。

## 工作原理

```text
┌─────────┐   本地 IPC    ┌──────────────┐  mDNS/UDP 广播发现
│ TUI/CLI │ ────────────▶ │  守护进程     │ ─ ─ ─ ─ ─ ─ ─ ─▶ 同事的守护进程
└─────────┘               │  发现·身份    │  Noise 加密 TCP 直连（聊天/文件）
                          │  聊天·文件    │ ◀ ─ ─ ─ ─ ─ ─ ─ ─  同事的守护进程
                          └──────────────┘
```

- 单个 Go 二进制，三种模式：`daemon`（守护进程）、TUI（默认）、CLI 子命令
- 守护进程负责发现、维护加密连接、收发与落盘；关掉终端窗口照样能收文件
- 传输层与业务层分离，将来要加跨网段中继只需改传输层

本地数据位置：配置在 `~/.config/chuanshu/config.toml`（昵称、端口可改），密钥与聊天记录在 `~/.local/share/chuanshu`（密钥文件权限 `600`），接收文件默认存 `~/Downloads/Chuanshu`。

## 开发

```sh
make run     # 一条命令：编译并直接打开 TUI（守护进程会自动在后台启动）
make build   # 只编译，产出 ./chuanshu
make test    # 单元 + 双守护进程集成测试
make vet
```

仓库布局：

| 路径 | 职责 |
|---|---|
| [cmd/chuanshu](cmd/chuanshu) | 程序入口、CLI 命令 |
| [internal/daemon](internal/daemon) | 守护进程：发现、连接、聊天、文件传输、本地 IPC |
| [internal/tui](internal/tui) | Bubble Tea 终端界面（选人 + 聊天） |
| [internal/wire](internal/wire) | Noise 会话与线上协议、文件名清洗 |
| [internal/identity](internal/identity) | 长期密钥、指纹与核对码 |
| [internal/store](internal/store) | 同事与聊天记录的本地持久化 |
| [internal/ipc](internal/ipc) | TUI/CLI 与守护进程的本地通信客户端 |
| [internal/appcfg](internal/appcfg) | 配置与路径解析 |

设计背景、取舍与分步方案见 [设计文档](.agents/plans/2026-10-27-chuanshu-design.md)。

本地双开自测：两个终端分别用 `CHUANSHU_HOME=/tmp/a` 与 `CHUANSHU_HOME=/tmp/b` 运行，再用 `chuanshu peer-add 127.0.0.1:<对方端口>` 互连即可（注意本机 WiFi 可能不环回广播，同机自测请走手动连接）。

如果 Go 是用 mise 装的（不在 PATH 里），把 `GO` 指过去即可：`make run GO="mise x go -- go"`。已经安装到系统的版本日常使用直接运行 `chuanshu`，不需要编译。

## 路线图

- 顶栏未读提醒组件、跟随 Omarchy 主题配色
- 更完整的群聊（命名群组，而非仅全员广播）
- 跨网段时的可选加密中继（如确有需求）

欢迎提 issue 和 PR。提 PR 前请先跑通 `make test`。

## 许可证

本项目以 **GNU 通用公共许可证第 3 版或（由你选择）更高版本**（GPL-3.0-or-later）发布，全文见 [LICENSE](LICENSE)。

鸣谢：产品形态受“飞鸽传书 / IP Messenger”启发；身份、公告签名与 Noise 加密设计借鉴 [bitchat](https://github.com/permissionlesstech/bitchat)（公共领域/Unlicense）；TUI 基于 [Bubble Tea](https://github.com/charmbracelet/bubbletea)。
