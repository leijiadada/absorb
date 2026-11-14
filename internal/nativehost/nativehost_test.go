package nativehost

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// channelWriter 把每次 Write 转投 channel，供测试按片收集宿主出站字节。
type channelWriter struct {
	ch chan []byte
}

func (c *channelWriter) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	copy(b, p)
	c.ch <- b
	return len(p), nil
}

// readOutMsg 从 channelWriter 分片流还原一条 NM 消息（4 字节长度前缀 + JSON）。
func readOutMsg(t *testing.T, ch chan []byte) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	want := -1
	for want < 0 || buf.Len() < want {
		select {
		case b := <-ch:
			buf.Write(b)
			if want < 0 {
				if buf.Len() < 4 {
					t.Fatalf("长度前缀分片不足 4 字节: %d", buf.Len())
				}
				want = 4 + int(binary.LittleEndian.Uint32(buf.Bytes()[:4]))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("等待出站消息超时")
		}
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes()[4:want], &m); err != nil {
		t.Fatalf("出站消息非合法 JSON: %v", err)
	}
	return m
}

func TestRunHostRepliesAndReports(t *testing.T) {
	inR, inW := io.Pipe()
	outCh := make(chan []byte, 32)
	out := &msgWriter{w: &channelWriter{ch: outCh}}

	var mu sync.Mutex
	var reports [][2]string
	report := func(path, reason string) bool {
		mu.Lock()
		reports = append(reports, [2]string{path, reason})
		mu.Unlock()
		return true
	}
	var port atomic.Int64
	port.Store(8901)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// watchInterval 取 1 小时：测试窗口内监视线程不触发，保持用例确定性
		runHost(inR, out, report, func() int { return int(port.Load()) }, time.Hour)
	}()

	// ping → pong
	if err := writeNMMessage(inW, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	if m := readOutMsg(t, outCh); m["type"] != "pong" {
		t.Fatalf("ping 应答 pong, got %v", m)
	}

	// get-port → bridge 应答携带注入端口的实时值
	port.Store(8902)
	if err := writeNMMessage(inW, []byte(`{"type":"get-port"}`)); err != nil {
		t.Fatal(err)
	}
	if m := readOutMsg(t, outCh); m["type"] != "bridge" || m["port"] != float64(8902) {
		t.Fatalf("get-port 应答 bridge:8902, got %v", m)
	}

	// probe-ack：不出站应答，仅作为入站消息触发重报
	if err := writeNMMessage(inW, []byte(`{"type":"probe-ack"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-outCh:
		t.Fatalf("probe-ack 不应产生出站消息: %x", b)
	case <-time.After(100 * time.Millisecond):
	}

	// stdin 关闭（EOF）→ 宿主退出并上报断开
	inW.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stdin 关闭后宿主未退出")
	}

	mu.Lock()
	defer mu.Unlock()
	want := [][2]string{
		{"/ext/connect", ""},       // 启动
		{"/ext/connect", ""},       // ping
		{"/ext/connect", ""},       // get-port
		{"/ext/connect", ""},       // probe-ack
		{"/ext/disconnect", "eof"}, // stdin EOF
	}
	if len(reports) != len(want) {
		t.Fatalf("上报序列长度不符, got %v", reports)
	}
	for i := range want {
		if reports[i] != want[i] {
			t.Fatalf("上报序列[%d] = %v, want %v (全部: %v)", i, reports[i], want[i], reports)
		}
	}
}

func TestWatchBridgePortProbesOnChange(t *testing.T) {
	outCh := make(chan []byte, 32)
	out := &msgWriter{w: &channelWriter{ch: outCh}}
	var port atomic.Int64
	port.Store(0)

	done := make(chan struct{})
	defer close(done)
	go watchBridgePort(out, func() int { return int(port.Load()) }, done, 15*time.Millisecond)

	// 初值无变化不出站
	select {
	case b := <-outCh:
		t.Fatalf("端口未变化不应出站: %x", b)
	case <-time.After(60 * time.Millisecond):
	}

	// 0 → 8901：桌面端首启，变化触发 probe
	port.Store(8901)
	if m := readOutMsg(t, outCh); m["type"] != "probe" {
		t.Fatalf("端口变化应发 probe, got %v", m)
	}

	// 8901 → 0：桌面端退出，同样视为变化
	port.Store(0)
	if m := readOutMsg(t, outCh); m["type"] != "probe" {
		t.Fatalf("端口再次变化应再发 probe, got %v", m)
	}
}
