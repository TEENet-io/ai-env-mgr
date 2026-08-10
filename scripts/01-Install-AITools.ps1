#Requires -RunAsAdministrator
<#
.SYNOPSIS
  One-shot AI toolchain init for a shared Windows machine (single file, all init steps):
    Part 1 (CLI, shared):        Codex CLI + Claude Code native single-exe -> C:\Tools + machine PATH.
    Part 2 (GUI, provisioned):   ChatGPT desktop App (Chat/Work/Codex GUI). Auto-downloads the official
                                 offline package (or use -ChatGptMsix), provisions for ALL future users,
                                 and installs for the current user.
    Part 3 (GUI, existing users): for existing users -- named (-EnsureGuiForUsers) or all of them
                                 (-AllExistingUsers) -- if the GUI is missing, schedule an auto-install
                                 from the local msix on their next logon (MSIX is per-user; cannot register
                                 for another user immediately).
  Credentials stay per-profile: each user logs in once.

.PARAMETER ToolsDir           Shared dir for CLI binaries + staged msix (default C:\Tools).
.PARAMETER ChatGptMsix        Optional: path to an already-downloaded ChatGPT-*.msix. If omitted, auto-download.
.PARAMETER ChatGptLicense     Optional: path to ChatGPT-License.xml (pairs with -ChatGptMsix).
.PARAMETER EnsureGuiForUsers  Existing usernames to ensure the GUI for (Part 3) -- installs from the local msix.
.PARAMETER AllExistingUsers   Ensure the GUI for EVERY real user already under C:\Users (Part 3), no naming
                              needed. Use this when installing on a machine that already has employee profiles.
.PARAMETER SkipCli            Skip Part 1.
.PARAMETER SkipGui            Skip Part 2 (all-user provision + current-user install). Part 3 still runs if requested.

.EXAMPLE
  # Fully automatic: install CLI, auto-download + provision GUI, and cover two existing users
  .\Setup-AICli.ps1 -EnsureGuiForUsers work1,work2

.EXAMPLE
  # Install everything for ALL users -- CLI (machine-wide), GUI provisioned for future users,
  # and the GUI scheduled for every profile already on the machine (no naming needed)
  .\Setup-AICli.ps1 -AllExistingUsers

.EXAMPLE
  # CLI only
  .\Setup-AICli.ps1 -SkipGui

.EXAMPLE
  # After first run (msix already in C:\Tools): just add the GUI for one more existing user
  .\Setup-AICli.ps1 -SkipCli -SkipGui -EnsureGuiForUsers newguy

.NOTES
  Run as Administrator. Auto-download source (official, no auth, ~759 MB):
    https://persistent.oaistatic.com/codex-app-prod/ChatGPT-x64.msix (+ arm64, + ChatGPT-License.xml)
  ChatGPT App package identity is OpenAI.Codex (display name "ChatGPT") -- detection uses the identity.
  NOT yet tested on a real Windows host -- validate on a test VDI before production.
#>

param(
    [string]$ToolsDir = "C:\Tools",
    [string]$ChatGptMsix = "",
    [string]$ChatGptLicense = "",
    [string[]]$EnsureGuiForUsers = @(),
    [switch]$AllExistingUsers,
    [switch]$SkipCli,
    [switch]$SkipGui,
    [switch]$Force
)

