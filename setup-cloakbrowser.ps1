$ErrorActionPreference = "Stop"
$ProjectRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$VenvPath = Join-Path $ProjectRoot ".venv"
$Requirements = Join-Path $ProjectRoot "requirements-cloakbrowser.txt"
$Python = Join-Path $VenvPath "Scripts\python.exe"

Write-Host "[CloakBrowser] Project: $ProjectRoot"
if (-not (Test-Path $Python)) {
    Write-Host "[CloakBrowser] Creating Python 3.10+ environment at $VenvPath"
    py -3 -m venv $VenvPath
} else {
    Write-Host "[CloakBrowser] Reusing environment at $VenvPath"
}

$Version = & $Python -c "import sys; print('.'.join(map(str, sys.version_info[:3]))); raise SystemExit(sys.version_info < (3, 10))"
if ($LASTEXITCODE -ne 0) {
    throw "The .venv interpreter must be Python 3.10 or newer."
}
Write-Host "[CloakBrowser] Python $Version"
Write-Host "[CloakBrowser] Installing pinned dependencies from $Requirements"
& $Python -m pip install --upgrade pip
& $Python -m pip install -r $Requirements
if ($LASTEXITCODE -ne 0) {
    throw "Dependency installation failed."
}

Write-Host "[CloakBrowser] Installation complete."
Write-Host "[CloakBrowser] Health check: $Python internal\codexreg\cloakbrowser_sidecar.py --health"
