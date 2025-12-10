
![2](https://github.com/user-attachments/assets/5bd2ad6f-83cb-4955-bf99-3e84594290db)

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
├── manager.json      # 进程配置文件，添加进程会自动生成
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
### 负载均衡状态下显示解密速度需安装 iproute2 ：
```text
apt update
apt install -y iproute2
```
### 在web界面添加新进程：
* 点击仪表盘上的 "+ 添加新进程" 卡片。
* 输入或选择账号对应的区域。
* 输入完整的启动命令，如：`-H 127.0.0.1 -D 10020 -M 10021` 或 `-H 127.0.0.1 -D 10020 -M 10021 -L 邮箱:密码，或 其他启动参数`。
* 在日志里查看并发送 `2fa` 验证码。
* 注意：命令中必须包含 -D <端口> 参数，管理器将使用该端口作为进程的唯一 ID 进行识别和健康检查。
* 仅个人理解的解密流程：
```mermaid
flowchart TD
    %% 核心流程
    A["Apple Music"] --> B["Content ID<br>(歌曲ID)"]
    B --> C["获取 Authorization Token (AT)<br>有效期: 数小时~1天<br>解密限额: ~1000首"]
    C --> D["调用 KDProcessPersistentKeyWithAT<br>(AT + Content ID)"]

    %% 成功分支
    D -->|"✅ 解密成功"| NormalPlay["生成 Persistent Key<br>进入离线播放/解密流程"]

    %% 失败判定中心
    D -->|"❌ 捕获异常 (Catch Exception)"| ErrorHandler{{"分析错误类型"}}

    %% 分支 1: 软故障 (可恢复)
    ErrorHandler -->|"类似状态码: -42812 等<br>Persistent Key Error"| SoftError["⚠️ 事务性错误 (Soft Fault)<br>网络波动 或:<br>Key Server 拒绝"]
    SoftError -->|"动作: 丢弃当前 Key"| RefreshAT["刷新 AT / 重新请求"]
    RefreshAT --> D

    %% 分支 2: 硬故障 (会话冲突/致死)
    ErrorHandler -->|"类似状态码: -42829 等<br>FairPlay error / Context Invalid"| HardError["⛔ 会话冲突/上下文损坏 (Hard Fault)<br>并发导致 DeviceID 互踢<br>或者 Session 句柄泄露"]
    HardError -->|"动作: 熔断保护"| KillProcess["🔴 标记进程不健康<br>触发重启 / 重新登录"]
    KillProcess --> Restart["重新初始化实例<br>(生成新 DeviceID / Session)"]
    Restart -.->|"恢复后"| C

    %% 分支 3: 风控/限制
    ErrorHandler -->|"关键词: Invalid CKC <br>高负载保护"| RateLimit["⏳ 频率限制 (Rate Limit)<br>请求过快 / 服务器风控"]
    RateLimit -->|"动作: 强制冷却"| CoolDown["🧊 Sleep (休眠) 3-10秒<br>等待服务器重置计数"]
    CoolDown --> RefreshAT

    %% 样式定义
    style A fill:#2c2c2c,stroke:#fff,color:#fff
    style B fill:#1c1c1e,stroke:#007AFF,color:#fff
    style C fill:#1c1c1e,stroke:#007AFF,color:#fff
    style D fill:#1c1c1e,stroke:#007AFF,color:#fff
    style NormalPlay fill:#007AFF,stroke:#007AFF,color:#fff
    
    style ErrorHandler fill:#FF9F0A,stroke:#fff,color:#000
    
    style SoftError fill:#3a3a3c,stroke:#FF9F0A,color:#fff
    style HardError fill:#3a3a3c,stroke:#FF3B30,color:#fff
    style RateLimit fill:#3a3a3c,stroke:#30D158,color:#fff
    
    style KillProcess fill:#8B0000,stroke:#FF3B30,color:#fff
    style CoolDown fill:#004d00,stroke:#30D158,color:#fff
