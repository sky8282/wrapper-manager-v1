package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

const MaxLogLines = 200

var GlobalRestartPatterns []string
var globalRoundRobinCounter uint64

var bufferPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 1024*1024)
		return &b
	},
}

type PeekedConn struct {
	net.Conn
	peeked []byte
}

func (c *PeekedConn) Read(p []byte) (n int, err error) {
	if len(c.peeked) > 0 {
		n = copy(p, c.peeked)
		c.peeked = c.peeked[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

type VirtualListener struct {
	addr    net.Addr
	conns   chan net.Conn
	closed  chan struct{}
	closeMu sync.Mutex
}

func NewVirtualListener(addr net.Addr) *VirtualListener {
	return &VirtualListener{
		addr:   addr,
		conns:  make(chan net.Conn, 100),
		closed: make(chan struct{}),
	}
}

func (l *VirtualListener) Accept() (net.Conn, error) {
	select {
	case c, ok := <-l.conns:
		if !ok {
			return nil, net.ErrClosed
		}
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *VirtualListener) Close() error {
	l.closeMu.Lock()
	defer l.closeMu.Unlock()
	select {
	case <-l.closed:
		return nil
	default:
		close(l.closed)
		close(l.conns)
	}
	return nil
}

func (l *VirtualListener) Addr() net.Addr {
	return l.addr
}

type WrapperStatus struct {
	Status     string `json:"status"`
	Speed      string `json:"speed"`
	Percentage int    `json:"percentage"`
}

type ProcessState string

const (
	StateStarting ProcessState = "STARTING"
	StateRunning  ProcessState = "RUNNING"
	StateStopped  ProcessState = "STOPPED"
	StateFailed   ProcessState = "FAILED"
)

type ProxyUpdate struct {
	Region   string
	Addr     string
	IsRemove bool
	Type     string
}

type ManagedProcess struct {
	ID              string       `json:"id"`
	M3U8Port        string       `json:"m3u8Port"`
	Region          string       `json:"region"`
	Command         string       `json:"command"`
	Args            []string     `json:"args"`
	RestartPatterns []string     `json:"restartPatterns"`
	WrapperPath     string       `json:"-"`
	State           ProcessState `json:"state"`
	PID             int          `json:"pid"`
	StartTime       time.Time    `json:"startTime"`
	Speed           string       `json:"speed,omitempty"`
	NetSpeed        string       `json:"netSpeed"`
	prevBytes       uint64
	ProxyBytesSent  uint64 `json:"-"`
	isRemoved       bool
	ctx             context.Context
	cancel          context.CancelFunc
	cmd             *exec.Cmd
	ptmx            io.ReadWriteCloser
	mutex           sync.Mutex
	logBuffer       []string
	restartCounter  int
	RetryCount      int
	ActiveConn      int64     `json:"activeConn"`
	LastUseTime     time.Time `json:"-"`
}

type WriteCounter struct {
	Total  *uint64
	Writer io.Writer
}

func (wc *WriteCounter) Write(p []byte) (int, error) {
	n, err := wc.Writer.Write(p)
	if n > 0 {
		atomic.AddUint64(wc.Total, uint64(n))
	}
	return n, err
}

type SystemStats struct {
	OSInfo      string  `json:"os_info"`
	CPUUsage    float64 `json:"cpu"`
	MemUsage    float64 `json:"mem"`
	NetDownRate float64 `json:"net_down"`
	NetUpRate   float64 `json:"net_up"`
}

type ProcessConfig struct {
	ID              string   `json:"id"`
	Region          string   `json:"region"`
	Command         string   `json:"command"`
	Args            []string `json:"args"`
	RestartPatterns []string `json:"restartPatterns"`
}

type ServerConfig struct {
	WebListen string `yaml:"web_listen"`
}

type WrapperConfig struct {
	Path            string   `yaml:"path"`
	StateFile       string   `yaml:"state_file"`
	RestartPatterns []string `yaml:"restart_patterns"`
}

type RegionConfig struct {
	DecryptPort string `yaml:"decrypt-m3u8-port"`
	GetPort     string `yaml:"get-m3u8-port"`
}

type Manager struct {
	WrapperPath    string
	Processes      map[string]*ManagedProcess
	mutex          sync.RWMutex
	ConfigPath     string
	proxyListeners []chan ProxyUpdate
	listenerLock   sync.Mutex
	ServerOS       string
}

func NewManager(wrapperPath string, configPath string) *Manager {
	m := &Manager{
		WrapperPath:    wrapperPath,
		Processes:      make(map[string]*ManagedProcess),
		ConfigPath:     configPath,
		proxyListeners: make([]chan ProxyUpdate, 0),
		ServerOS:       getOSName(),
	}
	m.LoadConfig()
	return m
}

func getOSName() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			full := strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), "\"")
			parts := strings.Fields(full)
			if len(parts) > 0 {
				return parts[0]
			}
			return full
		}
	}
	return "Linux"
}

func (m *Manager) SubscribeProxyUpdates() chan ProxyUpdate {
	m.listenerLock.Lock()
	defer m.listenerLock.Unlock()
	ch := make(chan ProxyUpdate, 100)
	m.proxyListeners = append(m.proxyListeners, ch)
	return ch
}

func (m *Manager) notifyProxies(u ProxyUpdate) {
	m.listenerLock.Lock()
	defer m.listenerLock.Unlock()
	for _, ch := range m.proxyListeners {
		select {
		case ch <- u:
		default:
		}
	}
}

func getPortFromArgs(args []string, flagName string) string {
	for i, arg := range args {
		if (arg == flagName) && i+1 < len(args) {
			_, port, err := net.SplitHostPort(args[i+1])
			if err == nil {
				return port
			}
			return args[i+1]
		}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, flagName) && len(arg) > len(flagName) {
			return strings.TrimPrefix(arg, flagName)
		}
	}
	return ""
}

