# ProxDeck 构建脚本
# 用法：右键「使用 PowerShell 运行」，或在本目录执行  .\build.ps1
$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $root

# 兜底：winget 安装的 Go 不在当前会话 PATH 里时补上
$goBin = "C:\Program Files\Go\bin"
if (Test-Path $goBin) { $env:Path = "$goBin;" + $env:Path }

$env:GOPROXY = "https://goproxy.cn,direct"

Write-Host "[1/3] 构建前端..." -ForegroundColor Cyan
Push-Location frontend
if (-not (Test-Path node_modules)) {
    npm install --registry=https://registry.npmmirror.com --no-audit --no-fund
}
npm run build
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "前端构建失败" }
Pop-Location

Write-Host "[2/3] 编译 Go 应用..." -ForegroundColor Cyan
New-Item -ItemType Directory -Force -Path bin | Out-Null
go build -ldflags "-s -w -H windowsgui" -o bin\ProxDeck.exe .
if ($LASTEXITCODE -ne 0) { throw "编译失败" }

$size = [math]::Round((Get-Item bin\ProxDeck.exe).Length / 1MB, 1)
Write-Host "[3/3] 完成 -> bin\ProxDeck.exe ($size MB)" -ForegroundColor Green
