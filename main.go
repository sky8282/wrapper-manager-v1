package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

const MaxLogLines = 200

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

type ManagedProcess struct {
	ID          string       `json:"id"`
	Region      string       `json:"region"`
	Command     string       `json:"command"`
	Args        []string     `json:"args"`
	WrapperPath string       `json:"-"`
	State       ProcessState `json:"state"`
	PID         int          `json:"pid"`
	StartTime   time.Time    `json:"startTime"`
	Speed       string       `json:"speed,omitempty"`
	NetSpeed    string       `json:"netSpeed"`
	prevBytes   uint64
	isRemoved   bool
	ctx         context.Context
	cancel      context.CancelFunc
	cmd         *exec.Cmd
	ptmx        io.ReadWriteCloser
	mutex       sync.Mutex
	logBuffer   []string
}

type SystemStats struct {
	CPUUsage    float64 `json:"cpu"`
	MemUsage    float64 `json:"mem"`
	NetDownRate float64 `json:"net_down"`
	NetUpRate   float64 `json:"net_up"`
}

func NewManagedProcess(id string, region string, wrapperPath string, command string, args []string) *ManagedProcess {
	ctx, cancel := context.WithCancel(context.Background())
	if region == "" {
		region = "cn"
	}
	return &ManagedProcess{
		ID:          id,
		Region:      region,
		Command:     command,
		Args:        args,
		WrapperPath: wrapperPath,
		State:       StateStopped,
		ctx:         ctx,
		cancel:      cancel,
		logBuffer:   make([]string, 0, MaxLogLines),
		StartTime:   time.Time{},
		Speed:       "N/A",
		NetSpeed:    "N/A",
		prevBytes:   0,
		isRemoved:   false,
	}
}

func (p *ManagedProcess) Start() {
	go p.runLoop()
}

