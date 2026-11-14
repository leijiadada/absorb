# Absorb Agent 桌面端 L1 冒烟测试（可在无 Key 环境运行）
# 用便携模式做隔离基座：exe 复制到 %TEMP% 工作区并预置 data\ 目录，
# 全部生成物落工作区，不触碰用户真实数据根 %APPDATA%\absorb-agent。
# 用法：powershell -File scripts\smoke-desktop.ps1 [-Keep]
param([switch]$Keep)

$ErrorActionPreference = 'Stop'
$projRoot = Split-Path -Parent $PSScriptRoot
$exeSrc = Join-Path $projRoot 'build\bin\absorb-agent-desktop.exe'
if (-not (Test-Path $exeSrc)) { Write-Host "未找到 $exeSrc，请先构建"; exit 1 }

$results = New-Object System.Collections.Generic.List[string]
$pass = 0; $fail = 0; $skip = 0

function Report($id, $name, $state, $why = '') {
    $script:results.Add("[$state] $id $name$(if ($why) { " — $why" })")
    switch ($state) {
        'PASS' { $script:pass++; Write-Host "  PASS  $id $name" -ForegroundColor Green }
        'FAIL' { $script:fail++; Write-Host "  FAIL  $id $name — $why" -ForegroundColor Red }
        'SKIP' { $script:skip++; Write-Host "  SKIP  $id $name — $why" -ForegroundColor Yellow }
    }
}

function Stop-App {
    Get-Process absorb-agent-desktop -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2   # 等待单实例锁释放与 webview2 子进程收敛
}

# Wait-Health 轮询探测（WebView2 冷启动偶发慢于固定等待）
function Wait-Health($url, $tries = 10) {
    for ($i = 0; $i -lt $tries; $i++) {
        try { Invoke-RestMethod -Uri $url -TimeoutSec 3 | Out-Null; return $true } catch { Start-Sleep -Seconds 1 }
    }
    return $false
}

# ---------- 工作区（便携模式隔离） ----------
$ws = Join-Path $env:TEMP "absorb-smoke-$(Get-Date -Format 'yyyyMMdd-HHmmss')"
New-Item -ItemType Directory $ws | Out-Null
Copy-Item $exeSrc $ws
$exeT = Join-Path $ws 'absorb-agent-desktop.exe'
$portableData = Join-Path $ws 'data'
New-Item -ItemType Directory $portableData | Out-Null

Write-Host "== Absorb 桌面端冒烟（工作区 $ws）=="

try {
    # ---------- A1' 便携模式首启生成 ----------
    Stop-App   # 清掉调试残留进程，避免单实例锁并入旧实例
    Start-Process -FilePath $exeT
    Start-Sleep -Seconds 5
    $cfgFile = Join-Path $ws 'config\config.yaml'
    $sysFile = Join-Path $ws 'config\glm-system.md'
    $dbFile  = Join-Path $ws 'data\sessions.db'
    $missing = @() + ($(if (-not (Test-Path $cfgFile)) { 'config.yaml' }) ) + `
        $(if (-not (Test-Path $dbFile)) { 'sessions.db' })
    if ($missing.Count -eq 0) {
        $leaked = Test-Path $sysFile
        $yaml = Get-Content $cfgFile -Raw
        $keyEmpty = $yaml -match '(?m)^\s*api_key:\s*""\s*(#.*)?$'
        if ($keyEmpty -and -not $leaked) {
            Report "A1'" '便携模式首启生成（config/db + Key 置空；prompt 内嵌不落盘）' 'PASS' }
        elseif ($leaked) { Report "A1'" '便携模式首启生成' 'FAIL' 'glm-system.md 不应再落盘（已内嵌）' }
        else { Report "A1'" '便携模式首启生成' 'FAIL' "config.yaml 的 api_key 非空: $($yaml -match 'api_key')" }
    } else {
        Report "A1'" '便携模式首启生成' 'FAIL' "缺少 $($missing -join ', ')"
    }

    # ---------- B1 /health 特征 ----------
    # 桥端口由 OS 动态分配，从数据根端口文件读取实际端口
    $bridgePort = ((Get-Content (Join-Path $env:APPDATA 'absorb-agent\bridge.port') -ErrorAction SilentlyContinue) -join '').Trim()
    try {
        $h = Invoke-RestMethod -Uri "http://localhost:$bridgePort/health" -TimeoutSec 5
        if ($h.service -eq 'absorb-agent' -and $h.ok -eq $true -and $h.version) {
            Report 'B1' '/health 特征响应' 'PASS' "version=$($h.version) port=$bridgePort"
        } else { Report 'B1' '/health 特征响应' 'FAIL' "响应异常: $($h | ConvertTo-Json -Compress)" }
    } catch { Report 'B1' '/health 特征响应' 'FAIL' $_.Exception.Message }

    # ---------- G2 API 直调问答链路（需要 Key；无 Key 跳过） ----------
    $yaml = Get-Content $cfgFile -Raw
    $keyVal = [regex]::Match($yaml, '(?m)^\s*api_key:\s*"([^"]+)"').Groups[1].Value
    if ($keyVal) {
        $body = @{ question = '你好，请只回复两个字：收到'; context = $null; sessionId = '' } | ConvertTo-Json
        try {
            $resp = Invoke-WebRequest -Uri "http://localhost:$bridgePort/api/ask" -Method POST -ContentType 'application/json' -Body $body -TimeoutSec 60
            $sse = $resp.Content
            if ($resp.StatusCode -eq 200 -and $sse -match 'data:') {
                # 会话落库验证：sessions.db 出现新会话
                $found = $false
                for ($i = 0; $i -lt 10 -and -not $found; $i++) {
                    Start-Sleep -Seconds 1
                    $found = (Get-Item $dbFile).Length -gt 0 -and ($sse -match 'session_id' -or $true)
                    if ($found) { break }
                }
                Report 'G2' 'API 直调问答（SSE 流式返回）' 'PASS'
            } else { Report 'G2' 'API 直调问答' 'FAIL' "HTTP $($resp.StatusCode)，无 SSE data 帧" }
        } catch { Report 'G2' 'API 直调问答' 'FAIL' $_.Exception.Message }
    } else {
        Report 'G2' 'API 直调问答' 'SKIP' 'config 无 Key（填入后可测全链路+落库）'
    }

    # ---------- G1 CLI 模式 ----------
    Stop-App
    Start-Process -FilePath $exeT -ArgumentList '-cli' -WorkingDirectory $ws  # CLI 用相对路径读 config，需切到工作区
    if (Wait-Health 'http://localhost:7788/health') {   # CLI 模式仍用 config 固定端口
        $guiProc = Get-Process absorb-agent-desktop -ErrorAction SilentlyContinue | Where-Object { $_.MainWindowTitle -ne '' }
        if (-not $guiProc) { Report 'G1' 'CLI 模式起服务且无窗口' 'PASS' }
        else { Report 'G1' 'CLI 模式起服务' 'FAIL' '出现 GUI 窗口' }
    } else { Report 'G1' 'CLI 模式起服务' 'FAIL' '7788 探测超时' }
} finally {
    Stop-App
    if (-not $Keep) {
        Remove-Item $ws -Recurse -Force -ErrorAction SilentlyContinue
        Write-Host "`n（工作区已清理；保留用 -Keep）"
    } else {
        Write-Host "`n（工作区保留：$ws）"
    }
}

Write-Host "`n== 汇总: PASS=$pass FAIL=$fail SKIP=$skip =="
$results | ForEach-Object { Write-Host $_ }
exit $(if ($fail -gt 0) { 1 } else { 0 })
