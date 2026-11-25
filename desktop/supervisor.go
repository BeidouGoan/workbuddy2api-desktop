package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 服务状态。字符串直接透给前端展示。
const (
	stateStopped    = "stopped"
	stateStarting   = "starting"
	stateRunning    = "running"
	stateRestarting = "restarting"
	stateFailed     = "failed"
)

const (
	maxConsecutiveRestarts = 5
	healthInterval         = 3 * time.Second
	readyTimeout           = 45 * time.Second
)

// AccountCounts 账号池概览（来自 /panel/api/overview）。
type AccountCounts struct {
	Total    int `json:"total"`
	Healthy  int `json:"healthy"`
	Cooling  int `json:"cooling"`
	Disabled int `json:"disabled"`
}

// Snapshot 是给界面与托盘读的只读状态快照。
type Snapshot struct {
	State        string        `json:"state"`
	Port         int           `json:"port"`
	PortNote     string        `json:"port_note"`
	BaseURL      string        `json:"base_url"`
	PanelURL     string        `json:"panel_url"`
	APIKey       string        `json:"api_key"`
	Uptime       int64         `json:"uptime_sec"`
	Pid          int           `json:"pid"`
	Healthy      bool          `json:"healthy"`
	External     bool          `json:"external"`
	LastError    string        `json:"last_error"`
	Restarts     int           `json:"restarts"`
	Accounts     AccountCounts `json:"accounts"`
	Version      string        `json:"version"`
	Autostart    bool          `json:"autostart"`
	DataDir      string        `json:"data_dir"`
	ConfigPath   string        `json:"config_path"`
	ServerLog    string        `json:"server_log"`
	DesktopLog   string        `json:"desktop_log"`
}

type svcCmd int

const (
	cmdStart svcCmd = iota
	cmdStop
	cmdRestart
)

// Supervisor 守护 workbuddy2api 服务进程：启动、健康探测、崩溃退避重启。
//
// 所有状态变更都收敛在一个 goroutine（Run）里串行处理，避免多线程下
// 「刚判定退出又开始重启」这类竞态。
type Supervisor struct {
	dataDir    string
	exePath    string
	configPath string

	// 端口：configuredPort 来自 config.json，activePort 是本次实际使用的端口
	// （配置端口被其他程序占用时会顺延到备用端口）。
	listenRaw      string
	configuredPort int
	activePort     int
	portNote       string

	cmds   chan svcCmd
	exited chan error
	quit   chan struct{}
	done   chan struct{}

	mu     sync.RWMutex
	snap   Snapshot
	notify func(title, text string)

	cmd        *exec.Cmd
	stopping   bool
	startedAt  time.Time
	restarts   int
	everReady  bool
	client     *http.Client
	logFile    *os.File
	onChangeMu sync.Mutex
	onChange   func()
}

// NewSupervisor 构造守护器；onChange 在状态变化时回调（用于刷新托盘提示与界面）。
func NewSupervisor(dataDir, exePath, configPath string) *Supervisor {
	base := &Supervisor{
		dataDir:        dataDir,
		exePath:        exePath,
		configPath:     configPath,
		listenRaw:      "127.0.0.1:7863",
		configuredPort: 7863,
		activePort:     7863,
		cmds:           make(chan svcCmd, 8),
		exited:     make(chan error, 4),
		quit:       make(chan struct{}),
		done:       make(chan struct{}),
		client:     &http.Client{Timeout: 4 * time.Second},
		snap: Snapshot{
			State:      stateStopped,
			Port:       7863,
			DataDir:    dataDir,
			ConfigPath: configPath,
			ServerLog:  filepath.Join(dataDir, "logs", "server-console.log"),
			DesktopLog: filepath.Join(dataDir, "logs", "desktop.log"),
		},
	}
	base.refreshConfig()
	return base
}

// SetNotifier 设置气泡通知回调（托盘不可用时退化为 no-op）。
func (s *Supervisor) SetNotifier(fn func(title, text string)) { s.notify = fn }

// SetOnChange 设置状态变化回调。
func (s *Supervisor) SetOnChange(fn func()) { s.onChange = fn }

// Start 启动守护循环并发起首次启动。
func (s *Supervisor) Start() {
	go s.run()
	s.cmds <- cmdStart
}

// Stop 主动停止服务并结束守护循环（退出程序时调用）。
func (s *Supervisor) Stop() {
	select {
	case <-s.done:
		return
	default:
	}
	s.cmds <- cmdStop
	close(s.quit)
}

