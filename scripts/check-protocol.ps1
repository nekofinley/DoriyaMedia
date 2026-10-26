[CmdletBinding()]
param(
    [string]$RepoRoot
)

$ErrorActionPreference = 'Stop'

if ([string]::IsNullOrWhiteSpace($RepoRoot)) {
    $RepoRoot = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
} else {
    $RepoRoot = (Resolve-Path -LiteralPath $RepoRoot).Path
}

$helperDir = Join-Path $PSScriptRoot 'protocolcheck'
if (-not (Test-Path -LiteralPath (Join-Path $helperDir 'go.mod') -PathType Leaf)) {
    throw "Protocol checker module not found at $helperDir"
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'Go is required to run the protocol checker.'
}

Push-Location -LiteralPath $helperDir
try {
    & go run -mod=readonly . -root $RepoRoot
    if ($LASTEXITCODE -ne 0) {
        throw "Protocol checker exited with code $LASTEXITCODE"
    }
} finally {
    Pop-Location
}
