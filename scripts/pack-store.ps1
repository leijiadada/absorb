# Build store submission packages from extension/:
#   Edge package  : manifest WITHOUT the "key" field (Edge validation rejects it; ID assigned by Edge)
#   CWS package   : manifest WITH the "key" field (pins the extension ID to the dev unpacked ID)
# Zip entries use forward slashes (PS 5.1 Compress-Archive writes backslashes and some
# store validators reject such packages). Markdown files are excluded, same as the bundle.
# Usage: powershell -NoProfile -ExecutionPolicy Bypass -File scripts\pack-store.ps1 [-Version 0.1.0]
param(
  [string]$Version = '0.1.0'
)
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.IO.Compression
Add-Type -AssemblyName System.IO.Compression.FileSystem

$root = Split-Path -Parent $PSScriptRoot
$src = Join-Path $root 'extension'
$outDir = Join-Path $root 'build\bin'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

function New-StoreZip([string]$OutPath, [bool]$StripKey) {
  $stage = Join-Path $env:TEMP ('absorb-ext-store-' + [IO.Path]::GetFileNameWithoutExtension($OutPath))
  Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
  New-Item -ItemType Directory $stage | Out-Null
  Copy-Item (Join-Path $src '*') $stage -Recurse -Force
  Get-ChildItem -Path $stage -Recurse -Filter '*.md' -File | Remove-Item -Force
  $manifestPath = Join-Path $stage 'manifest.json'
  $manifest = [IO.File]::ReadAllText($manifestPath)
  if ($StripKey) {
    # remove the whole "key": "..." line (single line incl. trailing comma, JSON stays valid)
    $manifest = [regex]::Replace($manifest, '(?m)^\s*"key":\s*"[^"]*",\s*\r?\n', '')
    if ($manifest -match '"key"') { Write-Error 'key field was not removed from manifest' }
  }
  [IO.File]::WriteAllText($manifestPath, $manifest, [Text.UTF8Encoding]::new($false))
  if (Test-Path $OutPath) { Remove-Item $OutPath -Force }
  $fs = [IO.File]::Open($OutPath, 'Create')
  $arc = New-Object IO.Compression.ZipArchive($fs, [IO.Compression.ZipArchiveMode]::Create)
  foreach ($f in (Get-ChildItem $stage -Recurse -File)) {
    $rel = $f.FullName.Substring($stage.Length + 1).Replace('\', '/')
    [IO.Compression.ZipFileExtensions]::CreateEntryFromFile($arc, $f.FullName, $rel, [IO.Compression.CompressionLevel]::Optimal) | Out-Null
  }
  $arc.Dispose(); $fs.Dispose()
  Remove-Item $stage -Recurse -Force
}

function Assert-Zip([string]$OutPath, [bool]$ExpectKey) {
  $zip = [IO.Compression.ZipFile]::OpenRead($OutPath)
  try {
    $entries = @($zip.Entries | Where-Object { $_.FullName -ne '' })
    $badSep = @($entries | Where-Object { $_.FullName -like '*\*' }).Count
    $md = @($entries | Where-Object { $_.Name -like '*.md' }).Count
    if ($badSep -ne 0 -or $md -ne 0) { Write-Error ("zip check failed: backslash=$badSep md=$md") }
    $manifestEntry = $entries | Where-Object { $_.FullName -eq 'manifest.json' }
    if (-not $manifestEntry) { Write-Error 'manifest.json missing in zip' }
    $manifest = [IO.StreamReader]::new($manifestEntry.Open()).ReadToEnd()
    $hasKey = $manifest -match '"key"'
    if ($hasKey -ne $ExpectKey) { Write-Error ("manifest key mismatch: hasKey=$hasKey expect=$ExpectKey") }
    if ($manifest -notmatch '"manifest_version": 3') { Write-Error 'manifest_version 3 missing' }
    $version = [regex]::Match($manifest, '"version":\s*"([^"]+)"').Groups[1].Value
    if ($version -ne $Version) { Write-Error ("manifest version mismatch: $version != $Version") }
    Write-Host ("ok: {0} ({1} entries, {2} bytes, key={3}, v{4})" -f $OutPath, $entries.Count, (Get-Item $OutPath).Length, $hasKey, $version)
  } finally {
    $zip.Dispose()
  }
}

$edgeZip = Join-Path $outDir ("absorb-extension-edge-$Version.zip")
$cwsZip = Join-Path $outDir ("absorb-extension-cws-$Version.zip")
New-StoreZip -OutPath $edgeZip -StripKey $true
New-StoreZip -OutPath $cwsZip -StripKey $false
Assert-Zip -OutPath $edgeZip -ExpectKey $false
Assert-Zip -OutPath $cwsZip -ExpectKey $true
