# Compose store screenshots: article(860x800) + panel(420x800) -> 1280x800 shot-N.png.
# Raw shots come from e2e spec: ABSORB_E2E_STORE_SHOTS=1 npx playwright test specs/store-shots.spec.ts
# (English-only literals: PS 5.1 parses no-BOM UTF-8 as ANSI.)
param(
  [string]$RawDir = 'docs/store/shots/raw',
  [string]$OutDir = 'docs/store/shots'
)
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing

$rawRoot = Join-Path $PSScriptRoot (Join-Path '..' $RawDir)
$outRoot = Join-Path $PSScriptRoot (Join-Path '..' $OutDir)
if (-not (Test-Path $rawRoot)) { Write-Error "raw dir not found: $rawRoot (run the store-shots spec first)" }
New-Item -ItemType Directory -Force -Path $outRoot | Out-Null

foreach ($n in 1..3) {
  $leftPath = Join-Path $rawRoot ("article-$n.png")
  $rightPath = Join-Path $rawRoot ("panel-$n.png")
  if (-not (Test-Path $leftPath) -or -not (Test-Path $rightPath)) {
    Write-Error "missing raw pair for shot ${n}: $leftPath / $rightPath"
  }
  $left = [System.Drawing.Bitmap]::FromFile($leftPath)
  $right = [System.Drawing.Bitmap]::FromFile($rightPath)
  $canvas = New-Object System.Drawing.Bitmap(1280, 800)
  $g = [System.Drawing.Graphics]::FromImage($canvas)
  $g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
  $g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::HighQuality
  $g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
  $g.Clear([System.Drawing.Color]::White)
  $g.DrawImage($left, 0, 0, 860, 800)
  $g.DrawImage($right, 860, 0, 420, 800)
  # thin divider between article and panel
  $pen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(255, 210, 210, 215), 1)
  $g.DrawLine($pen, 859.5, 0, 859.5, 800)
  $g.Dispose()
  $outPath = Join-Path $outRoot ("shot-$n.png")
  $canvas.Save($outPath, [System.Drawing.Imaging.ImageFormat]::Png)
  $canvas.Dispose(); $left.Dispose(); $right.Dispose()
  Write-Host "composed: $outPath"
}

# promo tile: raw is already 440x280, pass through
$promoSrc = Join-Path $rawRoot 'promo-tile.png'
if (Test-Path $promoSrc) {
  $promo = [System.Drawing.Bitmap]::FromFile($promoSrc)
  $promoOut = Join-Path $outRoot 'promo-tile.png'
  $promo.Save($promoOut, [System.Drawing.Imaging.ImageFormat]::Png)
  $promo.Dispose()
  Write-Host "composed: $promoOut"
}

Write-Host 'done. store assets:'
Get-ChildItem $outRoot | ForEach-Object { Write-Host (" - {0} ({1} bytes)" -f $_.Name, $_.Length) }
