package bridge

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"absorb-agent/internal/log"
)

// Deps 桥的端点依赖（由宿主提供 handler），bridge 包不直接依赖 chat 层。
type Deps struct {
	Ask          http.HandlerFunc // POST /api/ask：扩展问答 SSE（经 "POST /api/ask" 模式注册）
	Cancel       http.HandlerFunc // POST /api/cancel：中断运行
	Capabilities http.HandlerFunc // GET /api/capabilities：默认模型能力下发（扩展端视觉软闸数据源）
	Extra        func(*http.ServeMux) // 附加路由注册（如知识库端点组，由宿主包整体注册；nil 跳过）
}

// Bridge 内嵌 HTTP 服务（扩展问答唯一入口）：宿主只是壳。/health 响应带服务
// 特征字段，供冒烟脚本与外部探测确认服务就绪。
type Bridge struct {
	srv          *http.Server
	port         atomic.Int32 // 实际监听端口：New 时存入，port=0 时由 Start 监听后回填
	version      string
	errCh        chan error   // 服务启动失败时接收错误并保持关闭；正常退出时仅关闭
	lastSeen     atomic.Int64 // 扩展最近一次 Native 事件的 Unix 秒
	nativeConns  atomic.Int64 // Native Messaging 活跃管道数（Edge/Chrome 可各持一条）
	disconnectAt atomic.Int64 // Native 管道全部断开的时刻（0=无断开事件或仍有连接）

	pidMu      sync.Mutex         // 保护 activePIDs
	activePIDs map[int64]struct{} // 已登记管道的宿主进程 PID（connect 上报按 PID 幂等去重）
}

// New 组装桥的路由但不启动（生产用 Start 启动；测试可只构造不监听）。
// port>0 监听固定端口；port=0 由 OS 动态分配，实际端口经 Port() 获取。
func New(port int, version string, deps Deps) *Bridge {
	b := &Bridge{version: version, errCh: make(chan error, 1), activePIDs: map[int64]struct{}{}}
	b.port.Store(int32(port))

	// method-aware 路由模式：方法不符由 ServeMux 自动 405（带 Allow 头），
	// 处理器内不再手工校验 req.Method；OPTIONS 预检单独注册
	mux := http.NewServeMux()
	if deps.Ask != nil {
		mux.HandleFunc("POST /api/ask", deps.Ask)
		mux.HandleFunc("OPTIONS /api/ask", CorsPreflight)
	}
	if deps.Cancel != nil {
		mux.HandleFunc("POST /api/cancel", deps.Cancel)
		mux.HandleFunc("OPTIONS /api/cancel", CorsPreflight)
	}
	if deps.Capabilities != nil {
		// GET 简单请求无 CORS 预检，CORS 响应头由 handler 自带（同 /health）
		mux.HandleFunc("GET /api/capabilities", deps.Capabilities)
	}
	mux.HandleFunc("GET /health", b.healthHandler)
	mux.HandleFunc("POST /ext/connect", b.extConnectHandler)
	mux.HandleFunc("POST /ext/disconnect", b.extDisconnectHandler)
	// 附加端点组（宿主自包注册，如知识库；保持 bridge 与业务包解耦）
	if deps.Extra != nil {
		deps.Extra(mux)
	}

	b.srv = &http.Server{Handler: mux}
	return b
}

// Start 同步监听后异步 serve：监听成功时 Port() 即为实际端口（port=0 时由 OS
// 动态分配并回填），监听失败通过 errCh 上报，StartErr 读取后展示红点。
func (b *Bridge) Start() {
	l, err := net.Listen("tcp", ":"+strconv.Itoa(int(b.port.Load())))
	if err != nil {
		log.Errorf("HTTP bridge failed to start (extension cannot connect): %v", err)
		b.errCh <- err
		close(b.errCh)
		return
	}
	if addr, ok := l.Addr().(*net.TCPAddr); ok {
		b.port.Store(int32(addr.Port))
	}
	log.Infof("HTTP bridge started: http://localhost:%d", b.port.Load())
	go func() {
		if err := b.srv.Serve(l); err != nil && err != http.ErrServerClosed {
			log.Errorf("HTTP bridge exited abnormally: %v", err)
			b.errCh <- err
			close(b.errCh)
		} else {
			close(b.errCh)
		}
	}()
}