func NewManagedProcess(id string, region string, wrapperPath string, command string, args []string, patterns []string) *ManagedProcess {
	ctx, cancel := context.WithCancel(context.Background())
	if region == "" {
		region = "cn"
	}
	mPort := getPortFromArgs(args, "-M")

	if len(patterns) == 0 {
		patterns = make([]string, len(GlobalRestartPatterns))
		copy(patterns, GlobalRestartPatterns)
	}

	return &ManagedProcess{
		ID:              id,
		M3U8Port:        mPort,
		Region:          region,
		Command:         command,
		Args:            args,
		RestartPatterns: patterns,
		WrapperPath:     wrapperPath,
		State:           StateStopped,
		ctx:             ctx,
		cancel:          cancel,
		logBuffer:       make([]string, 0, MaxLogLines),
		StartTime:       time.Time{},
		Speed:           "N/A",
		NetSpeed:        "N/A",
		prevBytes:       0,
		ProxyBytesSent:  0,
		isRemoved:       false,
		restartCounter:  0,
		RetryCount:      0,
		ActiveConn:      0,
	}
}

func (p *ManagedProcess) Start() {
	go p.runLoop()
}

func (p *ManagedProcess) Stop() {
	p.logToBuffer("\033[33m--- 收到停止命令 ---\033[0m")
	p.cancel()
	p.mutex.Lock()
	pidToKill := 0
	if p.cmd != nil && p.cmd.Process != nil {
		pidToKill = p.cmd.Process.Pid
	}
	p.mutex.Unlock()
	if pidToKill != 0 {
		if err := syscall.Kill(-pidToKill, syscall.SIGKILL); err != nil {
			p.logToBuffer(fmt.Sprintf("\033[33m进程组 %d 终止异常: %v\033[0m", pidToKill, err))
		} else {
			p.logToBuffer(fmt.Sprintf("\033[33m--- 进程组 %d 已终止 (SIGKILL) ---\033[0m", pidToKill))
		}
	}
}

func (p *ManagedProcess) Write(data string) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if p.ptmx == nil {
		return fmt.Errorf("进程尚未启动 (ptmx is nil)")
	}
	p.logToBuffer(fmt.Sprintf("> %s", data))
	_, err := p.ptmx.Write([]byte(data + "\n"))
	return err
}

func (p *ManagedProcess) setState(newState ProcessState) {
	p.mutex.Lock()
	if p.isRemoved {
		p.mutex.Unlock()
		return
	}
	if p.State == newState {
		p.mutex.Unlock()
		return
	}

	if p.State == StateStopped && newState == StateFailed {
		p.mutex.Unlock()
		return
	}

	oldState := p.State
	p.State = newState
	log.Printf("进程 [%s] 状态变为: %s", p.ID, p.State)

	if newState == StateStopped {
		p.PID = 0
		p.StartTime = time.Time{}
		p.Speed = "N/A"
		p.NetSpeed = "N/A"
		p.prevBytes = 0
		p.restartCounter = 0
		p.RetryCount = 0
	} else if newState == StateFailed {
		p.PID = 0
		p.StartTime = time.Time{}
		p.Speed = "N/A"
		p.NetSpeed = "N/A"
		p.prevBytes = 0
		p.restartCounter = 0
	}

	region := p.Region
	id := p.ID
	mPort := p.M3U8Port
	payload := *p
	p.mutex.Unlock()

	globalHub.BroadcastStateUpdate(&payload)

	if newState == StateRunning {
		globalManager.notifyProxies(ProxyUpdate{Region: region, Addr: "127.0.0.1:" + id, IsRemove: false, Type: "tcp"})
		if mPort != "" {
			globalManager.notifyProxies(ProxyUpdate{Region: region, Addr: "127.0.0.1:" + mPort, IsRemove: false, Type: "http"})
		}
	}
	if oldState == StateRunning && newState != StateRunning {
		globalManager.notifyProxies(ProxyUpdate{Region: region, Addr: "127.0.0.1:" + id, IsRemove: true, Type: "tcp"})
		if mPort != "" {
			globalManager.notifyProxies(ProxyUpdate{Region: region, Addr: "127.0.0.1:" + mPort, IsRemove: true, Type: "http"})
		}
	}
}

func (p *ManagedProcess) logToBuffer(line string) {
	p.logBuffer = append(p.logBuffer, line)
	if len(p.logBuffer) > MaxLogLines {
		p.logBuffer = p.logBuffer[len(p.logBuffer)-MaxLogLines:]
	}
	globalHub.BroadcastLog(p.ID, line)
}

func copyFileWithPerms(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return os.Chmod(dst, info.Mode())
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(dst, relPath)
		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			os.Remove(dstPath)
			return os.Symlink(linkTarget, dstPath)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFileWithPerms(path, dstPath)
	})
}

func setupInstance(id string, region string, wrapperPath string) (string, string, error) {
	absWrapperBin, err := filepath.Abs(wrapperPath)
	if err != nil {
		return "", "", err
	}
	srcDir := filepath.Dir(absWrapperBin)
	binName := filepath.Base(absWrapperBin)
	instanceRoot := filepath.Join(srcDir, "instances")

	instanceDir := filepath.Join(instanceRoot, fmt.Sprintf("%s_%s", region, id))

	if _, err := os.Stat(instanceDir); err == nil {
		return instanceDir, "./" + binName, nil
	}
	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		return "", "", err
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return "", "", err
	}
	
	for _, entry := range entries {
		name := entry.Name()
		if name == "instances" || name == "manager.json" || name == "nohup.out" || strings.HasSuffix(name, ".log") {
			continue
		}
		
		srcPath := filepath.Join(srcDir, name)
		dstPath := filepath.Join(instanceDir, name)
		
		if name == "rootfs" {
			if err := os.MkdirAll(dstPath, 0755); err != nil {
				return "", "", err
			}
			rootfsEntries, err := os.ReadDir(srcPath)
			if err != nil {
				return "", "", err
			}
			for _, rEntry := range rootfsEntries {
				rName := rEntry.Name()
				rSrcPath := filepath.Join(srcPath, rName)
				rDstPath := filepath.Join(dstPath, rName)
				
				if rName == "dev" {
					if err := os.MkdirAll(rDstPath, 0755); err != nil {
					}
					continue
				}

				if rName == "data" {
					if _, err := os.Stat(rDstPath); os.IsNotExist(err) {
						if err := copyDir(rSrcPath, rDstPath); err != nil {
							return "", "", err
						}
					}
				} else {
					os.RemoveAll(rDstPath)
					if err := copyDir(rSrcPath, rDstPath); err != nil {
						return "", "", err
					}
				}
			}
		} else {
			if entry.IsDir() {
				os.RemoveAll(dstPath)
				if err := copyDir(srcPath, dstPath); err != nil {
					return "", "", err
				}
			} else {
				os.Remove(dstPath)
				if err := copyFileWithPerms(srcPath, dstPath); err != nil {
					return "", "", err
				}
			}
		}
	}
	return instanceDir, "./" + binName, nil
}

