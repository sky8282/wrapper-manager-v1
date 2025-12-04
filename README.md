
![2](https://github.com/user-attachments/assets/24df2618-d522-4382-86bd-fbb8307981eb)

# wrapper-manager-v1版
一个基于 Web 的轻量级进程管理工具，专为管理 `wrapper` 二进制程序设计。它提供了一个可视化的仪表盘，支持多进程并发管理、日志实时查看、健康检查以及断线自动重启。
## ✨ 功能特性
* **Web 可视化界面**：直观的卡片式布局，实时监控所有进程状态。
* **多进程管理**：支持添加、停止、重启多账号多端口的 `wrapper` 进程。
* **实时日志**：通过 WebSocket 实时推送进程输出日志（支持 PTY 伪终端）。
* **健康检查**：自动检测指定端口连通性，异常时自动标记状态并重启。
* **监控关键字**：监控到日志出现关键字，如：`KDCanProcessCKC` 将重启进程，可自行添加多个其他关键字。
* **自动重启**：进程意外退出或崩溃时自动尝试重启。
* **配置持久化**：自动保存进程列表，重启管理器后自动启动列表里的进程。
* **wrapper项目**：https://github.com/zhaarey/wrapper  或 https://github.com/WorldObservationLog/wrapper
* **apple-music-downloader 原版**：https://github.com/zhaarey/apple-music-downloader
* **apple-music-downloader 多线程多区域版本分支**：https://github.com/sky8282/apple-music-downloader

## 🛠️ 环境要求
* Linux (Debian/Ubuntu)
* Go 1.18+ (仅编译需要)
* 管理目标 `wrapper` 项目

## 🚀 部署与使用指南
### 目录结构如下：
```text
/root/wrapper/
├── main.go           # 源码 (或者编译好的 wrapper-manager)
├── config.yaml       # wrapper-manager 配置文件
├── index.html        # 前端界面
├── wrapper           # wrapper 二进制程序
└── rootfs            # wrapper 相关的文件夹
```
### 初始化 Go 模块：
```text
go mod init wrapper-manager
```
### 下载依赖 (WebSocket 和 PTY 库)：
```text
go mod tidy
```
### 编译程序：
```text
go build -o wrapper-manager .
```
### 赋予执行权限：
```text
chmod +x wrapper-manager
```
### 使用以下命令启动管理器：
* 请根据账号区域等进行修改 config.yaml 参数并启动：
```text
./wrapper-manager
``` 
### 在web界面添加新进程：
* 点击仪表盘上的 "+ 添加新进程" 卡片。
* 输入或选择账号对应的区域。
* 输入完整的启动命令，如：`-H 127.0.0.1 -D 10020 -M 10021` 或 `-H 127.0.0.1 -D 10020 -M 10021 -L 邮箱:密码，或 其他启动参数`。
* 在日志里查看并发送 `2fa` 验证码。
* 注意：命令中必须包含 -D <端口> 参数，管理器将使用该端口作为进程的唯一 ID 进行识别和健康检查。
