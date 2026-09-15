[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repository = Split-Path -Parent $PSScriptRoot
$manifest = Join-Path $repository 'native\computer-windows\Cargo.toml'

cargo build --manifest-path $manifest --release
if ($LASTEXITCODE -ne 0) { throw "cargo build failed with exit code $LASTEXITCODE" }

$targetRoot = if ($env:CARGO_TARGET_DIR) { $env:CARGO_TARGET_DIR } else { Join-Path $repository 'native\computer-windows\target' }
$source = Join-Path $targetRoot 'release\lrmcp-computer-worker.exe'
$destination = Join-Path $repository 'internal\computer\worker_windows_amd64.exe'
Copy-Item -LiteralPath $source -Destination $destination -Force
Write-Host "Built $destination"