// Restart 重启服务（面板改了 listen 等需重启的配置时用）。
func (s *Supervisor) Restart() { s.cmds <- cmdRestart }

// StartService 手动启动服务。
func (s *Supervisor) StartService() { s.cmds <- cmdStart }

// StopService 手动停止服务（保留守护循环，可再次启动）。
func (s *Supervisor) StopService() { s.cmds <- cmdStop }

// Snapshot 返回当前状态快照。
func (s *Supervisor) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := s.snap
	snap.Autostart = isAutostartEnabled()
	if snap.State == stateRunning && !s.startedAt.IsZero() {
		snap.Uptime = int64(time.Since(s.startedAt).Seconds())
	}
	return snap
}

// run 守护主循环。
func (s *Supervisor) run() {
	defer close(s.done)
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.quit:
			s.shutdownChild()
			return
		case c := <-s.cmds:
			switch c {
			case cmdStart:
				s.stopping = false
				s.restarts = 0
				s.launch()
			case cmdRestart:
				s.stopping = false
				s.restarts = 0
				s.killChild()
				s.launch()
			case cmdStop:
				s.stopping = true
				s.killChild()
				s.setState(stateStopped, "")
			}
		case err := <-s.exited:
			s.handleExit(err)
		case <-ticker.C:
			s.tick()
		}
	}
}

