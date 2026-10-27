[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$env:DOTNET_CLI_HOME = Join-Path $root '.dotnet'
$env:NUGET_PACKAGES = Join-Path $root '.packages'
$env:NUGET_HTTP_CACHE_PATH = Join-Path $root '.nuget-http-cache'
$env:TEMP = Join-Path $root '.tmp'
$env:TMP = $env:TEMP
$env:DOTNET_CLI_TELEMETRY_OPTOUT = '1'
$env:DOTNET_NOLOGO = '1'

@($env:DOTNET_CLI_HOME, $env:NUGET_PACKAGES, $env:NUGET_HTTP_CACHE_PATH, $env:TEMP) |
    ForEach-Object { New-Item -ItemType Directory -Path $_ -Force | Out-Null }

Push-Location $root
try {
    dotnet restore .\DoriyaMedia.WinUISpike.sln --configfile .\NuGet.Config
    if ($LASTEXITCODE -ne 0) { throw "dotnet restore failed with exit code $LASTEXITCODE" }

    dotnet build .\DoriyaMedia.WinUISpike.sln -c Debug --no-restore
    if ($LASTEXITCODE -ne 0) { throw "dotnet build failed with exit code $LASTEXITCODE" }

    dotnet test .\tests\DoriyaMedia.WinUISpike.Core.Tests\DoriyaMedia.WinUISpike.Core.Tests.csproj `
        -c Debug --arch x64 --no-build --no-restore
    if ($LASTEXITCODE -ne 0) { throw "dotnet test failed with exit code $LASTEXITCODE" }

    dotnet run --project .\tools\DoriyaMedia.WinUISpike.VlcProbe\DoriyaMedia.WinUISpike.VlcProbe.csproj `
        -c Debug --no-build --no-restore
    if ($LASTEXITCODE -ne 0) { throw "LibVLC probe failed with exit code $LASTEXITCODE" }
}
finally {
    Pop-Location
}
