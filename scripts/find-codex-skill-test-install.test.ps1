$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'find-codex-skill-test-install.ps1')
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('codex-install-discovery-' + [guid]::NewGuid())
$originalProfile = $env:USERPROFILE
$originalLocal = $env:LOCALAPPDATA
$originalProgramFiles = $env:ProgramFiles
$originalProgramFilesX86 = ${env:ProgramFiles(x86)}

function New-FixtureInstall([string]$Directory, [switch]$WithoutNode) {
    New-Item -ItemType Directory -Path (Join-Path $Directory '_internal/app/resources/cua_node/bin') -Force | Out-Null
    Set-Content -LiteralPath (Join-Path $Directory 'Codex.vbs') -Value 'fixture'
    Set-Content -LiteralPath (Join-Path $Directory '_internal/app/ChatGPT.exe') -Value 'fixture'
    if (-not $WithoutNode) { Set-Content -LiteralPath (Join-Path $Directory '_internal/app/resources/cua_node/bin/node.exe') -Value 'fixture' }
}
function Get-ItemProperty {
    param([string]$LiteralPath, $ErrorAction)
    if ($LiteralPath -eq 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\{A68E32B0-4AA6-4B16-9364-B668731F7062}_is1') {
        return @{ InstallLocation = $script:registeredInstall }
    }
}
function Get-Process {
    param($Id, $Name, $ErrorAction)
    if ($Id) { return @{ SessionId = 42 } }
    if ($script:runningInstall) {
        return @{ SessionId = 42; Path = (Join-Path $script:runningInstall '_internal/app/ChatGPT.exe') }
    }
}
function Assert-Equal($Actual, $Expected) {
    if ($Actual -cne $Expected) { throw "Expected '$Expected'; got '$Actual'." }
}

try {
    $env:USERPROFILE = Join-Path $testRoot 'profile'
    $env:LOCALAPPDATA = Join-Path $testRoot 'local'
    $env:ProgramFiles = Join-Path $testRoot 'programs'
    ${env:ProgramFiles(x86)} = Join-Path $testRoot 'programs-x86'
    $script:registeredInstall = Join-Path $testRoot 'other drive/installed Codex'
    New-FixtureInstall $script:registeredInstall
    New-FixtureInstall (Join-Path $env:USERPROFILE 'Codex') -WithoutNode
    Assert-Equal (Find-CodexSkillTestInstall) $script:registeredInstall
    Write-Output 'PASS: custom registered location wins over an incomplete default folder.'

    $script:registeredInstall = $null
    $script:runningInstall = Join-Path $testRoot 'portable Codex'
    New-FixtureInstall $script:runningInstall
    Assert-Equal (Find-CodexSkillTestInstall) $script:runningInstall
    Write-Output 'PASS: running portable Codex is discovered without registry data.'

    $script:runningInstall = $null
    $localInstall = Join-Path $env:LOCALAPPDATA 'Programs/Codex'
    New-FixtureInstall $localInstall
    Assert-Equal (Find-CodexSkillTestInstall) $localInstall
    Assert-Equal (Find-CodexSkillTestInstall -Directory $localInstall) $localInstall
    Write-Output 'PASS: local Programs and explicit installation directories work.'

    $rejected = $false
    try { Find-CodexSkillTestInstall -Directory (Join-Path $env:USERPROFILE 'Codex') | Out-Null }
    catch { $rejected = $_.Exception.Message -like '*incomplete*' }
    if (-not $rejected) { throw 'An explicit incomplete installation must not silently select another one.' }
    Write-Output 'PASS: explicit incomplete paths report the missing installation files.'
}
finally {
    $env:USERPROFILE = $originalProfile
    $env:LOCALAPPDATA = $originalLocal
    $env:ProgramFiles = $originalProgramFiles
    ${env:ProgramFiles(x86)} = $originalProgramFilesX86
    if (Test-Path -LiteralPath $testRoot) { Remove-Item -LiteralPath $testRoot -Recurse -Force }
}
