# 打包浏览器扩展为 Go 内嵌资源 extension_bundle.zip
# 用法: powershell -NoProfile -ExecutionPolicy Bypass -File scripts\pack-extension.ps1
$ErrorActionPreference = 'Stop'

$root    = Split-Path -Parent $PSScriptRoot          # 项目根（scripts/ 上一级）
$src     = Join-Path $root 'extension'
$dest    = Join-Path $root 'internal\extension\extension_bundle.zip'
$stage   = Join-Path $env:TEMP 'absorb-ext-stage'

Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory $stage | Out-Null

# 全目录打包：extension/ 内均为运行所需文件（设计文档在 docs/、开发脚本在 scripts/，不在本目录内），
# 新增文件自动进包，避免显式清单遗漏
Copy-Item (Join-Path $src '*') $stage -Recurse -Force

# 文档不随扩展分发：运行时只加载 manifest / js / css / html。
# SPEC.md 是开发期契约（见 extension/sidepanel/js/visual/SPEC.md），vendor/README.md 是第三方说明，
# 均无需进包。
Get-ChildItem -Path $stage -Recurse -Filter '*.md' -File | Remove-Item -Force

if (Test-Path $dest) { Remove-Item $dest -Force }
Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $dest -Force
Remove-Item $stage -Recurse -Force

Write-Host ("packed: {0} ({1} bytes)" -f $dest, (Get-Item $dest).Length)
