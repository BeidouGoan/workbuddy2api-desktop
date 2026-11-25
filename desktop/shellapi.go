package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

//go:embed ui/console.html
var consoleFS embed.FS

// shellServer 是对内嵌控制台页面提供的数据通道：
// 页面只与随机端口的本机 HTTP 服务通信，避免 WebView2 里的跨域问题，
// 也不用把能力直接暴露成 JS 绑定。
type shellServer struct {
	app    *App
	token  string
	ln     net.Listener
	server *http.Server
	url    string
}

func newShellServer(app *App) (*shellServer, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &shellServer{app: app, token: hex.EncodeToString(buf), ln: ln}
	s.url = fmt.Sprintf("http://%s/?t=%s", ln.Addr().String(), s.token)
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/action", s.handleAction)
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return s, nil
}

func (s *shellServer) run() {
	_ = s.server.Serve(s.ln)
}

func (s *shellServer) close() {
	_ = s.server.Close()
}

// authorized 只认带正确 token 的请求：控制台页面 URL 里的 token 是本进程随机生成的，
// 其他本机网页拿不到，无法借道调用退出/重启之类的动作。
func (s *shellServer) authorized(r *http.Request) bool {
	if r.Header.Get("X-WB2A-Token") == s.token {
		return true
	}
	return r.URL.Query().Get("t") == s.token
}

func (s *shellServer) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	raw, err := consoleFS.ReadFile("ui/console.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body := strings.ReplaceAll(string(raw), "__TOKEN__", s.token)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

func (s *shellServer) handleState(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.app.statePayload())
}

type actionRequest struct {
	Action string `json:"action"`
	Text   string `json:"text"`
	Value  *bool  `json:"value"`
}

func (s *shellServer) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req actionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msg, err := s.app.doAction(req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"ok": "1", "message": msg})
}