[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

# ---- -AllExistingUsers: cover every real profile already on the machine ----
# The CLI tools are machine-wide already; this is only about the ChatGPT GUI,
# which is per-user (MSIX). Provisioning covers all FUTURE users, but profiles
# that already exist need Part 3. -AllExistingUsers fills EnsureGuiForUsers with
# every real user under C:\Users so the admin need not name them one by one.
function Get-RealUserProfiles {
    # Profiles that belong to a person, not to Windows or to tooling. Mirrors
    # the agent's own profile filter (isSystemProfile) so the two agree on who
    # counts as a real user.
    $skip = @('public', 'default', 'default user', 'all users', 'wdagutilityaccount',
        'defaultapppool', 'systemprofile', 'localservice', 'networkservice')
    $usersRoot = Join-Path $env:SystemDrive 'Users'
    Get-ChildItem $usersRoot -Directory -ErrorAction SilentlyContinue | Where-Object {
        $n = $_.Name.ToLower()
        ($skip -notcontains $n) -and (-not $n.StartsWith('codexsandbox')) -and (-not $n.StartsWith('.'))
    } | Select-Object -ExpandProperty Name
}

if ($AllExistingUsers) {
    $found = @(Get-RealUserProfiles)
    Write-Host "  [info] -AllExistingUsers: $($found.Count) existing profile(s): $($found -join ', ')" -ForegroundColor DarkCyan
    $EnsureGuiForUsers = @($EnsureGuiForUsers + $found | Select-Object -Unique)
}

# ============================ Part 1: shared CLI binaries ============================
if (-not $SkipCli) {
    Write-Host "=== Part 1: shared CLI (Codex CLI + Claude Code) -> $ToolsDir ===" -ForegroundColor Cyan
    New-Item -ItemType Directory -Force -Path $ToolsDir | Out-Null

    # Codex CLI -- skip if already in ToolsDir (use -Force to re-download)
    $codexDst = Join-Path $ToolsDir "codex.exe"
    if ((Test-Path $codexDst) -and (-not $Force)) {
        Write-Host "  [skip] codex.exe already in $ToolsDir (use -Force to re-download)" -ForegroundColor DarkYellow
    } else {
        Write-Host "  Installing Codex CLI (native exe)..." -ForegroundColor White
        try {
            $zip = Join-Path $env:TEMP "codex.zip"
            Invoke-WebRequest -Uri "https://github.com/openai/codex/releases/latest/download/codex-x86_64-pc-windows-msvc.exe.zip" -OutFile $zip -UseBasicParsing
            $exd = Join-Path $env:TEMP "codex_extract"; Remove-Item $exd -Recurse -Force -ErrorAction SilentlyContinue
            Expand-Archive -Path $zip -DestinationPath $exd -Force
            $exe = Get-ChildItem $exd -Recurse -Filter "codex*.exe" | Select-Object -First 1
            if ($exe) { Copy-Item $exe.FullName $codexDst -Force; Write-Host "  [ok] codex.exe -> $ToolsDir" -ForegroundColor Green }
            else { Write-Host "  [warn] codex exe not found in archive" -ForegroundColor Yellow }
            Remove-Item $zip, $exd -Recurse -Force -ErrorAction SilentlyContinue
        } catch { Write-Host "  [err] Codex CLI install failed: $_" -ForegroundColor Red }
    }

    # Claude Code -- native installer (no winget / no Node). Skip if already staged.
    # The winget path is unreliable on WuYing / Windows Server images that ship
    # without App Installer, so the official native installer is primary and
    # winget is only a fallback when it happens to be present.
    $claudeDst = Join-Path $ToolsDir "claude.exe"
    if ((Test-Path $claudeDst) -and (-not $Force)) {
        Write-Host "  [skip] claude.exe already in $ToolsDir (use -Force to reinstall)" -ForegroundColor DarkYellow
    } else {
        # Where the installers drop claude.exe. Prefer the real versioned native
        # binary under .local\share\...\versions (self-contained, so it still runs
        # when copied to C:\Tools for OTHER users) over the .local\bin launcher,
        # which may just point back into the installing user's profile.
        $findClaude = {
            $c = $null
            $v = Get-ChildItem "$env:USERPROFILE\.local\share\claude\versions" -Filter "claude*.exe" -Recurse -ErrorAction SilentlyContinue |
                Sort-Object LastWriteTime -Descending | Select-Object -First 1
            if ($v) { $c = $v.FullName }
            if (-not $c) {
                $c = @("$env:USERPROFILE\.local\bin\claude.exe", "$env:LOCALAPPDATA\Programs\claude\claude.exe") |
                    Where-Object { Test-Path $_ } | Select-Object -First 1
            }
            if (-not $c) { $g = Get-Command claude -ErrorAction SilentlyContinue; if ($g) { $c = $g.Source } }
            $c
        }

        Write-Host "  Installing Claude Code (official native installer)..." -ForegroundColor White
        try { Invoke-Expression (Invoke-RestMethod -Uri 'https://claude.ai/install.ps1') }
        catch { Write-Host "  [warn] native installer failed: $_" -ForegroundColor Yellow }

        $found = & $findClaude
        if (-not $found -and (Get-Command winget -ErrorAction SilentlyContinue)) {
            Write-Host "  native installer did not place claude.exe; trying winget..." -ForegroundColor White
            try { winget install --id Anthropic.ClaudeCode --accept-source-agreements --accept-package-agreements --silent } catch {}
            $found = & $findClaude
        }

        if ($found) {
            Copy-Item $found $claudeDst -Force
            Write-Host "  [ok] claude.exe -> $ToolsDir  (from $found)" -ForegroundColor Green
        } else {
            Write-Host "  [err] Claude installed but claude.exe not located. Last resort: install Node.js 22+, then 'npm i -g @anthropic-ai/claude-code'." -ForegroundColor Red
        }
    }

    $p = [Environment]::GetEnvironmentVariable('Path','Machine')
    if ($p -notlike "*$ToolsDir*") { [Environment]::SetEnvironmentVariable('Path', ($p.TrimEnd(';') + ";$ToolsDir"), 'Machine'); Write-Host "  [ok] added $ToolsDir to machine PATH" -ForegroundColor Green }
    else { Write-Host "  [skip] $ToolsDir already in machine PATH" -ForegroundColor DarkYellow }
    [Environment]::SetEnvironmentVariable('DISABLE_AUTOUPDATER','1','Machine')
    Write-Host "  [ok] DISABLE_AUTOUPDATER=1 (machine)" -ForegroundColor Green
}

# ============ Resolve ChatGPT App package (reuse local, or auto-download) ============
$needGui = (-not $SkipGui) -or ($EnsureGuiForUsers.Count -gt 0)
$sharedMsix = $null; $sharedLic = $null
if ($needGui) {
    New-Item -ItemType Directory -Force -Path $ToolsDir | Out-Null
    if ($ChatGptMsix -and (Test-Path $ChatGptMsix)) {
        $sharedMsix = Join-Path $ToolsDir (Split-Path $ChatGptMsix -Leaf)
        Copy-Item $ChatGptMsix $sharedMsix -Force
        if ($ChatGptLicense -and (Test-Path $ChatGptLicense)) { $sharedLic = Join-Path $ToolsDir "ChatGPT-License.xml"; Copy-Item $ChatGptLicense $sharedLic -Force }
    }
    elseif (Test-Path (Join-Path $ToolsDir "ChatGPT-x64.msix"))   { $sharedMsix = Join-Path $ToolsDir "ChatGPT-x64.msix";   $sharedLic = Join-Path $ToolsDir "ChatGPT-License.xml"; Write-Host "  [info] reusing local msix in $ToolsDir" -ForegroundColor DarkYellow }
    elseif (Test-Path (Join-Path $ToolsDir "ChatGPT-arm64.msix")) { $sharedMsix = Join-Path $ToolsDir "ChatGPT-arm64.msix"; $sharedLic = Join-Path $ToolsDir "ChatGPT-License.xml"; Write-Host "  [info] reusing local msix in $ToolsDir" -ForegroundColor DarkYellow }
    else {
        Write-Host ""
        Write-Host "=== Downloading ChatGPT App from official CDN (~759 MB, please wait) ===" -ForegroundColor Cyan
        $base = "https://persistent.oaistatic.com/codex-app-prod"
        $arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "x64" }
        $sharedMsix = Join-Path $ToolsDir "ChatGPT-$arch.msix"
        $sharedLic  = Join-Path $ToolsDir "ChatGPT-License.xml"
        $hdr = @{ "User-Agent" = "Mozilla/5.0" }
        try {
            Invoke-WebRequest "$base/ChatGPT-$arch.msix"   -OutFile $sharedMsix -UseBasicParsing -Headers $hdr
            Invoke-WebRequest "$base/ChatGPT-License.xml"  -OutFile $sharedLic  -UseBasicParsing -Headers $hdr
            Write-Host "  [ok] downloaded $arch package + license -> $ToolsDir" -ForegroundColor Green
        } catch { Write-Host "  [err] download failed: $_" -ForegroundColor Red; $sharedMsix = $null }
    }
    if ($sharedMsix -and (Test-Path $sharedMsix)) { icacls "$sharedMsix" /grant "*S-1-5-32-545:(RX)" /C | Out-Null }  # Users: read/execute
}