func (p *ManagedProcess) runLoop() {
	p.setState(StateStarting)
	p.logToBuffer(fmt.Sprintf("\033[33m--- 正在启动: %s %s (Region: %s) ---\033[0m", p.WrapperPath, strings.Join(p.Args, " "), p.Region))
	if len(p.RestartPatterns) > 0 {
		p.logToBuffer(fmt.Sprintf("\033[33m--- 监控重启关键词: %v ---\033[0m", p.RestartPatterns))
	}

	instanceDir, binCommand, err := setupInstance(p.ID, p.Region, p.WrapperPath)
	if err != nil {
		p.logToBuffer(fmt.Sprintf("\033[31m!!! 实例环境创建失败: %v\033[0m", err))
		p.setState(StateFailed)
		return
	}
	p.logToBuffer(fmt.Sprintf("\033[32m--- 实例环境已就绪: %s ---\033[0m", instanceDir))
	processStartTime := time.Now()
	p.mutex.Lock()
	p.cmd = exec.CommandContext(p.ctx, binCommand, p.Args...)
	p.cmd.Dir = instanceDir
	p.cmd.Env = os.Environ()
	p.cmd.Env = append(p.cmd.Env, "ANDROID_ROOT=/")
	p.cmd.Env = append(p.cmd.Env, "ANDROID_DATA=/tmp")
	p.mutex.Unlock()
	ptmx, err := pty.Start(p.cmd)
	if err != nil {
		p.setState(StateFailed)
	} else {
		p.ptmx = ptmx
		defer func() {
			if p.ptmx != nil {
				p.ptmx.Close()
			}
		}()
		p.mutex.Lock()
		if p.cmd != nil && p.cmd.Process != nil {
			p.PID = p.cmd.Process.Pid
		} else {
			p.PID = 0
		}
		payload := *p
		p.mutex.Unlock()
		globalHub.BroadcastStateUpdate(&payload)

		healthCtx, healthCancel := context.WithCancel(p.ctx)
		defer healthCancel()

		checkPort := p.getCheckPort()
		if checkPort == "" {
			p.logToBuffer("\033[33m!!! 提示: 无法从参数中找到 -D 端口，健康检查已禁用\033[0m")
			p.setState(StateRunning)
		} else {
			go p.healthCheck(healthCtx, checkPort)
		}

		var isCKCError bool = false

		func() {
			scanner := bufio.NewReader(ptmx)
			p.restartCounter = 0
			var firstPatternTime time.Time

			for {
				line, err := scanner.ReadString('\n')
				if len(line) > 0 {
					l := strings.TrimRight(line, "\r\n")
					if strings.TrimSpace(l) == "" {
						if err == io.EOF {
							break
						}
						continue
					}

					if strings.Contains(l, "SVError: Invalid CKC error") {
						isCKCError = true
						p.logToBuffer("\033[33m!!! 检测到 CKC 错误，下一次重启将触发冷却保护\033[0m")
					}

					isWarning := strings.Contains(l, "WARNING:")

					matched := false
					for _, pattern := range p.RestartPatterns {
						if strings.Contains(l, pattern) {
							matched = true
							break
						}
					}

					if matched {
						now := time.Now()
						if p.restartCounter == 0 || now.Sub(firstPatternTime) > 60*time.Second {
							p.restartCounter = 1
							firstPatternTime = now
						} else {
							p.restartCounter++
						}

						if p.restartCounter >= 3 {
							p.logToBuffer(fmt.Sprintf("\033[31m!!! [监控] 60秒内检测到3次异常，触发重启: %s\033[0m", strings.TrimSpace(l)))
							p.mutex.Lock()
							if p.cmd != nil && p.cmd.Process != nil {
								p.cmd.Process.Kill()
							}
							p.mutex.Unlock()
							p.restartCounter = 0
						}
					}

					if isWarning {
						if err == io.EOF {
							break
						}
						continue
					}

					p.logToBuffer(l)
					var status WrapperStatus
					if err := json.Unmarshal([]byte(l), &status); err == nil {
						if status.Speed != "" {
							p.mutex.Lock()
							if p.Speed != status.Speed {
								p.Speed = status.Speed
								payload := *p
								p.mutex.Unlock()
								globalHub.BroadcastStateUpdate(&payload)
							} else {
								p.mutex.Unlock()
							}
						}
					}
				}
				if err != nil {
					break
				}
			}
		}()

		p.cmd.Wait()
		healthCancel()
		p.ptmx = nil

		if p.ctx.Err() == nil {
			p.logToBuffer("\033[31m--- 进程意外退出 (或被监控触发重启) ---\033[0m")
			p.setState(StateFailed)

			if time.Since(processStartTime) > 3*time.Second {
				p.mutex.Lock()
				p.RetryCount = 0
				p.mutex.Unlock()
			}

			p.mutex.Lock()
			p.RetryCount++
			p.mutex.Unlock()

			delay := 2 * time.Second

			if isCKCError {
				delay = 3 * time.Second
				p.logToBuffer(fmt.Sprintf("\033[33m--- 触发 CKC 错误保护，进入冷却模式，等待 %v 后重启... ---\033[0m", delay))
			} else {
				p.logToBuffer(fmt.Sprintf("\033[33m--- 正在尝试自动重启，等待 %v ... ---\033[0m", delay))
			}

			time.Sleep(delay)

			globalManager.mutex.RLock()
			_, exists := globalManager.Processes[p.ID]
			globalManager.mutex.RUnlock()

			if exists {
				p.logToBuffer("\033[33m--- 正在重启... ---\033[0m")
				p.Start()
			} else {
				p.logToBuffer("\033[33m--- 进程已被移除，取消重启 ---\033[0m")
			}
		} else {
			p.logToBuffer("\033[33m--- 进程已停止 ---\033[0m")
			p.setState(StateStopped)
		}
	}
}

