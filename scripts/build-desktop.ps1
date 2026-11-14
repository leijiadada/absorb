# Build a desktop/CLI exe with safe process handling (dev experience):
#   1) stop running instances (GUI + NM-host are the same image name) and wait until exited
#   2) go build to a temp file - writing a NEW file is never blocked by a running exe,
#      so Edge respawning a native host during the build cannot fail the build
#   3) stop instances again (Edge may have respawned an OLD-binary host during the build)
#   4) swap temp -> final with retry (retry re-stops in case a host locks the file again)
# Output messages stay in English on purpose (PowerShell 5.1 parses no-BOM UTF-8 as ANSI).
param(
  [Parameter(Mandatory = $true)][string]$App,      # output exe name without extension
  [string]$Version = '0.1.0',
  [string]$StopNames = '',                          # comma-separated process names to stop (default: $App)
  [switch]$Gui,                                     # GUI build adds -H windowsgui
  [switch]$UseWails,                                # wails official pipeline (desktop only; it force-links -H windowsgui)
  [string]$WailsTool = 'wails',                     # wails CLI path (official build pipeline)
  [string]$GoTool = 'go'                            # go toolchain path (manual build for CLI console image)
)
$ErrorActionPreference = 'Stop'
$binDir = 'build/bin'
$final = Join-Path $binDir ($App + '.exe')
$tmp = Join-Path $binDir ($App + '.build.exe')
$names = @($App)
if ($StopNames) { $names = @($StopNames -split ',' | Where-Object { $_ }) }

function Stop-Instances([string[]]$processNames) {
  foreach ($n in $processNames) {
    Stop-Process -Name $n -Force -ErrorAction SilentlyContinue
  }
}

function Wait-Exited([string[]]$processNames, [int]$Seconds = 5) {
  $deadline = (Get-Date).AddSeconds($Seconds)
  while ((Get-Date) -lt $deadline) {
    $alive = $false
    foreach ($n in $processNames) {
      if (Get-Process -Name $n -ErrorAction SilentlyContinue) { $alive = $true; break }
    }
    if (-not $alive) { return }
    Start-Sleep -Milliseconds 200
  }
}

Write-Host ('[build] stopping running instances: ' + ($names -join ', '))
Stop-Instances -processNames $names
Wait-Exited -processNames $names

New-Item -ItemType Directory -Force -Path $binDir | Out-Null
$builtAt = Get-Date -Format 'yyyy-MM-ddTHH:mm'
$ldflags = '-w -s -X main.desktopVersion=' + $Version + ' -X main.builtAt=' + $builtAt
if ($Gui) { $ldflags = $ldflags + ' -H windowsgui' }

if ($UseWails) {
  # wails CLI: official pipeline (resources from build/appicon.png + go build).
  # -s: frontend dist is built by make build-web (npm via PS channel); wails cannot
  #     exec npm from msys-inherited POSIX-format PATH.
  # -skipbindings -m: skip child-process steps that fail the same way (exit status 1).
  # Note: production builds are force-linked -H windowsgui, unsuitable for CLI console image.
  if (-not (Get-Command $WailsTool -ErrorAction SilentlyContinue)) {
    Write-Error ("[build] wails CLI not found: '" + $WailsTool + "'. Install with: go install github.com/wailsapp/wails/v2/cmd/wails@latest")
    exit 1
  }
  Write-Host ('[build] wails build -> ' + $tmp)
  # wails -o takes a bare filename and always writes into build/bin
  & $WailsTool build -s -skipbindings -m -tags desktop,production -ldflags $ldflags -o ($App + '.build.exe')
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} else {
  Write-Host ('[build] go build -> ' + $tmp)
  & $GoTool build -buildvcs=false -tags desktop,production -ldflags $ldflags -o $tmp .
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}
if (-not (Test-Path $tmp)) {
  Write-Error ('[build] build reported success but ' + $tmp + ' is missing')
  exit 1
}

# Clear the old-binary native host Edge may have respawned during the build
Stop-Instances -processNames $names
Start-Sleep -Milliseconds 500

$swapped = $false
foreach ($i in 1..3) {
  try {
    Move-Item -Force -Path $tmp -Destination $final -ErrorAction Stop
    $swapped = $true
    break
  } catch {
    Write-Host ('[build] swap attempt ' + $i + ' blocked (exe held by a respawned host?), retrying')
    Stop-Instances -processNames $names
    Start-Sleep -Milliseconds 800
  }
}
if (-not $swapped) {
  Write-Error ('[build] failed to replace ' + $final + ' (still locked after retries)')
  exit 1
}
Write-Host ('[build] done: ' + $final)
