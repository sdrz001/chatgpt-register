# 激活当前项目的 Go 本地隔离环境（写入当前 PowerShell 会话）
# 用法: . .\activate.ps1

$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
$env:GOMODCACHE = Join-Path $Root ".gomodcache"
$env:GOCACHE = Join-Path $Root ".gocache"
if (-not $env:ADDR) { $env:ADDR = "9000" }

Write-Host "Local Go env activated:"
Write-Host "  GOMODCACHE=$env:GOMODCACHE"
Write-Host "  GOCACHE=$env:GOCACHE"
Write-Host "  ADDR=$env:ADDR"
Write-Host ""
Write-Host "Commands:"
Write-Host "  go run .                          # 源码运行"
Write-Host "  go build -o chatgpt-register-local.exe ."
Write-Host "  .\run.ps1                         # 启动服务"