func (p *ManagedProcess) getCheckPort() string {
	return p.ID
}

func (p *ManagedProcess) healthCheck(ctx context.Context, port string) {
	checkAddr := "127.0.0.1:" + port
	initialCheckOK := false

	for i := 0; i < 36; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			conn, err := net.DialTimeout("tcp", checkAddr, 3*time.Second)
			if err == nil {
				conn.Close()
				p.logToBuffer(fmt.Sprintf("\033[32m--- 健康检查通过: %s ---\033[0m", checkAddr))
				p.mutex.Lock()
				if p.StartTime.IsZero() {
					p.StartTime = time.Now()
				}
				p.mutex.Unlock()
				p.setState(StateRunning)
				initialCheckOK = true
				break
			}
		}
		if initialCheckOK {
			break
		}
	}

	if !initialCheckOK {
		p.logToBuffer(fmt.Sprintf("\033[31m!!! 启动失败: 3分钟内健康检查未通过 %s\033[0m", checkAddr))
		p.forceKill()
		p.setState(StateFailed)
		return
	}

	const checkInterval = 5 * time.Second
	const toleranceDuration = 20 * time.Minute
	const maxRetries = int(toleranceDuration / checkInterval)

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	failCount := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", checkAddr, 3*time.Second)

			if err != nil {
				p.mutex.Lock()
				state := p.State
				p.mutex.Unlock()

				if state == StateRunning {
					failCount++

					if failCount >= maxRetries {
						p.logToBuffer(fmt.Sprintf("\033[31m!!! 健康检查失败: 连续 %d 次无法连接 (已等待 20 分钟)，判定进程彻底失去响应，执行重启。\033[0m", failCount))
						p.forceKill()
						p.setState(StateFailed)
						return
					} else {
						if failCount == 1 || failCount%12 == 0 {
							p.logToBuffer(fmt.Sprintf("\033[33m! [高负载保护] 健康检查未通过 (%d/%d): 进程可能正在解密大文件，将在 %v 后超时...\033[0m",
								failCount, maxRetries, time.Duration(maxRetries-failCount)*checkInterval))
						}
					}
				}
			} else {
				conn.Close()

				if failCount > 0 {
					p.logToBuffer(fmt.Sprintf("\033[32m--- 进程负载已恢复 (曾连续阻塞 %d 次检测) ---\033[0m", failCount))
					failCount = 0
				}

				p.mutex.Lock()
				state := p.State
				p.mutex.Unlock()

				if state != StateRunning && state != StateStopped {
					p.logToBuffer(fmt.Sprintf("\033[32m--- 健康检查恢复: %s ---\033[0m", checkAddr))
					p.mutex.Lock()
					if p.StartTime.IsZero() {
						p.StartTime = time.Now()
					}
					p.mutex.Unlock()
					p.setState(StateRunning)
				}
			}
		}
	}
}

func (p *ManagedProcess) forceKill() {
	p.mutex.Lock()
	pid := p.PID
	ptmx := p.ptmx
	p.mutex.Unlock()

	if pid > 0 {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
			syscall.Kill(pid, syscall.SIGKILL)
		}
		p.logToBuffer(fmt.Sprintf("\033[31m--- [健康检查] 已强制杀死进程组 %d ---\033[0m", pid))
	}

	if ptmx != nil {
		ptmx.Close()
	}
}

func (m *Manager) saveConfig_internal() {
	configs := make([]ProcessConfig, 0, len(m.Processes))
	for _, p := range m.Processes {
		configs = append(configs, ProcessConfig{
			ID:              p.ID,
			Region:          p.Region,
			Command:         p.Command,
			Args:            p.Args,
			RestartPatterns: p.RestartPatterns,
		})
	}
	data, err := json.MarshalIndent(configs, "", "  ")
	if err != nil {
		log.Printf("!!! [SaveConfig BUG] 序列化 manager.json 失败: %v", err)
		return
	}

	tmpFile := m.ConfigPath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		log.Printf("!!! [SaveConfig] 写入临时配置文件失败: %v", err)
		return
	}

	if err := os.Rename(tmpFile, m.ConfigPath); err != nil {
		log.Printf("!!! [SaveConfig] 重命名配置文件失败: %v", err)
	} else {
		log.Printf("配置已保存到 %s", m.ConfigPath)
	}
}

func (m *Manager) LoadConfig() {
	data, err := os.ReadFile(m.ConfigPath)
	if err != nil {
		log.Printf("未找到 %s，全新启动", m.ConfigPath)
		return
	}
	var configs []ProcessConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		log.Printf("%s 解析失败: %v", m.ConfigPath, err)
		return
	}
	m.mutex.Lock()
	defer m.mutex.Unlock()
	for _, cfg := range configs {
		if cfg.Region == "" {
			cfg.Region = "cn"
		}
		p := NewManagedProcess(cfg.ID, cfg.Region, m.WrapperPath, cfg.Command, cfg.Args, cfg.RestartPatterns)
		m.Processes[cfg.ID] = p
	}
	log.Printf("从 %s 加载了 %d 个进程配置", m.ConfigPath, len(configs))
}

