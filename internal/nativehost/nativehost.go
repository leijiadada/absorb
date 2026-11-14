package nativehost

// Native Messaging 宿主与注册。
// 浏览器按 HKCU 注册表定位 native-host.json → 拉起本 exe（--native-host，无 GUI），
// 经 stdin/stdout 交换「4 字节小端长度前缀 + JSON」消息。管道建立/断开（SW 休眠、
// 浏览器关闭、扩展卸载）时向桌面端 HTTP 桥上报，扩展连接状态由此秒级感知。

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows/registry"
)

const (
	nativeHostName = "com.absorb.desktop"
	nativeExtID    = "pgabpbnopahacdbfdmpgdmohbeppflgd"
)

// RunNativeHost Native Messaging 宿主入口。浏览器拉起时 argv 首参为 origin，
// 与固定扩展 ID 不符则直接退出（防其他扩展占用通道）。
func RunNativeHost(args []string) {
	if len(args) == 0 || args[0] != "chrome-extension://"+nativeExtID+"/" {
		fmt.Fprintf(os.Stderr, "[absorb-native] origin validation failed: %v\n", args)
		return
	}
	runHost(os.Stdin, &msgWriter{w: os.Stdout}, reportExtEvent, readBridgePort, time.Second)
}

// runHost 宿主核心循环，stdin/stdout/上报/端口读取均可注入（单测）。
// 每条消息都重报建立事件：桥按 PID 幂等去重，桌面端重启换新桥后，宿主下一条
// 消息（ping 周期 ≤30s 或 probe 应答）即恢复连接状态；孤儿宿主收不到消息而
// 保持静默，不污染新桥状态。
func runHost(in io.Reader, out *msgWriter, report func(path, reason string) bool, readPort func() int, watchInterval time.Duration) {
	report("/ext/connect", "")
	defer report("/ext/disconnect", "eof")

	// 静默超时自杀：长期收不到浏览器消息的宿主是 SW 世代更替遗留的孤儿管道
	// （活跃管道由扩展 30s ping 保活），自行上报断开并退出，避免孤儿进程堆积。
	// 超时阈值 2 分钟 = 4 个 ping 周期；经 os.Exit 退出会跳过 defer，故此处自行
	// 上报断开，双报由桥端按 PID 幂等兜底。
	var lastMsg atomic.Int64
	lastMsg.Store(time.Now().UnixNano())
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastMsg.Load())) > 2*time.Minute {
					fmt.Fprintf(os.Stderr, "[absorb-native] no browser message for 2 minutes, host exiting\n")
					report("/ext/disconnect", "selfkill")
					os.Exit(0)
				}
			}
		}
	}()

	// 桌面端主动探测：端口文件变化（桌面端首启/重启漂移）时发 probe 唤醒扩展
	// SW 回 ack，ack 作为入站消息触发下方重报，连接状态秒级恢复
	go watchBridgePort(out, readPort, done, watchInterval)

	for {
		msg, err := readNMMessage(in)
		if err != nil {
			// EOF/损坏输入：管道断开（SW 休眠、浏览器关闭、扩展卸载）
			return
		}
		lastMsg.Store(time.Now().UnixNano())
		var m map[string]any
		if json.Unmarshal(msg, &m) != nil {
			continue
		}
		switch m["type"] {
		case "ping":
			_ = out.write([]byte(`{"type":"pong"}`))
		case "get-port":
			_ = out.write(bridgePortReply(readPort()))
		}
		// probe-ack 无需专门分支：任意入站消息统一走到这里完成重报
		report("/ext/connect", "")
	}
}

// bridgePortFile 桌面端桥端口文件路径（桌面端启动时把实际监听端口写在此处）。
func bridgePortFile() (string, error) {
	dir, err := dataRootDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "bridge.port"), nil
}

// readBridgePort 读桥端口文件并解析为端口号；文件缺失（桌面端未启动）或内容
// 非法时返回 0 表示未知。
func readBridgePort() int {
	file, err := bridgePortFile()
	if err != nil {
		return 0
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

// bridgePortReply 构造 get-port 的应答消息：{"type":"bridge","port":N}（0=未知）。
func bridgePortReply(port int) []byte {
	b, err := json.Marshal(map[string]any{"type": "bridge", "port": port})
	if err != nil {
		return []byte(`{"type":"bridge","port":0}`)
	}
	return b
}

// msgWriter 跨 goroutine 的整消息串行写出：长度前缀与载荷在同一次锁内完成，
// 避免主循环应答与端口探测线程交错写坏帧。
type msgWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (m *msgWriter) write(msg []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return writeNMMessage(m.w, msg)
}

// watchBridgePort 轮询桥端口文件，内容变化（桌面端首启/重启漂移）时向浏览器侧
// 发 probe 消息唤醒扩展 SW，SW 回 probe-ack 后由主循环重报 connect，连接状态
// 秒级恢复；无变化零流量，探测失败不影响主循环（30s alarm 链路兜底）。
func watchBridgePort(out *msgWriter, readPort func() int, done <-chan struct{}, interval time.Duration) {
	last := readPort()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			cur := readPort()
			if cur == last {
				continue
			}
			last = cur
			_ = out.write([]byte(`{"type":"probe"}`))
		}
	}
}