// refreshConfig 从 config.json 读取 listen / api_key（首次运行由 server 生成）。
func (s *Supervisor) refreshConfig() {
	raw, err := os.ReadFile(s.configPath)
	if err != nil {
		return
	}
	var cfg struct {
		Listen string `json:"listen"`
		APIKey string `json:"api_key"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return
	}
	_, port := splitListen(cfg.Listen)
	s.mu.Lock()
	s.listenRaw = strings.TrimSpace(cfg.Listen)
	if port != s.configuredPort {
		// 面板里改了 listen：以新配置为准，重置上次的端口冲突提示。
		s.configuredPort = port
		s.activePort = port
		s.portNote = ""
	}
	s.snap.APIKey = cfg.APIKey
	s.applyAddrLocked()
	s.mu.Unlock()
}

// applyAddrLocked 用当前生效端口刷新展示地址（调用方须持锁）。
func (s *Supervisor) applyAddrLocked() {
	host, _ := splitListen(s.listenRaw)
	port := s.activePort
	if port == 0 {
		port = s.configuredPort
	}
	s.snap.Port = port
	s.snap.PortNote = s.portNote
	s.snap.BaseURL = fmt.Sprintf("http://%s:%d/v1", host, port)
	s.snap.PanelURL = fmt.Sprintf("http://%s:%d/panel/", host, port)
}

// bindHost 返回配置文件里的监听主机（保留 0.0.0.0 等原始绑定意图）。
func (s *Supervisor) bindHost() string {
	listen := strings.TrimSpace(s.listenRaw)
	if listen == "" {
		return "127.0.0.1"
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return "127.0.0.1"
	}
	if host == "" {
		return "0.0.0.0"
	}
	return strings.Trim(host, "[]")
}

// pickPort 返回本次实际监听端口。配置端口空闲时原样使用；被其他程序占用时
// 向后寻找备用端口，并返回给用户看的说明文案。
func (s *Supervisor) pickPort() (int, string, error) {
	host := s.bindHost()
	if canListen(host, s.configuredPort) {
		return s.configuredPort, "", nil
	}
	for port := s.configuredPort + 1; port <= s.configuredPort+20 && port <= 65535; port++ {
		if canListen(host, port) {
			return port, fmt.Sprintf(
				"端口 %d 已被其他程序占用，本次运行改用 %d。如需固定端口，请释放 %d 或在面板中修改 listen 配置。",
				s.configuredPort, port, s.configuredPort), nil
		}
	}
	return 0, "", fmt.Errorf("%d~%d 端口均被占用，服务无法启动；请关闭占用端口的程序后点击「启动服务」",
		s.configuredPort, s.configuredPort+20)
}

// canListen 判断地址是否可绑定（能绑定即可启动监听）。
func canListen(host string, port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// splitListen 把 listen 配置解析成可访问的 host + port。
// 监听全零地址时用回环访问（本机面板与健康探测都走回环）。
func splitListen(listen string) (string, int) {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return "127.0.0.1", 7863
	}
	host, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		// 只有端口（":7863"）或裸端口（"7863"）
		portStr = strings.TrimPrefix(listen, ":")
		if p, e := strconv.Atoi(portStr); e == nil {
			return "127.0.0.1", p
		}
		return "127.0.0.1", 7863
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		port = 7863
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return host, port
}

func (s *Supervisor) setState(state, errText string) {
	s.mu.Lock()
	prev := s.snap.State
	s.snap.State = state
	s.snap.LastError = errText
	if state == stateRunning {
		if prev != stateRunning {
			s.startedAt = time.Now()
		}
	} else {
		s.startedAt = time.Time{}
	}
	s.mu.Unlock()
	if prev != state {
		s.fireChange()
	}
}

func (s *Supervisor) fireChange() {
	if s.onChange == nil {
		return
	}
	s.onChangeMu.Lock()
	fn := s.onChange
	s.onChangeMu.Unlock()
	if fn != nil {
		fn()
	}
}

// launch 启动子进程（若端口上已有健康服务则直接接管，不重复启动）。
func (s *Supervisor) launch() {
	s.refreshConfig()
	if ok, version := s.probe(s.configuredPort); ok {
		s.mu.Lock()
		s.snap.External = true
		s.snap.Healthy = true
		s.snap.Version = version
		s.startedAt = time.Now()
		s.activePort = s.configuredPort
		s.portNote = ""
		s.applyAddrLocked()
		s.mu.Unlock()
		s.setState(stateRunning, "")
		s.pullOverview()
		return
	}

	// 配置端口被其他程序占用时顺延到备用端口，避免服务起不来却没有任何反馈。
	port, note, err := s.pickPort()
	if err != nil {
		s.setState(stateFailed, err.Error())
		appendLog(s.dataDir, "desktop.log", "端口不可用: "+err.Error())
		s.bubble("WorkBuddy2API 启动失败", err.Error())
		return
	}
	s.mu.Lock()
	s.activePort = port
	s.portNote = note
	s.applyAddrLocked()
	s.mu.Unlock()
	if note != "" {
		appendLog(s.dataDir, "desktop.log", note)
		s.bubble("WorkBuddy2API 端口已切换", note)
	}

	s.setState(stateStarting, "")

	logFile, err := os.OpenFile(filepath.Join(s.dataDir, "logs", "server-console.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logFile = nil
	}
	s.logFile = logFile

	cmd := exec.Command(s.exePath, "-config", s.configPath)
	cmd.Dir = s.dataDir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	env := append(os.Environ(), "WB2API_DESKTOP=1")
	if port != s.configuredPort {
		// 用环境变量覆盖 listen，不改动用户的 config.json。
		env = append(env, "WB2A_LISTEN="+net.JoinHostPort(s.bindHost(), strconv.Itoa(port)))
	}
	cmd.Env = env
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		s.mu.Lock()
		s.snap.External = false
		s.mu.Unlock()
		s.setState(stateFailed, "启动失败: "+err.Error())
		s.bubble("WorkBuddy2API 启动失败", err.Error())
		return
	}
	s.cmd = cmd
	s.mu.Lock()
	s.snap.Pid = cmd.Process.Pid
	s.snap.External = false
	s.mu.Unlock()
	appendLog(s.dataDir, "desktop.log", fmt.Sprintf("%s 启动服务 pid=%d", time.Now().Format("15:04:05"), cmd.Process.Pid))

	go func(c *exec.Cmd) {
		s.exited <- c.Wait()
	}(cmd)

	// 等首轮健康：成功即进入 running，超时不算失败（交给 tick 继续探测）。
	go s.waitReady(cmd)
}

func (s *Supervisor) waitReady(cmd *exec.Cmd) {
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-s.quit:
			return
		default:
		}
		snap := s.Snapshot()
		if ok, version := s.probe(snap.Port); ok {
			s.mu.Lock()
			if s.cmd == cmd {
				s.snap.Healthy = true
				s.snap.Version = version
				s.everReady = true
				s.restarts = 0
			}
			s.mu.Unlock()
			s.refreshConfig()
			s.setState(stateRunning, "")
			s.pullOverview()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// handleExit 子进程退出后的处置：主动停止 / 外部接管 / 崩溃重启。
func (s *Supervisor) handleExit(err error) {
	s.cmd = nil
	if s.logFile != nil {
		_ = s.logFile.Close()
		s.logFile = nil
	}
	if s.stopping {
		return
	}
	snap := s.Snapshot()
	// 端口上已有健康服务（例如自动更新拉起的新进程）：直接接管，不再重启。
	if ok, version := s.probe(snap.Port); ok {
		s.mu.Lock()
		s.snap.External = true
		s.snap.Healthy = true
		s.snap.Version = version
		s.snap.Pid = 0
		s.startedAt = time.Now()
		s.mu.Unlock()
		s.setState(stateRunning, "")
		return
	}

	reason := "服务已退出"
	if err != nil {
		reason = err.Error()
	}
	appendLog(s.dataDir, "desktop.log", fmt.Sprintf("%s 服务退出: %v", time.Now().Format("15:04:05"), err))

	s.restarts++
	if s.restarts > maxConsecutiveRestarts {
		s.setState(stateFailed, "连续崩溃 "+strconv.Itoa(maxConsecutiveRestarts)+" 次，已停止自动重启")
		s.bubble("WorkBuddy2API 已停止", "服务连续崩溃，请打开 Web 面板查看日志")
		return
	}
	delay := backoffDelay(s.restarts)
	s.setState(stateRestarting, reason)
	s.bubble("WorkBuddy2API 正在重启", fmt.Sprintf("第 %d 次自动重启（%s 后）", s.restarts, delay))
	time.AfterFunc(delay, func() {
		select {
		case <-s.quit:
			return
		default:
			s.cmds <- cmdStart
		}
	})
}

// backoffDelay 崩溃重启退避：1s / 2s / 5s / 10s，之后固定 10s。
func backoffDelay(n int) time.Duration {
	switch {
	case n <= 1:
		return time.Second
	case n == 2:
		return 2 * time.Second
	case n == 3:
		return 5 * time.Second
	default:
		return 10 * time.Second
	}
}

func (s *Supervisor) killChild() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	s.stopping = true
	_ = s.cmd.Process.Kill()
	// 等待 exited 通道把句柄收掉；超时兜底。
	deadline := time.Now().Add(3 * time.Second)
	for s.cmd != nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	s.stopping = false
}

func (s *Supervisor) shutdownChild() {
	s.stopping = true
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	if s.logFile != nil {
		_ = s.logFile.Close()
	}
}

// tick 周期探测健康与账号概览。
func (s *Supervisor) tick() {
	s.refreshConfig()
	snap := s.Snapshot()
	ok, version := s.probe(snap.Port)
	s.mu.Lock()
	changed := s.snap.Healthy != ok
	s.snap.Healthy = ok
	if ok && version != "" {
		s.snap.Version = version
	}
	if ok && s.snap.State != stateRunning {
		s.snap.State = stateRunning
		s.startedAt = time.Now()
		changed = true
	} else if !ok && s.snap.State == stateRunning && s.cmd != nil {
		// 进程还在但探活失败：保持 running，等待退出或恢复（避免误报）。
	}
	s.mu.Unlock()
	if changed {
		s.fireChange()
	}
	if ok {
		s.pullOverview()
	}
}

// probe 探测 /healthz，返回服务版本标识。
func (s *Supervisor) probe(port int) (bool, string) {
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	resp, err := s.client.Get(url)
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return false, ""
	}
	var body struct {
		Service string `json:"service"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Service == "" {
		return false, ""
	}
	return true, body.Service
}

// pullOverview 拉取面板概览（账号数、版本），失败静默。
func (s *Supervisor) pullOverview() {
	snap := s.Snapshot()
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/panel/api/overview", snap.Port), nil)
	if err != nil {
		return
	}
	if snap.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+snap.APIKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var body struct {
		Version string `json:"version"`
		Total   int    `json:"total"`
		Healthy int    `json:"healthy"`
		Cooling int    `json:"cooling"`
		Disabled int   `json:"disabled"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return
	}
	s.mu.Lock()
	s.snap.Accounts = AccountCounts{
		Total: body.Total, Healthy: body.Healthy,
		Cooling: body.Cooling, Disabled: body.Disabled,
	}
	if body.Version != "" {
		s.snap.Version = body.Version
	}
	s.mu.Unlock()
	s.fireChange()
}

// bubble 弹托盘气泡（未接入托盘时忽略）。
func (s *Supervisor) bubble(title, text string) {
	if s.notify != nil {
		s.notify(title, text)
	}
}

// Wait 阻塞到守护循环结束（退出时用于等待子进程收尾）。
func (s *Supervisor) Wait(ctx context.Context) {
	select {
	case <-s.done:
	case <-ctx.Done():
	}
}