func (m *Manager) ReloadConfig() {
	log.Printf("--- 收到 SIGHUP 或热重载请求，正在重新加载配置 ---")
	newConfigs := make(map[string]ProcessConfig)
	data, err := os.ReadFile(m.ConfigPath)
	if err != nil {
		log.Printf("!!! 重新加载配置失败: %v", err)
		return
	}
	var configs []ProcessConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		log.Printf("!!! 重新加载配置解析失败: %v", err)
		return
	}
	for _, cfg := range configs {
		if cfg.Region == "" {
			cfg.Region = "cn"
		}
		newConfigs[cfg.ID] = cfg
	}
	m.mutex.Lock()
	defer m.mutex.Unlock()
	for id, proc := range m.Processes {
		if _, exists := newConfigs[id]; !exists {
			log.Printf("配置中移除进程 [%s]，正在停止...", id)
			proc.Stop()
			delete(m.Processes, id)
			globalHub.BroadcastProcessRemoved(id)
			m.notifyProxies(ProxyUpdate{Region: proc.Region, Addr: "127.0.0.1:" + proc.ID, IsRemove: true, Type: "tcp"})
			if proc.M3U8Port != "" {
				m.notifyProxies(ProxyUpdate{Region: proc.Region, Addr: "127.0.0.1:" + proc.M3U8Port, IsRemove: true, Type: "http"})
			}
		}
	}
	for id, cfg := range newConfigs {
		if _, exists := m.Processes[id]; !exists {
			log.Printf("配置中新增进程 [%s] Region: %s，正在启动...", id, cfg.Region)
			p := NewManagedProcess(id, cfg.Region, m.WrapperPath, cfg.Command, cfg.Args, cfg.RestartPatterns)
			m.Processes[id] = p
			p.Start()
		}
	}
	m.saveConfig_internal()
	log.Printf("--- 配置热重载完成。当前进程数: %d ---", len(m.Processes))
}

func (m *Manager) AddProcess(id string, region string, command string, args []string) (*ManagedProcess, error) {
	m.mutex.Lock()
	p_exists, exists := m.Processes[id]
	if exists {
		log.Printf("进程 [%s] 已存在，将先停止并替换它。", id)
	}
	p := NewManagedProcess(id, region, m.WrapperPath, command, args, nil)
	m.Processes[id] = p
	m.saveConfig_internal()
	m.mutex.Unlock()
	if exists {
		p_exists.Stop()
	}
	p.Start()
	return p, nil
}

func (m *Manager) RemoveProcess(id string) error {
	m.mutex.Lock()
	p, exists := m.Processes[id]
	if !exists {
		m.mutex.Unlock()
		return fmt.Errorf("未找到 ID 为 %s 的进程", id)
	}
	p.mutex.Lock()
	p.isRemoved = true
	targetRegion := p.Region
	mPort := p.M3U8Port
	p.mutex.Unlock()
	delete(m.Processes, id)
	m.saveConfig_internal()
	m.mutex.Unlock()
	p.Stop()
	globalHub.BroadcastProcessRemoved(id)

	m.notifyProxies(ProxyUpdate{Region: targetRegion, Addr: "127.0.0.1:" + id, IsRemove: true, Type: "tcp"})
	if mPort != "" {
		m.notifyProxies(ProxyUpdate{Region: targetRegion, Addr: "127.0.0.1:" + mPort, IsRemove: true, Type: "http"})
	}

	absWrapperBin, _ := filepath.Abs(m.WrapperPath)
	srcDir := filepath.Dir(absWrapperBin)
	instanceDir := filepath.Join(srcDir, "instances", fmt.Sprintf("%s_%s", targetRegion, id))
	log.Printf("移除进程实例，清理目录: %s", instanceDir)
	os.RemoveAll(instanceDir)
	return nil
}

func (m *Manager) GetProcess(id string) *ManagedProcess {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.Processes[id]
}
func (m *Manager) GetAllProcesses() []*ManagedProcess {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	list := make([]*ManagedProcess, 0, len(m.Processes))
	for _, p := range m.Processes {
		list = append(list, p)
	}
	return list
}

func readCPUSample() (idle, total uint64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) > 4 && fields[0] == "cpu" {
			for i := 1; i < len(fields); i++ {
				val, _ := strconv.ParseUint(fields[i], 10, 64)
				total += val
				if i == 4 {
					idle += val
				}
			}
		}
	}
	return
}

func readMemUsage() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	var total, avail float64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		if parts[0] == "MemTotal:" {
			total, _ = strconv.ParseFloat(parts[1], 64)
		} else if parts[0] == "MemAvailable:" {
			avail, _ = strconv.ParseFloat(parts[1], 64)
		}
	}
	if total > 0 {
		return ((total - avail) / total) * 100
	}
	return 0
}

func readNetStats() (rx, tx uint64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, ":") {
			parts := strings.Fields(line)
			if len(parts) < 10 {
				continue
			}
			if strings.HasPrefix(parts[0], "lo") {
				continue
			}
			cleanParts := strings.Fields(strings.ReplaceAll(line, ":", " "))
			if len(cleanParts) < 10 {
				continue
			}
			r, _ := strconv.ParseUint(cleanParts[1], 10, 64)
			var t uint64
			if len(cleanParts) > 9 {
				t, _ = strconv.ParseUint(cleanParts[9], 10, 64)
			}
			rx += r
			tx += t
		}
	}
	return
}

func monitorSystem() {
	prevIdle, prevTotal := readCPUSample()
	prevRx, prevTx := readNetStats()
	for {
		time.Sleep(1 * time.Second)
		currIdle, currTotal := readCPUSample()
		idleDiff := float64(currIdle - prevIdle)
		totalDiff := float64(currTotal - prevTotal)
		cpuUsage := 0.0
		if totalDiff > 0 {
			cpuUsage = (1.0 - (idleDiff / totalDiff)) * 100.0
		}
		prevIdle, prevTotal = currIdle, currTotal
		memUsage := readMemUsage()
		currRx, currTx := readNetStats()
		downRate := float64(currRx - prevRx)
		upRate := float64(currTx - prevTx)
		prevRx, prevTx = currRx, currTx

		stats := SystemStats{
			OSInfo:      globalManager.ServerOS,
			CPUUsage:    cpuUsage,
			MemUsage:    memUsage,
			NetDownRate: downRate,
			NetUpRate:   upRate,
		}
		globalHub.BroadcastSystemStats(stats)
	}
}

