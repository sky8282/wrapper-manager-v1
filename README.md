
![1](https://github.com/user-attachments/assets/f713effd-3d2e-4435-88f1-7bb5a0e9559d)

# wrapper-manager-v1版
一个基于 Web 的轻量级进程管理工具，专为管理 `wrapper` 二进制程序设计。它提供了一个可视化的仪表盘，支持多进程并发管理、日志实时查看、健康检查以及断线自动重启。
## ✨ 功能特性
* **Web 可视化界面**：直观的卡片式布局，实时监控所有进程状态。
* **多进程管理**：支持添加、停止、重启多个 Wrapper 实例。
* **实时日志**：通过 WebSocket 实时推送进程输出日志（支持 PTY 伪终端）。
* **健康检查**：自动检测指定端口连通性，异常时自动标记状态。
* **自动重启**：进程意外退出或崩溃时自动尝试重启。
* **配置持久化**：自动保存进程列表，重启管理器后自动恢复之前的任务。
* **wrapper项目**：https://github.com/zhaarey/wrapper
* **多线程多区域项目分支**：https://github.com/sky8282/apple-music-downloader

## 🛠️ 环境要求
* Linux (推荐 Debian/Ubuntu)
* Go 1.18+ (仅编译需要)
* 目标 `wrapper` 可执行文件

## 🚀 部署指南
### 1. 准备目录结构
服务器上目录结构如下：
```text
/root/wrapper/
├── main.go           # 源码 (或者编译好的 wrapper-manager)
├── index.html        # 前端界面
├── wrapper           # 你的业务二进制程序 (必须存在)
└── rootfs            # wrapper相关的文件夹
```
# 初始化 Go 模块
```text
go mod init wrapper-manager
```
# 下载依赖 (WebSocket 和 PTY 库)
```text
go mod tidy
```
# 编译程序
```text
go build -o wrapper-manager .
```
# 赋予执行权限
```text
chmod +x wrapper-manager
```

启动参数说明:
* --wrapper-bin: 指定被管理的二进制文件路径（默认为 ./wrapper）。
* --port: Web 界面监听端口（默认为 8080）。
* --config: 配置文件路径（默认为 manager.json，会自动创建）。

使用以下命令启动管理器：
```text
./wrapper-manager --wrapper-bin="./wrapper" --port=8080
```
在web界面添加新进程：
* 点击仪表盘上的 "+ 添加新进程" 卡片。
* 输入完整的启动命令，如：-H 0.0.0.0 -D 10020 -M 10021 或者 -H 0.0.0.0 -D 10020 -M 10021 -L 邮箱:密码。
* 注意：命令中必须包含 -D <端口> 参数，管理器将使用该端口作为进程的唯一 ID 进行识别和健康检查。