// Port 实际监听端口（Start 同步监听完成后可安全读取；port=0 未启动时返回 0）。
func (b *Bridge) Port() int {
	return int(b.port.Load())
}

// Fail 模拟启动失败（测试注入）：写入错误并保持关闭语义。
func (b *Bridge) Fail(err error) {
	b.errCh <- err
	close(b.errCh)
}

// CorsPreflight 浏览器扩展跨域预检（OPTIONS）：带 CORS 头后 204。
// CLI 模式路由复用（bridge.CorsPreflight）。
func CorsPreflight(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.WriteHeader(http.StatusNoContent)
}

// healthHandler /health：特征响应（供冒烟脚本与外部探测确认服务就绪）。
func (b *Bridge) healthHandler(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"service": "absorb-agent",
		"version": b.version,
		"ok":      true,
	})
}

// extConnectHandler Native Messaging 管道建立（nativehost 进程启动时上报；
// 桌面端重启后宿主会周期性重报，按 PID 幂等）。
func (b *Bridge) extConnectHandler(w http.ResponseWriter, req *http.Request) {
	pid, _ := readExtEvent(req)
	b.NativeConnect(pid)
	w.WriteHeader(http.StatusNoContent)
}

// extDisconnectHandler Native Messaging 管道断开（nativehost 进程退出时上报）。
// 计数归零才记断开时刻（Edge/Chrome 同时连接时任一断开不误判）。
func (b *Bridge) extDisconnectHandler(w http.ResponseWriter, req *http.Request) {
	pid, reason := readExtEvent(req)
	log.Infof("extension native pipe disconnect reported (pid=%d, reason=%s)", pid, reason)
	b.NativeDisconnect(pid)
	w.WriteHeader(http.StatusNoContent)
}

// readExtEvent 解析上报 body 中的宿主进程 PID 与断开原因（{"pid":N,"reason":"..."}；
// 缺失或非法时 pid 为 0，仍可登记）。
func readExtEvent(req *http.Request) (pid int64, reason string) {
	var body struct {
		PID    int64  `json:"pid"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return 0, ""
	}
	return body.PID, body.Reason
}

// NativeConnect 记录一条 Native 管道建立；同 PID 重复上报（宿主周期重报）只刷新活跃时刻。
func (b *Bridge) NativeConnect(pid int64) {
	b.pidMu.Lock()
	if _, dup := b.activePIDs[pid]; !dup {
		b.activePIDs[pid] = struct{}{}
		b.nativeConns.Add(1)
		log.Infof("extension native pipe connected (pid=%d)", pid)
	}
	b.pidMu.Unlock()
	b.disconnectAt.Store(0)
	b.lastSeen.Store(time.Now().Unix())
}

// NativeDisconnect 记录一条 Native 管道断开（计数归零时记录断开时刻）。
func (b *Bridge) NativeDisconnect(pid int64) {
	b.pidMu.Lock()
	if _, known := b.activePIDs[pid]; known {
		delete(b.activePIDs, pid)
		if n := b.nativeConns.Add(-1); n <= 0 {
			b.nativeConns.Store(0)
			b.disconnectAt.Store(time.Now().Unix())
		}
	}
	b.pidMu.Unlock()
	b.lastSeen.Store(time.Now().Unix())
}

// ExtLink 返回扩展连接状态判定所需的三个原子值。
func (b *Bridge) ExtLink() (lastSeen, nativeConns, disconnectAt int64) {
	return b.lastSeen.Load(), b.nativeConns.Load(), b.disconnectAt.Load()
}

// SetNativeConns 注入活跃管道数（测试用）。
func (b *Bridge) SetNativeConns(n int64) { b.nativeConns.Store(n) }

// SetDisconnectAt 注入管道全断时刻（Unix 秒，测试用）。
func (b *Bridge) SetDisconnectAt(ts int64) { b.disconnectAt.Store(ts) }

// StartErr 非阻塞读取启动错误（无错误或尚未失败时返回 nil）。
func (b *Bridge) StartErr() error {
	select {
	case err := <-b.errCh:
		return err
	default:
		return nil
	}
}

// Stop 优雅停机（退出路径调用）。
func (b *Bridge) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = b.srv.Shutdown(ctx)
}
