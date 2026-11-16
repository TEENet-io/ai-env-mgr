[CmdletBinding()]
param(
    [string]$CodexInstallDir,
    [switch]$Rollback
)
$ErrorActionPreference = 'Stop'
if ($env:OS -ne 'Windows_NT') { throw 'Run this installer on Windows.' }
. (Join-Path $PSScriptRoot 'find-codex-skill-test-install.ps1')
if (-not $CodexInstallDir) {
    try { $CodexInstallDir = Find-CodexSkillTestInstall }
    catch { $CodexInstallDir = Read-Host 'Enter the Codex installation folder containing Codex.vbs and _internal' }
}
$CodexInstallDir = Find-CodexSkillTestInstall $CodexInstallDir
Write-Host "Codex directory: $CodexInstallDir"
Write-Host 'Stop the OLD test Agent with Ctrl+C, and close Codex once before installing this adapter.'
$nodeExe = Join-Path $CodexInstallDir '_internal/app/resources/cua_node/bin/node.exe'
$installArgs = @((Join-Path $PSScriptRoot 'desktop/install-company-skills-refresh.cjs'), $CodexInstallDir)
if ($Rollback) { $installArgs += '--rollback' }
& $nodeExe @installArgs
if ($LASTEXITCODE -ne 0) { throw 'Live refresh adapter installation failed; see the message above.' }
Write-Host 'Next: run .\agent-skills-test.exe skills-test from this NEW folder, then open Codex Skill Test.'