func formatProcessSpeed(bytes uint64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B/s", bytes)
	}
	k := float64(bytes) / 1024
	if k < 1024 {
		return fmt.Sprintf("%.1f KB/s", k)
	}
	m := k / 1024
	return fmt.Sprintf("%.1f MB/s", m)
}

func getPortTrafficMap() map[string]uint64 {
	out, err := exec.Command("ss", "-ntH").Output()
	if err != nil {
		return nil
	}
	stats := make(map[string]uint64)
	lines := strings.Split(string(out), "\n")

	var currentPort string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) >= 4 {
			if strings.Contains(fields[3], ":") {
				_, port, err := net.SplitHostPort(fields[3])
				if err == nil {
					currentPort = port
				}
			}
		}

		if idx := strings.Index(line, "bytes_acked:"); idx != -1 {
			remaining := line[idx+len("bytes_acked:"):]
			remaining = strings.TrimSpace(remaining)
			parts := strings.Fields(remaining)
			if len(parts) > 0 {
				valStr := strings.TrimRight(parts[0], ",")
				val, _ := strconv.ParseUint(valStr, 10, 64)
				if currentPort != "" {
					stats[currentPort] += val
				}
			}
		}
	}
	return stats
}

func monitorNetworkSpeed() {
	_, err := exec.LookPath("ss")
	if err != nil {
		log.Println("警告: 系统未安装 'ss' (iproute2) 命令。如果未使用代理模式(负载均衡)，直连解密速度将显示为0。")
	}

	for {
		time.Sleep(1 * time.Second)
		portStats := getPortTrafficMap()

		globalManager.mutex.RLock()
		procs := make([]*ManagedProcess, 0, len(globalManager.Processes))
		for _, p := range globalManager.Processes {
			procs = append(procs, p)
		}
		globalManager.mutex.RUnlock()

		for _, p := range procs {
			p.mutex.Lock()
			if p.State == StateRunning {
				proxyBytes := atomic.LoadUint64(&p.ProxyBytesSent)
				var ssBytes uint64 = 0

				if portStats != nil {
					ssBytes = portStats[p.ID]
				}

				currBytes := proxyBytes
				if ssBytes > currBytes {
					currBytes = ssBytes
				}

				if p.prevBytes == 0 {
					p.prevBytes = currBytes
					p.NetSpeed = "0 B/s"
				} else {
					diff := uint64(0)
					if currBytes >= p.prevBytes {
						diff = currBytes - p.prevBytes
					} else {
						diff = currBytes
					}
					p.NetSpeed = formatProcessSpeed(diff)
					p.prevBytes = currBytes
				}
				payload := *p
				p.mutex.Unlock()
				globalHub.BroadcastStateUpdate(&payload)
			} else {
				p.prevBytes = 0
				p.NetSpeed = "N/A"
				p.mutex.Unlock()
			}
		}
	}
}

type UnicastReq struct {
	Conn *websocket.Conn
	Msg  []byte
}

type Hub struct {
	clients    map[*websocket.Conn]bool
	broadcast  chan []byte
	unicast    chan UnicastReq
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	mutex      sync.Mutex
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*websocket.Conn]bool),
		broadcast:  make(chan []byte),
		unicast:    make(chan UnicastReq),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
	}
}

func (h *Hub) SendJSON(conn *websocket.Conn, v interface{}) {
	data, err := json.Marshal(v)
	if err == nil {
		h.unicast <- UnicastReq{Conn: conn, Msg: data}
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.mutex.Lock()
			h.clients[client] = true
			h.mutex.Unlock()
		case client := <-h.unregister:
			h.mutex.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				client.Close()
			}
			h.mutex.Unlock()
		case message := <-h.broadcast:
			h.mutex.Lock()
			for client := range h.clients {
				if err := client.WriteMessage(websocket.TextMessage, message); err != nil {
					log.Printf("WebSocket 写入错误: %v", err)
					h.unregister <- client
				}
			}
			h.mutex.Unlock()
		case req := <-h.unicast:
			h.mutex.Lock()
			if _, ok := h.clients[req.Conn]; ok {
				if err := req.Conn.WriteMessage(websocket.TextMessage, req.Msg); err != nil {
					log.Printf("WebSocket 单播错误: %v", err)
					h.unregister <- req.Conn
				}
			}
			h.mutex.Unlock()
		}
	}
}

type WSMessage struct {
	Type    string      `json:"type"`
	ID      string      `json:"id,omitempty"`
	Region  string      `json:"region,omitempty"`
	Command string      `json:"command,omitempty"`
	Args    []string    `json:"args,omitempty"`
	Data    string      `json:"data,omitempty"`
	Payload interface{} `json:"payload,omitempty"`
}

func (h *Hub) BroadcastStateUpdate(p *ManagedProcess) {
	msg := WSMessage{
		Type:    "state_update",
		ID:      p.ID,
		Payload: p,
	}
	data, _ := json.Marshal(msg)
	h.broadcast <- data
}
func (h *Hub) BroadcastLog(id string, line string) {
	msg := WSMessage{
		Type: "log_line",
		ID:   id,
		Data: line,
	}
	data, _ := json.Marshal(msg)
	h.broadcast <- data
}
func (h *Hub) BroadcastProcessRemoved(id string) {
	msg := WSMessage{
		Type: "process_removed",
		ID:   id,
	}
	data, _ := json.Marshal(msg)
	h.broadcast <- data
}
func (h *Hub) BroadcastSystemStats(stats SystemStats) {
	msg := WSMessage{
		Type:    "system_stats",
		Payload: stats,
	}
	data, _ := json.Marshal(msg)
	h.broadcast <- data
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var globalHub *Hub
var globalManager *Manager

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	allProcs := globalManager.GetAllProcesses()
	conn.WriteJSON(WSMessage{Type: "full_status", Payload: allProcs})
	globalHub.register <- conn
	for {
		msgType, message, err := conn.ReadMessage()
		if err != nil {
			globalHub.unregister <- conn
			break
		}
		if msgType != websocket.TextMessage {
			continue
		}
		var msg WSMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("WebSocket JSON 解析失败: %v", err)
			continue
		}
		switch msg.Type {
		case "start_process":
			port := getPortFromArgs(msg.Args, "-D")
			if port == "" {
				globalHub.SendJSON(conn, WSMessage{Type: "log_line", ID: "system", Data: "错误：启动命令中必须包含 -D <port>"})
				continue
			}
			region := msg.Region
			if region == "" {
				region = "cn"
			}
			log.Printf("收到 'start' 命令, ID: %s, Region: %s", port, region)
			globalManager.AddProcess(port, region, msg.Command, msg.Args)
		case "remove_process":
			log.Printf("收到 'remove' 命令, ID: %s", msg.ID)
			globalManager.RemoveProcess(msg.ID)
		case "stdin":
			p := globalManager.GetProcess(msg.ID)
			if p != nil {
				p.Write(msg.Data)
			}
		case "get_logs":
			p := globalManager.GetProcess(msg.ID)
			if p != nil {
				globalHub.SendJSON(conn, WSMessage{Type: "full_log", ID: p.ID, Payload: p.logBuffer})
			}
		}
	}
}

