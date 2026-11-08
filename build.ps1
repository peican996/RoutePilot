# NexusLink 本地构建脚本（只生成 exe，不打 zip）
# 用法：.\build.ps1                 默认走官方源
#       .\build.ps1 -ChinaMirror    走国内镜像（goproxy.cn / npmmirror）
param([switch]$ChinaMirror)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
Set-Location (Split-Path -Parent $MyInvocation.MyCommand.Path)

# 找 go：优先 PATH，其次常见安装位置。不再硬编码单一路径。
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    foreach ($p in @(
            "C:\Program Files\Go\bin",
            "$env:LOCALAPPDATA\Programs\Go\bin",
            "$env:USERPROFILE\go\bin")) {
        if (Test-Path (Join-Path $p "go.exe")) {
            $env:Path = "$p;" + $env:Path
            break
        }
    }
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "找不到 go。请安装 Go 1.26+，或把它加入 PATH。"
}

# 镜像只在显式指定时启用，避免海外贡献者被指向国内源
$npmArgs = @()
if ($ChinaMirror) {
    $env:GOPROXY = "https://goproxy.cn,direct"
    $npmArgs += "--registry=https://registry.npmmirror.com"
}

# go:embed 要求 assets 下的文件存在，缺了会在这里先说清楚
if (Get-Command python -ErrorAction SilentlyContinue) {
    python scripts/fetch-assets.py
    if ($LASTEXITCODE -ne 0) { throw "内嵌资源不完整，按上面的提示补齐后重试。" }
}

Write-Host "[1/3] 构建前端..." -ForegroundColor Cyan
Push-Location frontend
try {
    if (Test-Path package-lock.json) {
        npm ci @npmArgs --no-audit --no-fund
    } else {
        npm install @npmArgs --no-audit --no-fund
    }
    if ($LASTEXITCODE -ne 0) { throw "依赖安装失败" }
    npm run build
    if ($LASTEXITCODE -ne 0) { throw "前端构建失败" }
} finally {
    Pop-Location
}

Write-Host "[2/3] 编译 Go 应用..." -ForegroundColor Cyan
New-Item -ItemType Directory -Force -Path bin | Out-Null
go build -ldflags "-s -w -H windowsgui" -o bin\NexusLink.exe .
if ($LASTEXITCODE -ne 0) { throw "编译失败" }

Write-Host "[3/3] 完成 -> bin\NexusLink.exe" -ForegroundColor Green
$size = [math]::Round((Get-Item bin\NexusLink.exe).Length / 1MB, 1)
Write-Host "      体积 $size MB" -ForegroundColor Green
