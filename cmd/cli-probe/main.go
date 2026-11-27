// cli-probe：CLI 真进程装配探活（发布前手动验收的最小冒烟）。
//
// 拉起 build/bin/absorb-agent-cli.exe --cli，经 data/cli.port 动态端口发现 + /health
// 探活后，逐项检查核心路由注册（/health、/api/capabilities、/api/ask、/api/cancel）。
// main.go 的 CLI mux 与桌面 bridge.Deps 是两套独立注册，新增端点漏挂一边时单测
// 无法发现，本工具以真进程形态兜住这类装配盲区。
//
// 不做真实模型问答（模型行为抽验由 cmd/visual-check 的 integration 测试承担），
// 路由存在性以「非 404/405」判定，与 config.yaml 是否配置可用模型无关。
//
// 前置：make build-cli 产物存在；运行目录为仓库根（config.yaml 与 data/ 相对根）。
// 用法：go run ./cmd/cli-probe（可 -exe / -dir / -timeout 覆盖默认值）。
// 退出码：0 = 全部通过，1 = 存在失败项。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type check struct {
	name string
	ok   bool
	det  string
}

func main() {
	exe := flag.String("exe", filepath.Join("build", "bin", "absorb-agent-cli.exe"), "CLI 可执行文件路径")
	dir := flag.String("dir", ".", "工作目录（仓库根，config.yaml 与 data/ 相对此目录）")
	timeout := flag.Duration("timeout", 40*time.Second, "CLI 启动探活超时")
	flag.Parse()

	portFile := filepath.Join(*dir, "data", "cli.port")
	var results []check
	fail := func(name, det string) { results = append(results, check{name, false, det}) }
	pass := func(name, det string) { results = append(results, check{name, true, det}) }

	start := time.Now()
	proc, base, err := startCLI(*exe, *dir, portFile, *timeout)
	if err != nil {
		fail("CLI 启动+探活", err.Error())
		report(results, start)
		os.Exit(1)
	}
	defer func() {
		_ = proc.Process.Kill()
		_ = proc.Wait()
		_ = os.Remove(portFile) // 强杀不触发进程内 defer，残留文件清掉
	}()
	pass("CLI 启动+探活（cli.port 端口发现 + /health）", base)

	// /health：200 且响应体 ok（原生 mux 注册，非 bridge.Deps）
	body, status, err := httpGet(base + "/health")
	switch {
	case err != nil:
		fail("GET /health", err.Error())
	case status != 200 || strings.TrimSpace(body) != "ok":
		fail("GET /health", fmt.Sprintf("status=%d body=%q", status, body))
	default:
		pass("GET /health", "200 ok")
	}

	// /api/capabilities：200 + JSON 含 imageSupport 字段 + CORS 头（扩展端跨域读取依赖）
	body, status, err = httpGet(base + "/api/capabilities")
	if err != nil {
		fail("GET /api/capabilities", err.Error())
	} else if status != 200 {
		fail("GET /api/capabilities", fmt.Sprintf("status=%d", status))
	} else {
		var caps struct {
			ImageSupport *bool `json:"imageSupport"`
		}
		if jerr := json.Unmarshal([]byte(body), &caps); jerr != nil {
			fail("GET /api/capabilities", "JSON 解析失败: "+jerr.Error())
		} else if caps.ImageSupport == nil {
			fail("GET /api/capabilities", "响应缺 imageSupport 字段: "+body)
		} else {
			pass("GET /api/capabilities", fmt.Sprintf("200 imageSupport=%v", *caps.ImageSupport))
		}
	}

	// /api/ask、/api/cancel：路由注册存在性（空请求体的校验错误即可证明非 404）
	for _, rt := range []string{"/api/ask", "/api/cancel"} {
		status, err = httpPostEmpty(base + rt)
		switch {
		case err != nil:
			fail("POST "+rt, err.Error())
		case status == 404 || status == 405:
			fail("POST "+rt, fmt.Sprintf("路由未注册（status=%d）", status))
		default:
			pass("POST "+rt, fmt.Sprintf("status=%d（校验/业务错误均可，路由在位）", status))
		}
	}

	report(results, start)
	if anyFail(results) {
		os.Exit(1)
	}
}

// startCLI 清理残留端口文件后拉起 CLI，轮询 cli.port + /health 直到探活通过。
func startCLI(exe, dir, portFile string, timeout time.Duration) (*exec.Cmd, string, error) {
	if _, err := os.Stat(exe); err != nil {
		return nil, "", fmt.Errorf("CLI exe 不存在（先 make build-cli）: %w", err)
	}
	if err := os.Remove(portFile); err != nil && !os.IsNotExist(err) {
		return nil, "", err
	}
	cmd := exec.Command(exe, "--cli")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			_ = cmd.Wait()
			return nil, "", fmt.Errorf("CLI 进程提前退出 exit=%d", cmd.ProcessState.ExitCode())
		}
		if raw, rerr := os.ReadFile(portFile); rerr == nil {
			port := strings.TrimSpace(string(raw))
			if port != "" {
				base := "http://127.0.0.1:" + port
				if body, status, gerr := httpGet(base + "/health"); gerr == nil && status == 200 && strings.TrimSpace(body) == "ok" {
					return cmd, base, nil
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return nil, "", fmt.Errorf("%v 内 cli.port 未出现或探活未通过", timeout)
}

func httpGet(url string) (string, int, error) {
	resp, err := http.Get(url) //nolint:gosec // 探活目标为本进程产出的本地地址
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, err
}

// httpPostEmpty 发空 JSON 体，仅用于判定路由存在性（404/405 = 未注册）。
func httpPostEmpty(url string) (int, error) {
	resp, err := http.Post(url, "application/json", strings.NewReader("{}")) //nolint:gosec // 同上
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func anyFail(results []check) bool {
	for _, c := range results {
		if !c.ok {
			return true
		}
	}
	return false
}

func report(results []check, start time.Time) {
	for _, c := range results {
		mark := "PASS"
		if !c.ok {
			mark = "FAIL"
		}
		line := fmt.Sprintf("[%s] %s", mark, c.name)
		if c.det != "" {
			line += "：" + c.det
		}
		fmt.Println(line)
	}
	nfail := 0
	for _, c := range results {
		if !c.ok {
			nfail++
		}
	}
	fmt.Printf("=== 探活完成：%d/%d 通过（耗时 %s）===\n",
		len(results)-nfail, len(results), time.Since(start).Round(time.Second))
}
