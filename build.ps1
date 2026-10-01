# build.ps1 —— 打包 onerclone 成品（Windows x64）
#
# 用法:
#   .\build.ps1                      测试 + 构建 + 打包
#   .\build.ps1 -Version 0.3.0       指定版本
#   .\build.ps1 -SkipTests           跳过测试（仅快速出包）
#   .\build.ps1 -Installer           额外用 Inno Setup 编译安装包
#
# 产物:
#   dist\onerclone-v<版本>-win64\          发布目录（exe + rclone + README + VERSION）
#   dist\onerclone-v<版本>-win64.zip      压缩包（可直接分发）
#   dist\onerclone-setup-<版本>.exe       安装包（需 -Installer + Inno Setup 6）

param(
    [string]$Version = "0.3.6",
    [switch]$SkipTests,
    [switch]$Installer
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $root

$buildDate = Get-Date -Format 'yyyy-MM-dd'
$ldflags = "-s -w -X main.version=v$Version -X main.buildDate=$buildDate"

if (-not $SkipTests) {
    Write-Host '== 1/4 测试 ==' -ForegroundColor Cyan
    go test ./... -count=1
    if ($LASTEXITCODE -ne 0) { throw '测试失败，中止打包' }
}

Write-Host '== 2/4 构建 onerclone.exe ==' -ForegroundColor Cyan
$stage = Join-Path $root "dist\onerclone-v$Version-win64"
Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $stage | Out-Null

$exePath = Join-Path $stage 'onerclone.exe'
go build -trimpath -ldflags $ldflags -o $exePath ./cmd/spike
if ($LASTEXITCODE -ne 0) { throw '构建失败' }
$exe = Get-Item $exePath
Write-Host ("  onerclone.exe  {0:N1} MB" -f ($exe.Length / 1MB))

Write-Host '== 3/4 组装发布目录 ==' -ForegroundColor Cyan
# 夸克专用 rclone fork（成品必需；放在 exe 同目录可被自动发现）
$rcloneCandidates = @('D:\Tools\rclone\rclone.exe', 'D:\Software\rclone\rclone.exe', 'D:\Tools\onerclone\rclone.exe')
$rcloneSrc = $rcloneCandidates | Where-Object { Test-Path $_ } | Select-Object -First 1
if ($rcloneSrc) {
    Copy-Item $rcloneSrc (Join-Path $stage 'rclone.exe')
    $rc = Get-Item (Join-Path $stage 'rclone.exe')
    Write-Host ("  + rclone.exe  {0:N1} MB  ← {1}" -f ($rc.Length / 1MB), $rcloneSrc)
} else {
    Write-Warning '  未找到 rclone.exe（D:\Tools 或 D:\Software）—— 发布包不含 rclone（使用者需自备夸克 fork）'
}

Copy-Item (Join-Path $root 'README.md') (Join-Path $stage 'README.md')
$rcloneIncluded = if (Test-Path (Join-Path $stage 'rclone.exe')) { 'included (quark fork)' } else { 'not included' }
@"
onerclone v$Version
build   $buildDate
exe     $([math]::Round($exe.Length / 1MB, 1)) MB
rclone  $rcloneIncluded
"@ | Set-Content (Join-Path $stage 'VERSION.txt') -Encoding UTF8

# 版本自检（证明 ldflags 注入成功）
$ver = & $exePath version
Write-Host "  $ver"
# 防御：即使版本命令意外写了日志，也不让它进发布包
Remove-Item (Join-Path $stage 'onerclone.log') -Force -ErrorAction SilentlyContinue
Remove-Item (Join-Path $stage 'onerclone.json') -Force -ErrorAction SilentlyContinue

Write-Host '== 4/4 压缩 ==' -ForegroundColor Cyan
$zip = Join-Path $root "dist\onerclone-v$Version-win64.zip"
Remove-Item $zip -Force -ErrorAction SilentlyContinue
Compress-Archive -Path $stage -DestinationPath $zip -CompressionLevel Optimal
$z = Get-Item $zip
Write-Host ("  {0}  {1:N1} MB" -f $z.Name, ($z.Length / 1MB))

if ($Installer) {
    Write-Host '== 5/5 编译安装包（Inno Setup） ==' -ForegroundColor Cyan
    $iscc = $null
    $cmd = Get-Command ISCC.exe -ErrorAction SilentlyContinue
    if ($cmd) { $iscc = $cmd.Source }
    if (-not $iscc) {
        $candidates = @(
            "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe",
            "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe",
            "$env:ProgramFiles\Inno Setup 6\ISCC.exe"
        )
        $iscc = $candidates | Where-Object { Test-Path $_ } | Select-Object -First 1
    }
    if (-not $iscc) {
        Write-Warning '未找到 ISCC.exe，跳过安装包。安装：winget install --id JRSoftware.InnoSetup --scope user'
    } else {
        Write-Host "  ISCC: $iscc"
        & $iscc "/DMyAppVersion=$Version" (Join-Path $root 'installer.iss')
        if ($LASTEXITCODE -ne 0) { throw 'ISCC 编译失败' }
        $setup = Join-Path $root "dist\onerclone-setup-$Version.exe"
        if (Test-Path $setup) {
            $s = Get-Item $setup
            Write-Host ("  {0}  {1:N1} MB" -f $s.Name, ($s.Length / 1MB))
        }
    }
}

Write-Host ''
Write-Host "✅ 完成: $zip" -ForegroundColor Green
