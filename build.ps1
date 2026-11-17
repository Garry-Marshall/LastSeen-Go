# Builds LastSeen for Windows and Linux into dist\<os>-<arch>\, together with
# locales\ (and, for Linux, install.sh and lastseen.service).
#
#   .\build.ps1                          # windows/amd64 and linux/amd64
#   .\build.ps1 linux/arm64              # other targets (e.g. a Raspberry Pi 4/5)
#
# Go cross-compiles natively: GOOS/GOARCH pick the target and CGO_ENABLED=0
# keeps the binary self-contained (the SQLite driver is pure Go), so the
# Linux binary runs on any distribution without installing anything.

param(
    [string[]]$Targets = @("windows/amd64", "linux/amd64")
)

Set-Location $PSScriptRoot

# Version stamped into the binary (shown by `lastseen -version`).
$version = "dev"
try {
    $v = & git describe --tags --always --dirty 2>$null
    if ($LASTEXITCODE -eq 0 -and $v) { $version = "$v".Trim() }
} catch {}

# GOOS/GOARCH are set for this session only while building; restored after.
$saved = @{ GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED }
$failed = $false
try {
    foreach ($target in $Targets) {
        $os, $arch = $target.Split("/")
        $dir = "dist\$os-$arch"
        $out = "$dir\lastseen"
        if ($os -eq "windows") { $out += ".exe" }
        New-Item -ItemType Directory -Force $dir | Out-Null

        $env:GOOS = $os
        $env:GOARCH = $arch
        $env:CGO_ENABLED = "0"
        Write-Host "Building $out ($version)..."
        go build -trimpath -ldflags "-s -w -X main.version=$version" -o $out ./cmd/lastseen
        if ($LASTEXITCODE -ne 0) {
            Write-Host "Build failed for $target" -ForegroundColor Red
            $failed = $true
            continue
        }

        # Make the folder complete: the bot needs locales\ next to it, and a
        # Linux folder can be copied to a server as-is and installed with
        # install.sh (see README.md, "Deploying on Linux").
        if (Test-Path "$dir\locales") { Remove-Item -Recurse -Force "$dir\locales" }
        Copy-Item -Recurse locales "$dir\locales"
        if ($os -eq "linux") {
            Copy-Item deploy\install.sh, deploy\lastseen.service $dir
        }
    }
} finally {
    foreach ($k in $saved.Keys) {
        if ($null -eq $saved[$k]) {
            Remove-Item "Env:$k" -ErrorAction SilentlyContinue
        } else {
            Set-Item "Env:$k" $saved[$k]
        }
    }
}

if ($failed) { exit 1 }
Write-Host "Done." -ForegroundColor Green
