[CmdletBinding()]
param(
    [string]$CodexInstallDir,
    [switch]$NoLaunch
)
$ErrorActionPreference = 'Stop'
if ($env:OS -ne 'Windows_NT') { throw 'Run this setup on Windows.' }
. (Join-Path $PSScriptRoot 'find-codex-skill-test-install.ps1')
$CodexInstallDir = Find-CodexSkillTestInstall $CodexInstallDir
Write-Host "Found Codex: $CodexInstallDir"
$nodeExe = Join-Path $CodexInstallDir '_internal/app/resources/cua_node/bin/node.exe'
# Use the bundled runtime, as b13 setup does, including in ConstrainedLanguage.
$setupArgs = @((Join-Path $PSScriptRoot 'setup-codex-skill-test.cjs'), $CodexInstallDir)
if ($NoLaunch) { $setupArgs += '--no-launch' }
& $nodeExe @setupArgs
if ($LASTEXITCODE -ne 0) { throw 'Codex skill test setup failed. See the message above.' }