func handleSignals(m *Manager) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP)
	for {
		sig := <-c
		if sig == syscall.SIGHUP {
			m.ReloadConfig()
		}
	}
}

func handleTcpProxy(clientConn net.Conn, p *ManagedProcess) {
	defer clientConn.Close()
	defer atomic.AddInt64(&p.ActiveConn, -1)

	backendAddr := "127.0.0.1:" + p.ID
	backendConn, err := net.DialTimeout("tcp", backendAddr, 5*time.Second)
	if err != nil {
		return
	}
	defer backendConn.Close()
	if tcpConn, ok := clientConn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
		tcpConn.SetReadBuffer(4 * 1024 * 1024)
		tcpConn.SetWriteBuffer(4 * 1024 * 1024)
	}
	if tcpConn, ok := backendConn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
		tcpConn.SetReadBuffer(4 * 1024 * 1024)
		tcpConn.SetWriteBuffer(4 * 1024 * 1024)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		bufPtr := bufferPool.Get().(*[]byte)
		defer bufferPool.Put(bufPtr)
		buf := *bufPtr
		io.CopyBuffer(backendConn, clientConn, buf)
		if c, ok := backendConn.(*net.TCPConn); ok {
			c.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		bufPtr := bufferPool.Get().(*[]byte)
		defer bufferPool.Put(bufPtr)
		buf := *bufPtr
		counter := &WriteCounter{
			Total:  &p.ProxyBytesSent,
			Writer: clientConn,
		}
		io.CopyBuffer(counter, backendConn, buf)
		if c, ok := clientConn.(*net.TCPConn); ok {
			c.CloseWrite()
		}
	}()

	wg.Wait()
}

func selectBackendByIP(region string, clientAddr string, manager *Manager, protocol string) *ManagedProcess {
	var candidates []*ManagedProcess
	manager.mutex.RLock()
	for _, p := range manager.Processes {
		if strings.EqualFold(p.Region, region) && p.ID != "" && p.State == StateRunning {
			candidates = append(candidates, p)
		}
	}
	manager.mutex.RUnlock()

	if len(candidates) == 0 {
		return nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ID < candidates[j].ID
	})

	const CoolDownDuration = 2 * time.Second
	now := time.Now()
	var bestCandidate *ManagedProcess
	
	startIdx := int(atomic.LoadUint64(&globalRoundRobinCounter) % uint64(len(candidates)))
	for i := 0; i < len(candidates); i++ {
		idx := (startIdx + i) % len(candidates)
		p := candidates[idx]
		p.mutex.Lock()
		timeSince := now.Sub(p.LastUseTime)
		p.mutex.Unlock()

		if timeSince < CoolDownDuration {
			continue
		}

		currentConns := atomic.LoadInt64(&p.ActiveConn)
		if bestCandidate == nil || currentConns < atomic.LoadInt64(&bestCandidate.ActiveConn) {
			bestCandidate = p
		}
	}

	if bestCandidate == nil {
		minConns := int64(1<<63 - 1)
		for _, p := range candidates {
			currentConns := atomic.LoadInt64(&p.ActiveConn)
			if currentConns < minConns {
				minConns = currentConns
				bestCandidate = p
			}
		}
	}

	if bestCandidate != nil {
		bestCandidate.mutex.Lock()
		bestCandidate.LastUseTime = time.Now()
		bestCandidate.mutex.Unlock()
		atomic.AddUint64(&globalRoundRobinCounter, 1)
	}

	return bestCandidate
}

func startRegionalTcpProxy(region string, addr string, manager *Manager, ready chan struct{}) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("[TCP-%s] 启动失败: %v", strings.ToUpper(region), err)
		close(ready)
		return
	}
	log.Printf("[TCP-%s] 解密负载均衡已启动 -> %s ( 2秒 冷却时间 + 最小连接数 )", strings.ToUpper(region), addr)
	close(ready)

	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}

		bestTarget := selectBackendByIP(region, c.RemoteAddr().String(), manager, "TCP")

		if bestTarget != nil {
			atomic.AddInt64(&bestTarget.ActiveConn, 1)
			go handleTcpProxy(c, bestTarget)
		} else {
			c.Close()
		}
	}
}

func handleRawM3U8(region string, clientConn net.Conn, manager *Manager) {
	defer clientConn.Close()

	clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	requestData, err := io.ReadAll(clientConn)
	if err != nil && len(requestData) == 0 {
		return
	}

	target := selectBackendByIP(region, clientConn.RemoteAddr().String(), manager, "RAW")
	if target == nil {
		return
	}

	backendAddr := "127.0.0.1:" + target.M3U8Port
	backendConn, err := net.DialTimeout("tcp", backendAddr, 5*time.Second)
	if err != nil {
		return
	}
	defer backendConn.Close()

	_, err = backendConn.Write(requestData)
	if err != nil {
		return
	}

	backendConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	responseData, err := io.ReadAll(backendConn)
	if err != nil && len(responseData) == 0 {
		return
	}

	originalHost := clientConn.LocalAddr().String()

	bodyString := string(responseData)
	re := regexp.MustCompile(`(https?://)[^/:]+:` + target.M3U8Port)

	newString := re.ReplaceAllString(bodyString, "${1}"+originalHost)

	if len(newString) != len(bodyString) {
		responseData = []byte(newString)
	}

	clientConn.Write(responseData)
}

