[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('windows', 'linux', 'darwin')]
    [string]$OperatingSystem,

    [Parameter(Mandatory = $true)]
    [ValidateSet('amd64', 'arm64')]
    [string]$Architecture,

    [Parameter(Mandatory = $true)]
    [string]$DestinationDirectory,

    [string]$TemporaryDirectory
)

$ErrorActionPreference = 'Stop'
$version = '2026.9.1'
$artifacts = @{
    'windows/amd64' = @{
        Name = 'cloudflared-windows-amd64.exe'
        SHA256 = '2837888cc0f5d58f15b6dc478376de90b4d3ba5241c7947455d1e0a0df429712'
    }
    'linux/amd64' = @{
        Name = 'cloudflared-linux-amd64'
        SHA256 = '03f1f25d1cc93b9ad6c60569d44060bc4f17ed97075760ed8cfca4b12dcd68cc'
    }
    'linux/arm64' = @{
        Name = 'cloudflared-linux-arm64'
        SHA256 = '3d97437c71848bd8df68041e12436b484a661d95073ea1937f01a845ce88faa3'
    }
    'darwin/amd64' = @{
        Name = 'cloudflared-darwin-amd64.tgz'
        SHA256 = 'ff0d3b51d5ff70eceef89d6b32145fee985018a2174596a5dbe405e2766e2ac4'
    }
    'darwin/arm64' = @{
        Name = 'cloudflared-darwin-arm64.tgz'
        SHA256 = 'c27ab8fd0aa489449e3d201eb02f957ef460a13b613662928b1b23394bf1bcfe'
    }
}

$key = "$OperatingSystem/$Architecture"
$artifact = $artifacts[$key]
if ($null -eq $artifact) {
    throw "cloudflared $version has no pinned artifact for $key"
}

$destination = [System.IO.Path]::GetFullPath($DestinationDirectory)
New-Item -ItemType Directory -Path $destination -Force | Out-Null

$ownsTemporaryDirectory = [string]::IsNullOrWhiteSpace($TemporaryDirectory)
if ($ownsTemporaryDirectory) {
    $TemporaryDirectory = Join-Path ([System.IO.Path]::GetTempPath()) ("lrmcp-cloudflared-" + [guid]::NewGuid().ToString('N'))
}
$temporary = [System.IO.Path]::GetFullPath($TemporaryDirectory)
New-Item -ItemType Directory -Path $temporary -Force | Out-Null

try {
    $download = Join-Path $temporary $artifact.Name
    $url = "https://github.com/cloudflare/cloudflared/releases/download/$version/$($artifact.Name)"
    Invoke-WebRequest -Uri $url -OutFile $download
    $actual = (Get-FileHash -LiteralPath $download -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $artifact.SHA256) {
        throw "cloudflared checksum mismatch for $($artifact.Name): got $actual"
    }

    $executableName = if ($OperatingSystem -eq 'windows') { 'cloudflared.exe' } else { 'cloudflared' }
    $output = Join-Path $destination $executableName
    if ($OperatingSystem -eq 'darwin') {
        $extract = Join-Path $temporary 'extract'
        New-Item -ItemType Directory -Path $extract -Force | Out-Null
        tar -xzf $download -C $extract
        if ($LASTEXITCODE -ne 0) {
            throw 'failed to extract cloudflared archive'
        }
        Copy-Item -LiteralPath (Join-Path $extract 'cloudflared') -Destination $output -Force
    }
    else {
        Copy-Item -LiteralPath $download -Destination $output -Force
    }
    if ($OperatingSystem -ne 'windows') {
        chmod +x $output
        if ($LASTEXITCODE -ne 0) {
            throw 'failed to mark cloudflared executable'
        }
    }
    Write-Output $output
}
finally {
    if ($ownsTemporaryDirectory -and (Test-Path -LiteralPath $temporary)) {
        Remove-Item -LiteralPath $temporary -Recurse -Force
    }
}
