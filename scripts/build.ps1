param(
  [string]$Fnpack = "fnpack",
  [switch]$SkipPackage
)

$ErrorActionPreference = "Stop"
$projectRoot = Split-Path -Parent $PSScriptRoot
$appBin = Join-Path $projectRoot "app/bin"
$iconDir = Join-Path $projectRoot "app/ui/images"
$cacheRoot = Join-Path $projectRoot ".cache"

New-Item -ItemType Directory -Force -Path $appBin, $iconDir, (Join-Path $cacheRoot "go-build"), (Join-Path $cacheRoot "go-tmp") | Out-Null

$manifestLine = Get-Content (Join-Path $projectRoot "manifest") | Where-Object { $_ -like "version=*" } | Select-Object -First 1
$version = $manifestLine.Substring("version=".Length).Trim('"')

$oldGoos = $env:GOOS
$oldGoarch = $env:GOARCH
$oldCgo = $env:CGO_ENABLED
$oldCache = $env:GOCACHE
$oldTmp = $env:GOTMPDIR
try {
  $env:GOOS = "linux"
  $env:GOARCH = "amd64"
  $env:CGO_ENABLED = "0"
  $env:GOCACHE = Join-Path $cacheRoot "go-build"
  $env:GOTMPDIR = Join-Path $cacheRoot "go-tmp"
  & go build -trimpath -ldflags "-s -w -X main.version=$version" -o (Join-Path $appBin "tad-module") ./backend/powerguard
  if ($LASTEXITCODE -ne 0) { throw "go build failed" }
  $env:GOOS = $oldGoos
  $env:GOARCH = $oldGoarch
  $env:CGO_ENABLED = $oldCgo
  & go run ./tools/icon-gen -out $iconDir
  if ($LASTEXITCODE -ne 0) { throw "icon generation failed" }
} finally {
  $env:GOOS = $oldGoos
  $env:GOARCH = $oldGoarch
  $env:CGO_ENABLED = $oldCgo
  $env:GOCACHE = $oldCache
  $env:GOTMPDIR = $oldTmp
}

Copy-Item (Join-Path $iconDir "icon_64.png") (Join-Path $projectRoot "ICON.PNG") -Force
Copy-Item (Join-Path $iconDir "icon_256.png") (Join-Path $projectRoot "ICON_256.PNG") -Force

if (-not $SkipPackage) {
  Push-Location $projectRoot
  try {
    $buildStarted = Get-Date
    $packOutput = & $Fnpack build 2>&1
    $packOutput | Write-Output
    if ($LASTEXITCODE -ne 0 -or ($packOutput -join "`n") -match "Packing failed") { throw "fnpack build failed" }
    $package = Get-ChildItem -LiteralPath $projectRoot -Filter "*.fpk" | Where-Object { $_.LastWriteTime -ge $buildStarted.AddSeconds(-2) } | Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if (-not $package) { throw "fnpack reported success but no new .fpk was found" }
  } finally {
    Pop-Location
  }
}
