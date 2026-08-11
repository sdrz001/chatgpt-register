# 在本地隔离环境中启动服务
# 用法:
#   .\run.ps1              # 默认 :9000
#   .\run.ps1 -Addr 8080   # 自定义端口

param(
    [string]$Addr = "9000"
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $Root

$env:GOMODCACHE = Join-Path $Root ".gomodcache"
$env:GOCACHE = Join-Path $Root ".gocache"
$env:ADDR = $Addr

if (-not (Test-Path (Join-Path $Root ".gomodcache"))) {
    Write-Host "[run] local env missing, running setup..."
    & (Join-Path $Root "setup.ps1")
}

$exe = Join-Path $Root "chatgpt-register-local.exe"
if (-not (Test-Path $exe)) {
    Write-Host "[run] building binary..."
    go build -o $exe .
}

Write-Host "[run] starting http://localhost:$Addr"
Write-Host "[run] default login: admin / admin123"
& $exe