func (p *ManagedProcess) Stop() {
	p.logToBuffer("--- 收到停止命令 ---")
	p.cancel()
	p.mutex.Lock()
	pidToKill := 0
	if p.cmd != nil && p.cmd.Process != nil {
		pidToKill = p.cmd.Process.Pid
	}
	p.mutex.Unlock()

	if pidToKill != 0 {
		if err := syscall.Kill(-pidToKill, syscall.SIGKILL); err != nil {
			p.logToBuffer(fmt.Sprintf("!!! 警告: 无法杀死进程组 %d: %v", pidToKill, err))
		} else {
			p.logToBuffer(fmt.Sprintf("--- 进程组 %d 已终止 (SIGKILL) ---", pidToKill))
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

	p.State = newState
	log.Printf("进程 [%s] 状态变为: %s", p.ID, p.State)
	if newState == StateStopped || newState == StateFailed {
		p.PID = 0
		p.StartTime = time.Time{}
		p.Speed = "N/A"
		p.NetSpeed = "N/A"
		p.prevBytes = 0
	}

	payload := *p

	p.mutex.Unlock()

	globalHub.BroadcastStateUpdate(&payload)
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

func setupInstance(region string, wrapperPath string) (string, string, error) {
	absWrapperBin, err := filepath.Abs(wrapperPath)
	if err != nil {
		return "", "", err
	}
	srcDir := filepath.Dir(absWrapperBin)
	binName := filepath.Base(absWrapperBin)

	instanceRoot := filepath.Join(srcDir, "instances")
	instanceDir := filepath.Join(instanceRoot, region)

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

				if rName == "dev" || rName == "proc" || rName == "sys" {
					if err := os.MkdirAll(rDstPath, 0755); err != nil {
						log.Printf("创建系统目录失败: %v", err)
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
	p.logToBuffer(fmt.Sprintf("--- 正在启动: %s %s (Region: %s) ---", p.WrapperPath, strings.Join(p.Args, " "), p.Region))

	instanceDir, binCommand, err := setupInstance(p.Region, p.WrapperPath)
	if err != nil {
		p.logToBuffer(fmt.Sprintf("!!! 实例环境创建失败: %v", err))
		p.setState(StateFailed)
		return
	}
	p.logToBuffer(fmt.Sprintf("--- 实例环境已就绪: %s ---", instanceDir))

	p.mutex.Lock()
	p.cmd = exec.CommandContext(p.ctx, binCommand, p.Args...)
	p.cmd.Dir = instanceDir
	p.mutex.Unlock()

	ptmx, err := pty.Start(p.cmd)
	if err != nil {
		p.logToBuffer(fmt.Sprintf("!!! PTY 启动失败: %v", err))
		p.setState(StateFailed)
		return
	}
	p.ptmx = ptmx
	defer p.ptmx.Close()

	p.mutex.Lock()
	if p.cmd != nil && p.cmd.Process != nil {
		p.PID = p.cmd.Process.Pid
	} else {
		p.PID = 0
	}
	payload := *p
	p.mutex.Unlock()
	globalHub.BroadcastStateUpdate(&payload)

	checkPort := p.getCheckPort()
	if checkPort == "" {
		p.logToBuffer("!!! 警告: 无法从参数中找到 -D 端口，健康检查已禁用")
		p.setState(StateRunning)
	} else {
		go p.healthCheck(checkPort)
	}

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				line := string(buf[:n])
				lines := strings.Split(line, "\n")
				for _, l := range lines {
					if strings.TrimSpace(l) == "" {
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
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				if p.ctx.Err() != nil {
					break
				}
				p.logToBuffer(fmt.Sprintf("!!! PTY 读取错误: %v", err))
				break
			}
		}
	}()

	p.cmd.Wait()
	p.ptmx = nil

	if p.ctx.Err() == nil {
		p.logToBuffer("--- 进程意外退出 ---")
		p.setState(StateFailed)
		p.logToBuffer("--- 2秒后将自动重启 ---")

		time.Sleep(2 * time.Second)
		globalManager.mutex.RLock()
		_, exists := globalManager.Processes[p.ID]
		globalManager.mutex.RUnlock()

		if exists {
			p.logToBuffer("--- 正在重启... ---")
			p.cancel()
			p.ctx, p.cancel = context.WithCancel(context.Background())
			p.Start()
		} else {
			p.logToBuffer("--- 进程已被移除，取消重启 ---")
		}

	} else {
		p.logToBuffer("--- 进程已停止 ---")
		p.setState(StateStopped)
	}
}

func (p *ManagedProcess) getCheckPort() string {
	return p.ID
}

func (p *ManagedProcess) healthCheck(port string) {
	checkAddr := "127.0.0.1:" + port

	initialCheckOK := false
	for i := 0; i < 10; i++ {
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(3 * time.Second):
			conn, err := net.DialTimeout("tcp", checkAddr, 2*time.Second)
			if err == nil {
				conn.Close()
				p.logToBuffer(fmt.Sprintf("--- 健康检查通过: %s ---", checkAddr))

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
		p.logToBuffer(fmt.Sprintf("!!! 启动失败: 30秒内健康检查未通过 %s", checkAddr))
		p.setState(StateFailed)
		return
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", checkAddr, 2*time.Second)
			if err != nil {
				p.mutex.Lock()
				state := p.State
				p.mutex.Unlock()

				if state == StateRunning {
					p.logToBuffer(fmt.Sprintf("!!! 健康检查失败: 无法连接到 %s", checkAddr))
					p.setState(StateFailed)
				}
			} else {
				conn.Close()
				p.mutex.Lock()
				state := p.State
				p.mutex.Unlock()

				if state != StateRunning && state != StateStopped {
					p.logToBuffer(fmt.Sprintf("--- 健康检查恢复: %s ---", checkAddr))
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

type Manager struct {
	WrapperPath string
	Processes   map[string]*ManagedProcess
	mutex       sync.RWMutex
	ConfigPath  string
}

func NewManager(wrapperPath string, configPath string) *Manager {
	m := &Manager{
		WrapperPath: wrapperPath,
		Processes:   make(map[string]*ManagedProcess),
		ConfigPath:  configPath,
	}
	m.LoadConfig()
	return m
}

type ProcessConfig struct {
	ID      string   `json:"id"`
	Region  string   `json:"region"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func (m *Manager) saveConfig_internal() {
	configs := make([]ProcessConfig, 0, len(m.Processes))
	for _, p := range m.Processes {
		configs = append(configs, ProcessConfig{
			ID:      p.ID,
			Region:  p.Region,
			Command: p.Command,
			Args:    p.Args,
		})
	}

	data, err := json.MarshalIndent(configs, "", "  ")
	if err != nil {
		log.Printf("!!! [SaveConfig BUG] 序列化 manager.json 失败: %v", err)
		return
	}

	if err := os.WriteFile(m.ConfigPath, data, 0644); err != nil {
		log.Printf("!!! [SaveConfig BUG] 写入 manager.json 失败: %v", err)
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
		p := NewManagedProcess(cfg.ID, cfg.Region, m.WrapperPath, cfg.Command, cfg.Args)
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
		if cfg.Region == "" { cfg.Region = "cn" }
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
		}
	}

	for id, cfg := range newConfigs {
		if _, exists := m.Processes[id]; !exists {
			log.Printf("配置中新增进程 [%s] Region: %s，正在启动...", id, cfg.Region)
			p := NewManagedProcess(id, cfg.Region, m.WrapperPath, cfg.Command, cfg.Args)
			m.Processes[id] = p
			p.Start()
		} else {
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

	p := NewManagedProcess(id, region, m.WrapperPath, command, args)
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
	p.mutex.Unlock()

	delete(m.Processes, id)
	m.saveConfig_internal()
	
	hasSiblings := false
	for _, otherP := range m.Processes {
		if otherP.Region == targetRegion {
			hasSiblings = true
			break
		}
	}
	m.mutex.Unlock()

	p.Stop()
	globalHub.BroadcastProcessRemoved(id)

	absWrapperBin, _ := filepath.Abs(m.WrapperPath)
	srcDir := filepath.Dir(absWrapperBin)
	instanceDir := filepath.Join(srcDir, "instances", targetRegion)
	
	if !hasSiblings {
		log.Printf("区域 [%s] 已无运行实例，清理目录: %s", targetRegion, instanceDir)
		os.RemoveAll(instanceDir)
	} else {
		log.Printf("区域 [%s] 仍有实例运行，保留目录: %s", targetRegion, instanceDir)
	}

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
			t, _ := strconv.ParseUint(cleanParts[9], 10, 64)
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
	out, err := exec.Command("ss", "-nitH").Output()
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

		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				_, port, err := net.SplitHostPort(fields[3])
				if err == nil {
					currentPort = port
				}
			}
		}

		if strings.Contains(line, "bytes_acked:") {
			parts := strings.Fields(line)
			for _, part := range parts {
				if strings.HasPrefix(part, "bytes_acked:") {
					valStr := strings.TrimPrefix(part, "bytes_acked:")
					val, _ := strconv.ParseUint(valStr, 10, 64)
					if currentPort != "" {
						stats[currentPort] += val
					}
				}
			}
		}
	}
	return stats
}

func monitorNetworkSpeed() {
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
				currBytes := portStats[p.ID]

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

type Hub struct {
	clients    map[*websocket.Conn]bool
	broadcast  chan []byte
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	mutex      sync.Mutex
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*websocket.Conn]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
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

func extractPortFromArgs(args []string) string {
	for i, arg := range args {
		if arg == "-D" && i+1 < len(args) {
			_, port, err := net.SplitHostPort(args[i+1])
			if err == nil {
				return port
			}
			return args[i+1]
		}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-D") {
			return strings.TrimPrefix(arg, "-D")
		}
	}
	return ""
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	globalHub.register <- conn

	allProcs := globalManager.GetAllProcesses()
	conn.WriteJSON(WSMessage{Type: "full_status", Payload: allProcs})

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
			port := extractPortFromArgs(msg.Args)
			if port == "" {
				conn.WriteJSON(WSMessage{Type: "log_line", ID: "system", Data: "错误：启动命令中必须包含 -D <port>"})
				continue
			}
			region := msg.Region
			if region == "" { region = "cn" }

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
				conn.WriteJSON(WSMessage{Type: "full_log", ID: p.ID, Payload: p.logBuffer})
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

func main() {
	wrapperBin := flag.String("wrapper-bin", "./wrapper", "zhaarey/wrapper 可执行文件的路径")
	port := flag.String("port", "8080", "此管理器 Web UI 的监听端口")
	configJson := flag.String("config", "manager.json", "用于保存进程列表的 JSON 配置文件")
	flag.Parse()

	if _, err := os.Stat(*wrapperBin); os.IsNotExist(err) {
		log.Fatalf("错误: wrapper 可执行文件未找到: %s", *wrapperBin)
	}

	globalHub = newHub()
	globalManager = NewManager(*wrapperBin, *configJson)
	globalManager.mutex.RLock()
	for _, p := range globalManager.Processes {
		p.Start()
	}
	globalManager.mutex.RUnlock()

	go globalHub.run()
	go monitorSystem()
	go monitorNetworkSpeed()
	go handleSignals(globalManager)

	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		log.Println("收到关闭信号，正在停止所有子进程...")

		allProcs := globalManager.GetAllProcesses()

		for _, p := range allProcs {
			log.Printf("正在停止 [%s]...", p.ID)
			p.Stop()
		}

		log.Println("等待 2 秒以完成清理...")
		time.Sleep(2 * time.Second)

		log.Println("所有进程已停止。退出。")
		os.Exit(0)
	}()

	log.Printf("Wrapper 路径: %s", *wrapperBin)
	log.Printf("配置 JSON: %s", *configJson)
	log.Printf("管理器 Web UI 将在 http://0.0.0.0:%s 上运行", *port)
	if err := http.ListenAndServe(":"+*port, nil); err != nil {
		log.Fatal(err)
	}
}