// reportExtEvent 向桌面端 HTTP 桥上报连接事件（body 带宿主进程 PID，桥按 PID
// 幂等去重；reason 为断开原因，connect 事件传空省略）。桥端口每次从数据根端口
// 文件实时读取（跟随桌面端重启后的端口漂移），桌面端未启动或端口未知时静默放弃
// 并返回 false。
func reportExtEvent(path, reason string) bool {
	port := readBridgePort()
	if port == 0 {
		fmt.Fprintf(os.Stderr, "[absorb-native] bridge port unknown (desktop app not running or port file missing), skip reporting %s\n", path)
		return false
	}
	body := map[string]any{"pid": os.Getpid()}
	if reason != "" {
		body["reason"] = reason
	}
	data, _ := json.Marshal(body)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(fmt.Sprintf("http://localhost:%d%s", port, path), "application/json", bytes.NewReader(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[absorb-native] report %s failed: %v\n", path, err)
		return false
	}
	resp.Body.Close()
	return true
}

// readNMMessage 读取一条 NM 消息（4 字节小端长度前缀 + JSON，入站上限 1MB）。
func readNMMessage(r io.Reader) ([]byte, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, err
	}
	if n == 0 || n > 1<<20 {
		return nil, fmt.Errorf("非法消息长度 %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// writeNMMessage 写出一条 NM 消息（4 字节小端长度前缀 + JSON）。
func writeNMMessage(w io.Writer, msg []byte) error {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(msg)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(msg)
	return err
}

// nativeHostManifest 生成 native-host.json 内容（path 指向当前 exe，白名单只放行固定扩展 ID）。
func nativeHostManifest(exe string) ([]byte, error) {
	return json.MarshalIndent(map[string]any{
		"name":        nativeHostName,
		"description": "Absorb 阅读助手桌面端",
		"path":        exe,
		"type":        "stdio",
		"allowed_origins": []string{
			"chrome-extension://" + nativeExtID + "/",
		},
	}, "", "  ")
}

// nativeRegistryRoots Edge / Chrome 的 NativeMessagingHosts 注册表键（HKCU，无需管理员）。
func nativeRegistryRoots() []string {
	return []string{
		`Software\Microsoft\Edge\NativeMessagingHosts`,
		`Software\Google\Chrome\NativeMessagingHosts`,
	}
}

// dataRootDir 宿主数据根：便携模式（exe 旁存在 data\ 目录）为 exe 目录，
// 安装/开发模式为 %APPDATA%\absorb-agent。清单、桥端口文件等运行时注册数据
// 必须落在普通用户可写的数据根——安装目录（Program Files）可能只读，exe 旁会写失败。
func dataRootDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(dir, "data")); err == nil {
		return dir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(base, "absorb-agent")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
}

// manifestPath native-host.json 落点：数据根目录下的清单文件。
func manifestPath() (string, error) {
	dir, err := dataRootDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "native-host.json"), nil
}

// RegisterNativeHost 生成 native-host.json 到数据根并写入双浏览器注册表。
// 安装器 [Run] 与开发环境手工注册共用。
func RegisterNativeHost() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	jsonPath, err := manifestPath()
	if err != nil {
		return err
	}
	data, err := nativeHostManifest(exe)
	if err != nil {
		return err
	}
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		return err
	}
	for _, root := range nativeRegistryRoots() {
		key, _, err := registry.CreateKey(registry.CURRENT_USER, root+`\`+nativeHostName, registry.SET_VALUE)
		if err != nil {
			return err
		}
		err = key.SetStringValue("", jsonPath)
		key.Close()
		if err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "Native Messaging host registered: %s\n", jsonPath)
	return nil
}

// UnregisterNativeHost 移除双浏览器注册表项与数据根的 native-host.json（卸载路径调用）。
func UnregisterNativeHost() error {
	var firstErr error
	for _, root := range nativeRegistryRoots() {
		if err := registry.DeleteKey(registry.CURRENT_USER, root+`\`+nativeHostName); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if path, err := manifestPath(); err == nil {
		_ = os.Remove(path)
	}
	return firstErr
}
