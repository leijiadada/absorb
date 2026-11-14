# 确保本机具备 Inno Setup（ISCC.exe）与中文语言包（打包安装器的前置环境）
# 用法:
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\ensure-iscc.ps1 -Ensure  # 缺失则自动安装，失败 exit 1
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\ensure-iscc.ps1 -Resolve # 打印 ISCC.exe 路径（找不到输出空）
param(
    [switch]$Ensure,
    [switch]$Resolve,
    [string]$IsccOverride = ''   # make installer ISCC=C:/path/ISCC.exe 的人工指定路径
)
$ErrorActionPreference = 'Stop'

# 固定版本保证可复现（本机 winget 过旧会崩溃，故不走 winget，直接 GitHub releases 静默安装）。
# 与用户确认固定使用 Inno Setup 7.1.0（is-7_1_0 下的 innosetup-7.1.0-x86.exe，经验证存在且可下载），
# 不跟随 latest，避免未来 7.x 行为漂移影响安装包产出。
# 历史注：更早硬编码的 6.7.3 资产其实未被删，曾因手误拼错文件名（下划线）被判 404，实为 URL 拼错。
$PinnedVersion = '7.1.0'
$PinnedSetupUrl = "https://github.com/jrsoftware/issrc/releases/download/is-7_1_0/innosetup-7.1.0-x86.exe"
$IslTag         = 'is-7_1_0'

$script:SetupUrl = $PinnedSetupUrl
$IsccVersion = $PinnedVersion
# 中文语言包为非官方翻译，官方安装器可能不带，须单独补装（与 ISCC 同一版本 tag）。
# Inno 7 起官方已自带 ChineseSimplified.isl，此下载仅作缺失兜底。
$IslUrl      = "https://raw.githubusercontent.com/jrsoftware/issrc/$IslTag/Files/Languages/ChineseSimplified.isl"

$SetupMinBytes = 5MB   # 低于此值视为下载到了错误内容（如跳转页 HTML）
$TmpSetup      = Join-Path $env:TEMP "innosetup-$IsccVersion.exe"
# 非管理员时装到用户目录（实测 Inno 7 静默安装 /DIR= 用户目录无需提权）
$UserIsccDir   = Join-Path $env:LOCALAPPDATA 'InnoSetup'

function Fail([string]$msg) {
    Write-Host "ERROR: $msg" -ForegroundColor Red
    exit 1
}

# Find-Iscc 按顺序探测: 人工指定路径 -> Program Files (x86)/Program Files 各版本 -> 用户目录安装
function Find-Iscc([string]$Override) {
    if ($Override) {
        if (Test-Path $Override) { return (Resolve-Path $Override).Path }
        return $null
    }
    $bases = @("${env:ProgramFiles(x86)}", $env:ProgramFiles, $env:LOCALAPPDATA, $env:USERPROFILE)
    foreach ($base in $bases) {
        if (-not $base) { continue }
        foreach ($dir in @('Inno Setup 7', 'Inno Setup 6', 'InnoSetup7', 'InnoSetup')) {
            $p = Join-Path (Join-Path $base $dir) 'ISCC.exe'
            if (Test-Path $p) { return $p }
        }
    }
    return $null
}

function Download([string]$url, [string]$out) {
    # -f：HTTP 4xx/5xx 返回非 0 并删除输出，避免把错误页当成功存下来
    & curl.exe -fsSL -o $out $url
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path $out)) { Fail "下载失败: $url" }
}

if ($Resolve) {
    $iscc = Find-Iscc $IsccOverride
    if ($iscc) { Write-Output $iscc }
    exit 0
}

if (-not $Ensure) { Fail '未指定模式，使用 -Ensure 或 -Resolve' }

# ---- Ensure 模式: 缺什么补什么，任一步失败即 exit 1（make 据此中止打包） ----

$iscc = Find-Iscc $IsccOverride
if (-not $iscc) {
    if ($IsccOverride) {
        Fail "指定的 ISCC 不存在: $IsccOverride"
    }
    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    # 非管理员不再直接失败：静默安装到用户目录（Inno 支持 /DIR=，实测免提权）。
    $installArgs = @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/SP-')
    if (-not $isAdmin) {
        Write-Host "无管理员权限，安装到用户目录: $UserIsccDir"
        $installArgs += "/DIR=$UserIsccDir"
    }
    # 下载（已有的合法安装包直接复用，重试不重复下载）
    if ((Test-Path $TmpSetup) -and (Get-Item $TmpSetup).Length -ge $SetupMinBytes) {
        Write-Host "复用已下载的安装包: $TmpSetup"
    } else {
        Write-Host "未检测到 Inno Setup，下载 ${IsccVersion}: $SetupUrl"
        Download $SetupUrl $TmpSetup
        if ((Get-Item $TmpSetup).Length -lt $SetupMinBytes) {
            Remove-Item $TmpSetup -Force -ErrorAction SilentlyContinue
            Fail "安装包下载不完整，请手动下载: $SetupUrl"
        }
    }
    # 静默安装
    $p = Start-Process -FilePath $TmpSetup -ArgumentList $installArgs -Wait -PassThru
    if ($p.ExitCode -ne 0) { Fail "Inno Setup 安装程序退出码 $($p.ExitCode)" }
    $iscc = Find-Iscc $null
    if (-not $iscc) { Fail "安装后仍未找到 ISCC.exe，请手动安装: $SetupUrl" }
    Write-Host "Inno Setup $IsccVersion 安装完成: $iscc"
}

# ---- 中文语言包（setup.iss 的 [Languages] 引用，缺失时编译中止） ----

$isl = Join-Path (Split-Path $iscc -Parent) 'Languages\ChineseSimplified.isl'
if (-not (Test-Path $isl)) {
    Write-Host "中文语言包缺失，下载: $IslUrl"
    Download $IslUrl $isl
    if ((Get-Item $isl).Length -lt 5KB) {
        Remove-Item $isl -Force -ErrorAction SilentlyContinue
        Fail "语言包下载异常，请手动下载: $IslUrl -> $isl"
    }
    Write-Host "中文语言包安装完成: $isl"
}

Write-Host "ISCC 环境就绪: $iscc"
