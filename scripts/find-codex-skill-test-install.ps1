function Test-CodexSkillTestInstall {
    param([string]$Directory)
    if (-not $Directory) { return $false }
    foreach ($relative in @('Codex.vbs', '_internal/app/ChatGPT.exe', '_internal/app/resources/cua_node/bin/node.exe')) {
        if (-not (Test-Path -LiteralPath (Join-Path $Directory $relative) -PathType Leaf)) { return $false }
    }
    return $true
}

function Get-CodexSkillTestCandidates {
    # The installer reuses a previous location, even on a different drive.
    $appIds = @('{A68E32B0-4AA6-4B16-9364-B668731F7062}_is1', '{9B209189-3509-4596-B79C-28A83CF2DD09}_is1')
    $registryRoots = @(
        'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall',
        'HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall',
        'HKCU:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall',
        'HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall'
    )
    foreach ($appId in $appIds) {
        foreach ($root in $registryRoots) {
            $entry = Get-ItemProperty -LiteralPath "$root\$appId" -ErrorAction SilentlyContinue
            if ($entry.InstallLocation) { $entry.InstallLocation }
        }
    }
    # Portable installations may have no uninstall registration.
    $sessionId = (Get-Process -Id $PID).SessionId
    Get-Process -Name ChatGPT -ErrorAction SilentlyContinue | Where-Object {
        $_.SessionId -eq $sessionId -and $_.Path -match '[\\/]_internal[\\/]app[\\/]ChatGPT\.exe$'
    } | ForEach-Object { $_.Path -replace '[\\/]_internal[\\/]app[\\/]ChatGPT\.exe$', '' }
    foreach ($base in @($env:USERPROFILE, $env:LOCALAPPDATA, $env:ProgramFiles, ${env:ProgramFiles(x86)})) {
        if ($base) { Join-Path $base 'Codex' }
    }
    if ($env:LOCALAPPDATA) { Join-Path $env:LOCALAPPDATA 'Programs/Codex' }
    if ($env:SystemDrive) { Join-Path ($env:SystemDrive + '\') 'Tools/Codex' }
}

function Find-CodexSkillTestInstall {
    param([string]$Directory)
    if ($Directory) {
        if (-not (Test-CodexSkillTestInstall $Directory)) {
            throw "The supplied Codex directory is incomplete: $Directory. Expected Codex.vbs, _internal/app/ChatGPT.exe and _internal/app/resources/cua_node/bin/node.exe."
        }
        return (Resolve-Path -LiteralPath $Directory).ProviderPath
    }
    foreach ($candidate in @(Get-CodexSkillTestCandidates | Select-Object -Unique)) {
        if (Test-CodexSkillTestInstall $candidate) {
            return (Resolve-Path -LiteralPath $candidate).ProviderPath
        }
    }
    throw 'Cannot locate the installed Codex bundle. Run again with -CodexInstallDir "D:\your\Codex" (the directory containing Codex.vbs and _internal).'
}