# ================== Part 2: all-user provision + current-user install ==================
if (-not $SkipGui) {
    Write-Host ""
    Write-Host "=== Part 2: ChatGPT desktop App (GUI with Codex) ===" -ForegroundColor Cyan
    $already = Get-AppxPackage -Name "OpenAI.Codex" -ErrorAction SilentlyContinue
    if ($already) { Write-Host "  [info] already installed for current user: $($already.PackageFullName)" -ForegroundColor DarkYellow }

    if ($sharedMsix -and (Test-Path $sharedMsix) -and $sharedLic -and (Test-Path $sharedLic)) {
        Write-Host "  Provisioning for all users..." -ForegroundColor White
        try { Add-AppxProvisionedPackage -Online -PackagePath $sharedMsix -LicensePath $sharedLic -ErrorAction Stop | Out-Null
              Write-Host "  [ok] provisioned -> every NEW user auto-installs on first logon" -ForegroundColor Green }
        catch { Write-Host "  [err] provision failed: $_" -ForegroundColor Red }
        try { Add-AppxPackage -Path $sharedMsix -ErrorAction Stop; Write-Host "  [ok] installed for current user" -ForegroundColor Green }
        catch { Write-Host "  [warn] current-user install: $_" -ForegroundColor Yellow }
    } else {
        Write-Host "  [err] no msix+license available (download failed or none provided); skipping provision." -ForegroundColor Red
    }
}