func startRegionalHttpProxy(region string, addr string, manager *Manager, ready chan struct{}) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("[HTTP-%s] 启动失败: %v", strings.ToUpper(region), err)
		close(ready)
		return
	}

	httpListener := NewVirtualListener(l.Addr())

	director := func(req *http.Request) {
		target := selectBackendByIP(region, req.RemoteAddr, manager, "HTTP")
		if target == nil {
			return
		}
		targetStr := "127.0.0.1:" + target.M3U8Port
		targetUrl, _ := url.Parse("http://" + targetStr)

		originalHost := req.Host
		if originalHost == "" {
			originalHost = req.URL.Host
		}
		req.Header.Set("X-Original-Host", originalHost)
		req.Header.Del("Accept-Encoding")

		req.URL.Scheme = "http"
		req.URL.Host = targetUrl.Host
		req.Host = targetUrl.Host
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
	}

	modifyResponse := func(res *http.Response) error {
		originalHost := res.Request.Header.Get("X-Original-Host")
		if originalHost == "" {
			return nil
		}
		bodyBytes, _ := io.ReadAll(res.Body)
		res.Body.Close()

		checkLen := 512
		if len(bodyBytes) < checkLen {
			checkLen = len(bodyBytes)
		}
		if strings.Contains(string(bodyBytes[:checkLen]), "#EXTM3U") {
			bodyString := string(bodyBytes)
			_, port, _ := net.SplitHostPort(res.Request.Host)
			if port == "" {
				_, port, _ = net.SplitHostPort(res.Request.URL.Host)
			}

			if port != "" {
				re := regexp.MustCompile(`(https?://)[^/:]+:` + port)
				newString := re.ReplaceAllString(bodyString, "${1}"+originalHost)
				if len(newString) != len(bodyString) {
					bodyBytes = []byte(newString)
				}
			}
		}
		buf := bytes.NewBuffer(bodyBytes)
		res.Body = io.NopCloser(buf)
		res.ContentLength = int64(buf.Len())
		res.Header.Set("Content-Length", strconv.Itoa(buf.Len()))
		res.Header.Del("Transfer-Encoding")
		return nil
	}

	proxy := &httputil.ReverseProxy{Director: director, ModifyResponse: modifyResponse}

	httpServer := &http.Server{Handler: proxy}
	go httpServer.Serve(httpListener)

	log.Printf("[HTTP-%s] 双模负载均衡已启动 -> %s ( HTTP + Raw兼容模式 )", strings.ToUpper(region), addr)
	close(ready)

	for {
		clientConn, err := l.Accept()
		if err != nil {
			continue
		}

		go func(c net.Conn) {
			buf := make([]byte, 4)
			c.SetReadDeadline(time.Now().Add(3 * time.Second))
			n, err := c.Read(buf)
			c.SetReadDeadline(time.Time{})

			if err != nil && err != io.EOF {
				c.Close()
				return
			}

			peekedConn := &PeekedConn{Conn: c, peeked: buf[:n]}
			head := string(buf[:n])

			if strings.HasPrefix(head, "GET") || strings.HasPrefix(head, "POS") || strings.HasPrefix(head, "HEA") || strings.HasPrefix(head, "CON") {
				httpListener.conns <- peekedConn
			} else {
				handleRawM3U8(region, peekedConn, manager)
			}
		}(clientConn)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {
	configPath := flag.String("config", "config.yaml", "")
	flag.Parse()

	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}

	var rawMap map[string]yaml.Node
	if err := yaml.Unmarshal(data, &rawMap); err != nil {
		log.Fatalf("Error parsing config file: %v", err)
	}

	var serverCfg ServerConfig
	if node, ok := rawMap["server"]; ok {
		node.Decode(&serverCfg)
		delete(rawMap, "server")
	}
	if serverCfg.WebListen == "" {
		serverCfg.WebListen = "0.0.0.0:8080"
	}

	var wrapperCfg WrapperConfig
	if node, ok := rawMap["wrapper"]; ok {
		node.Decode(&wrapperCfg)
		delete(rawMap, "wrapper")
	}
	if wrapperCfg.Path == "" {
		wrapperCfg.Path = "./wrapper"
	}
	if wrapperCfg.StateFile == "" {
		wrapperCfg.StateFile = "manager.json"
	}

	if len(wrapperCfg.RestartPatterns) > 0 {
		GlobalRestartPatterns = wrapperCfg.RestartPatterns
	}

	if _, err := os.Stat(wrapperCfg.Path); os.IsNotExist(err) {
		log.Fatalf("Error: Wrapper binary not found at %s", wrapperCfg.Path)
	}

	globalHub = newHub()
	globalManager = NewManager(wrapperCfg.Path, wrapperCfg.StateFile)

	globalManager.mutex.RLock()
	for _, p := range globalManager.Processes {
		p.Start()
	}
	globalManager.mutex.RUnlock()

	go globalHub.run()
	go monitorSystem()
	go monitorNetworkSpeed()
	go handleSignals(globalManager)

	for regionName, node := range rawMap {
		var rCfg RegionConfig
		if err := node.Decode(&rCfg); err == nil {
			if rCfg.DecryptPort != "" {
				ready := make(chan struct{})
				go startRegionalTcpProxy(regionName, rCfg.DecryptPort, globalManager, ready)
				<-ready
			}
			if rCfg.GetPort != "" {
				ready := make(chan struct{})
				go startRegionalHttpProxy(regionName, rCfg.GetPort, globalManager, ready)
				<-ready
			}
			fmt.Println()
		}
	}

	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		log.Println("Stopping...")
		all := globalManager.GetAllProcesses()
		for _, p := range all {
			p.Stop()
		}
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()

	log.Printf("WebUI: http://%s", serverCfg.WebListen)
	if err := http.ListenAndServe(serverCfg.WebListen, nil); err != nil {
		log.Fatal(err)
	}
}
