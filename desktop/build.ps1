# Build WorkBuddy2API desktop shell + embedded server into dist\WorkBuddy2API.exe
# Usage: powershell -ExecutionPolicy Bypass -File desktop\build.ps1
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$go = Join-Path $root ".tools\go\bin\go.exe"
$env:GOPATH = Join-Path $root ".tools\gopath"
$env:GOMODCACHE = Join-Path $env:GOPATH "pkg\mod"
$env:GOPROXY = "https://goproxy.cn,direct"
$env:CGO_ENABLED = "0"
$env:GOTOOLCHAIN = "local"

$dist = Join-Path $root "dist"
New-Item -ItemType Directory -Force -Path $dist | Out-Null

Write-Output "[1/3] build server.exe"
Push-Location (Join-Path $root "repo")
& $go build -trimpath -buildvcs=false -ldflags "-s -w" -o (Join-Path $root "desktop\assets\server.exe") ./cmd/server
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "server build failed" }
Pop-Location

Write-Output "[2/3] embed icon resource"
$ico = Join-Path $root "desktop\assets\app.ico"
$syso = Join-Path $root "desktop\rsrc.syso"
$needIcon = $true
if (Test-Path $syso) {
  $needIcon = (Get-Item $syso).LastWriteTime -lt (Get-Item $ico).LastWriteTime
}
if ($needIcon) {
  $rsrc = Get-Command rsrc -ErrorAction SilentlyContinue
  if ($null -eq $rsrc) {
    & $go install github.com/akavel/rsrc@latest
    $rsrc = Join-Path $env:GOPATH "bin\rsrc.exe"
  } else { $rsrc = $rsrc.Source }
  & $rsrc -ico $ico -o $syso
  if ($LASTEXITCODE -ne 0) { throw "rsrc failed" }
}

Write-Output "[3/3] build desktop shell"
Push-Location (Join-Path $root "desktop")
& $go build -trimpath -buildvcs=false -ldflags "-s -w -H=windowsgui" -o (Join-Path $dist "WorkBuddy2API.exe") .
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "shell build failed" }
Pop-Location

$f = Get-Item (Join-Path $dist "WorkBuddy2API.exe")
Write-Output ("done: {0}  {1:N1} MB" -f $f.FullName, ($f.Length / 1MB))