# ============ Part 3: ensure GUI for named EXISTING users (queue for next logon) ============
if ($EnsureGuiForUsers.Count -gt 0) {
    Write-Host ""
    Write-Host "=== Part 3: ensure GUI for existing users ===" -ForegroundColor Cyan
    if (-not ($sharedMsix -and (Test-Path $sharedMsix))) {
        Write-Host "  [err] no msix available in $ToolsDir; run once without -SkipGui first to download it." -ForegroundColor Red
    } else {
        foreach ($u in $EnsureGuiForUsers) {
            Write-Host "  --- $u ---" -ForegroundColor White
            $has = $null
            try { $has = Get-AppxPackage -User $u -Name "OpenAI.Codex" -ErrorAction SilentlyContinue } catch {}
            if ($has) { Write-Host "  [skip] $u already has the GUI" -ForegroundColor DarkYellow; continue }
            $taskName = "InstallChatGPT_$u"
            $cmd = "Add-AppxPackage -Path '$sharedMsix'; Unregister-ScheduledTask -TaskName '$taskName' -Confirm:`$false"
            try {
                $action    = New-ScheduledTaskAction -Execute "powershell.exe" -Argument "-WindowStyle Hidden -ExecutionPolicy Bypass -Command `"$cmd`""
                $trigger   = New-ScheduledTaskTrigger -AtLogOn -User $u
                $principal = New-ScheduledTaskPrincipal -UserId $u -LogonType Interactive -RunLevel Limited
                Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Force | Out-Null
                Write-Host "  [ok] $u will auto-install the GUI on next logon (from local msix)" -ForegroundColor Green
            } catch { Write-Host "  [err] could not schedule for $u : $_" -ForegroundColor Red }
        }
    }
}

Write-Host ""
Write-Host "All done." -ForegroundColor Green
Write-Host "NOTE: machine PATH change needs a NEW login/session for the CLI." -ForegroundColor Yellow
Write-Host "Each user logs in once -- CLI: 'codex' / 'claude';  GUI: open ChatGPT app. Credentials per-profile." -ForegroundColor Yellow
