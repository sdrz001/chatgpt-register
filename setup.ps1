# 本地 Go 模块隔离环境初始化（等价于 Python venv）
# 用法: .\setup.ps1

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $Root

$mod = Join-Path $Root ".gomodcache"
$cache = Join-Path $Root ".gocache"
New-Item -ItemType Directory -Force -Path $mod, $cache | Out-Null

$env:GOMODCACHE = $mod
$env:GOCACHE = $cache

Write-Host "[setup] Go: $(go version)"
Write-Host "[setup] GOMODCACHE=$env:GOMODCACHE"
Write-Host "[setup] GOCACHE=$env:GOCACHE"
Write-Host "[setup] downloading modules..."
go mod download
go mod verify
Write-Host "[setup] building..."
go build -o chatgpt-register-local.exe .
Write-Host "[setup] done. Run: .\run.ps1"
