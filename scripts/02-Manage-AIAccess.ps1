#Requires -RunAsAdministrator
<#
.SYNOPSIS
  Control browser access to AI sites + AppLocker hardening, in one script.

.DESCRIPTION
  Two layers, managed together:
    1. Site block  -- Chrome/Edge URLBlocklist + Firefox WebsiteFilter (HKLM = machine-wide).
                      Standard (non-admin) users cannot lift it themselves.
    2. AppLocker   -- allowlist executables so a standard user cannot bypass the block with a
                      portable browser or by editing the registry.
  The blocked-domain list is stored in HKLM\SOFTWARE\AIAccessPolicy so -AddSite really appends
  (it does not overwrite what you added before).

.PARAMETER Init        Apply everything: site block + AppLocker hardening (audit mode by default).
.PARAMETER Enforce     With -Init: put AppLocker in ENFORCE mode instead of audit.
.PARAMETER Unblock     Temporarily lift the site block (AppLocker untouched). Admin-only.
.PARAMETER Block       Re-apply the site block.
.PARAMETER Reset       Undo everything: remove site block AND clear AppLocker policy.
.PARAMETER Status      Show current state and exit.
.PARAMETER AddSite     Append one or more domains to the block list and re-apply.
.PARAMETER RemoveSite  Remove one or more domains from the block list and re-apply.
.PARAMETER ListSites   List the currently configured domains.
.PARAMETER SkipAppLocker  With -Init: only do the site block.

.EXAMPLE
  .\AIAccess.ps1 -Init                       # block sites + AppLocker (audit)
  .\AIAccess.ps1 -Init -Enforce              # block sites + AppLocker (enforce)
  .\AIAccess.ps1 -Status
  .\AIAccess.ps1 -Unblock                    # admin needs access for a while
  .\AIAccess.ps1 -Block                      # restore
  .\AIAccess.ps1 -AddSite gemini.google.com,x.ai
  .\AIAccess.ps1 -RemoveSite x.ai
  .\AIAccess.ps1 -Reset                      # full rollback

.NOTES
  Run as Administrator. Restart browsers (or reload policies) after block changes.
  AppLocker needs Windows Enterprise/Education or Server. Validate in audit mode before -Enforce.
#>

param(
    [switch]$Init,
    [switch]$Enforce,
    [switch]$Unblock,
    [switch]$Block,
    [switch]$Reset,
    [switch]$Status,
    [string[]]$AddSite = @(),
    [string[]]$RemoveSite = @(),
    [switch]$ListSites,
    [switch]$SkipAppLocker
)

# ===================== configuration =====================
$StoreKey    = 'HKLM:\SOFTWARE\AIAccessPolicy'
$ChromePath  = 'HKLM:\SOFTWARE\Policies\Google\Chrome\URLBlocklist'
$EdgePath    = 'HKLM:\SOFTWARE\Policies\Microsoft\Edge\URLBlocklist'
$FirefoxPath = 'HKLM:\SOFTWARE\Policies\Mozilla\Firefox\WebsiteFilter\Block'

$DefaultDomains = @('openai.com','chatgpt.com','claude.ai','anthropic.com')

$AdminsSid   = 'S-1-5-32-544'
$EveryoneSid = 'S-1-1-0'

# ===================== domain list store =====================
function Get-Domains {
    if (-not (Test-Path $StoreKey)) { return $DefaultDomains }
    $v = (Get-ItemProperty -Path $StoreKey -Name Domains -ErrorAction SilentlyContinue).Domains
    if ($v) { return @($v) } else { return $DefaultDomains }
}

function Save-Domains([string[]]$list) {
    if (-not (Test-Path $StoreKey)) { New-Item -Path $StoreKey -Force | Out-Null }
    $clean = $list | Where-Object { $_ } | ForEach-Object { $_.Trim().ToLower() } | Sort-Object -Unique
    New-ItemProperty -Path $StoreKey -Name Domains -Value $clean -PropertyType MultiString -Force | Out-Null
    return $clean
}

# ===================== site block =====================
function Set-SiteBlock {
    $domains = Get-Domains
    foreach ($e in @(@{N='Google Chrome';P=$ChromePath}, @{N='Microsoft Edge';P=$EdgePath})) {
        if (Test-Path $e.P) { Remove-Item -Path $e.P -Recurse -Force }
        New-Item -Path $e.P -Force | Out-Null
        $i = 1
        foreach ($d in $domains) { New-ItemProperty -Path $e.P -Name "$i" -Value $d -PropertyType String -Force | Out-Null; $i++ }
        Write-Host ("  [ok] {0}: {1} rule(s)" -f $e.N, ($i - 1)) -ForegroundColor Green
    }
    if (Test-Path $FirefoxPath) { Remove-Item -Path $FirefoxPath -Recurse -Force }
    New-Item -Path $FirefoxPath -Force | Out-Null
    $i = 1
    foreach ($d in $domains) {
        foreach ($p in @("*://$d/*", "*://*.$d/*")) { New-ItemProperty -Path $FirefoxPath -Name "$i" -Value $p -PropertyType String -Force | Out-Null; $i++ }
    }
    Write-Host ("  [ok] Firefox: {0} rule(s)" -f ($i - 1)) -ForegroundColor Green
    Write-Host ("  Domains: {0}" -f ($domains -join ', ')) -ForegroundColor Gray
}

function Remove-SiteBlock {
    foreach ($e in @(@{N='Chrome';P=$ChromePath}, @{N='Edge';P=$EdgePath}, @{N='Firefox';P=$FirefoxPath})) {
        if (Test-Path $e.P) { Remove-Item -Path $e.P -Recurse -Force; Write-Host ("  [ok] {0}: block removed" -f $e.N) -ForegroundColor Green }
        else                { Write-Host ("  [skip] {0}: no policy" -f $e.N) -ForegroundColor DarkYellow }
    }
}

# ===================== AppLocker =====================
function Set-AppLocker([string]$Mode) {
    $policy = @"
<AppLockerPolicy Version="1">
  <RuleCollection Type="Exe" EnforcementMode="$Mode">
    <FilePathRule Id="a0000000-0000-0000-0000-000000000001" Name="Admins-allow-all" Description="admin safety valve" UserOrGroupSid="$AdminsSid" Action="Allow">
      <Conditions><FilePathCondition Path="*" /></Conditions>
    </FilePathRule>
    <FilePathRule Id="a0000000-0000-0000-0000-000000000002" Name="Everyone-allow-Windows" Description="allow Windows dir, exclude registry/script hosts" UserOrGroupSid="$EveryoneSid" Action="Allow">
      <Conditions><FilePathCondition Path="%WINDIR%\*" /></Conditions>
      <Exceptions>
        <FilePathCondition Path="%WINDIR%\regedit.exe" />
        <FilePathCondition Path="%SYSTEM32%\reg.exe" />
        <FilePathCondition Path="%SYSTEM32%\cmd.exe" />
        <FilePathCondition Path="%SYSTEM32%\WindowsPowerShell\v1.0\powershell.exe" />
        <FilePathCondition Path="%SYSTEM32%\WindowsPowerShell\v1.0\powershell_ise.exe" />
        <FilePathCondition Path="%SYSTEM32%\wscript.exe" />
        <FilePathCondition Path="%SYSTEM32%\cscript.exe" />
        <FilePathCondition Path="%SYSTEM32%\mshta.exe" />
        <FilePathCondition Path="%WINDIR%\SysWOW64\reg.exe" />
        <FilePathCondition Path="%WINDIR%\SysWOW64\cmd.exe" />
        <FilePathCondition Path="%WINDIR%\SysWOW64\WindowsPowerShell\v1.0\powershell.exe" />
        <FilePathCondition Path="%WINDIR%\SysWOW64\wscript.exe" />
        <FilePathCondition Path="%WINDIR%\SysWOW64\cscript.exe" />
        <FilePathCondition Path="%WINDIR%\SysWOW64\mshta.exe" />
      </Exceptions>
    </FilePathRule>
    <FilePathRule Id="a0000000-0000-0000-0000-000000000003" Name="Everyone-allow-ProgramFiles" Description="allow installed programs" UserOrGroupSid="$EveryoneSid" Action="Allow">
      <Conditions><FilePathCondition Path="%PROGRAMFILES%\*" /></Conditions>
    </FilePathRule>
  </RuleCollection>
  <RuleCollection Type="Script" EnforcementMode="$Mode">
    <FilePathRule Id="b0000000-0000-0000-0000-000000000001" Name="Admins-allow-all-scripts" Description="" UserOrGroupSid="$AdminsSid" Action="Allow">
      <Conditions><FilePathCondition Path="*" /></Conditions>
    </FilePathRule>
    <FilePathRule Id="b0000000-0000-0000-0000-000000000002" Name="Everyone-allow-Windows-scripts" Description="" UserOrGroupSid="$EveryoneSid" Action="Allow">
      <Conditions><FilePathCondition Path="%WINDIR%\*" /></Conditions>
    </FilePathRule>
    <FilePathRule Id="b0000000-0000-0000-0000-000000000003" Name="Everyone-allow-ProgramFiles-scripts" Description="" UserOrGroupSid="$EveryoneSid" Action="Allow">
      <Conditions><FilePathCondition Path="%PROGRAMFILES%\*" /></Conditions>
    </FilePathRule>
  </RuleCollection>
  <RuleCollection Type="Msi" EnforcementMode="$Mode">
    <FilePathRule Id="c0000000-0000-0000-0000-000000000001" Name="Admins-allow-all-msi" Description="only admins install MSI" UserOrGroupSid="$AdminsSid" Action="Allow">
      <Conditions><FilePathCondition Path="*" /></Conditions>
    </FilePathRule>
  </RuleCollection>
  <RuleCollection Type="Appx" EnforcementMode="$Mode">
    <FilePublisherRule Id="d0000000-0000-0000-0000-000000000001" Name="Everyone-allow-signed-appx" Description="allow signed packaged apps" UserOrGroupSid="$EveryoneSid" Action="Allow">
      <Conditions>
        <FilePublisherCondition PublisherName="*" ProductName="*" BinaryName="*">
          <BinaryVersionRange LowSection="*" HighSection="*" />
        </FilePublisherCondition>
      </Conditions>
    </FilePublisherRule>
  </RuleCollection>
</AppLockerPolicy>
"@
    # AppIDSvc is a protected service on modern Windows -- Set-Service is denied, use the registry.
    try {
        Set-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\AppIDSvc" -Name Start -Value 2 -ErrorAction Stop
        Write-Host "  [ok] AppIDSvc set to auto-start (registry)" -ForegroundColor Green
    } catch { Write-Host ("  [warn] could not set AppIDSvc start type: {0}" -f $_) -ForegroundColor Yellow }
    try {
        Start-Service -Name AppIDSvc -ErrorAction Stop
        Write-Host "  [ok] AppIDSvc started" -ForegroundColor Green
    } catch {
        Write-Host "  [warn] AppIDSvc could not be started now -- REBOOT required for AppLocker to enforce." -ForegroundColor Yellow
    }
    $tmp = Join-Path $env:TEMP "aiaccess-applocker.xml"
    $policy | Out-File -FilePath $tmp -Encoding Unicode
    Import-Module AppLocker -ErrorAction Stop
    Set-AppLockerPolicy -XmlPolicy $tmp -ErrorAction Stop
    Remove-Item $tmp -Force -ErrorAction SilentlyContinue
    Write-Host ("  [ok] AppLocker applied (mode: {0})" -f $Mode) -ForegroundColor Green
}

function Clear-AppLocker {
    $empty = '<AppLockerPolicy Version="1"></AppLockerPolicy>'
    $tmp = Join-Path $env:TEMP "aiaccess-applocker-empty.xml"
    $empty | Out-File -FilePath $tmp -Encoding Unicode
    try {
        Import-Module AppLocker -ErrorAction Stop
        Set-AppLockerPolicy -XmlPolicy $tmp -ErrorAction Stop
        Write-Host "  [ok] AppLocker policy cleared" -ForegroundColor Green
    } catch { Write-Host ("  [err] clearing AppLocker: {0}" -f $_) -ForegroundColor Red }
    Remove-Item $tmp -Force -ErrorAction SilentlyContinue
}

function Get-AppLockerMode {
    try {
        Import-Module AppLocker -ErrorAction Stop
        $p = Get-AppLockerPolicy -Effective -Xml -ErrorAction Stop
        if ($p -match 'EnforcementMode="Enabled"')   { return 'Enforce' }
        if ($p -match 'EnforcementMode="AuditOnly"') { return 'Audit' }
        return 'None'
    } catch { return 'Unknown' }
}

# ===================== status =====================
function Show-Status {
    Write-Host "=== AI Access Status ===" -ForegroundColor Cyan
    Write-Host "Site block:" -ForegroundColor White
    foreach ($e in @(@{N='Chrome';P=$ChromePath}, @{N='Edge';P=$EdgePath}, @{N='Firefox';P=$FirefoxPath})) {
        if (Test-Path $e.P) { Write-Host ("  {0,-8} BLOCKED ({1} rules)" -f $e.N, (Get-Item $e.P).Property.Count) -ForegroundColor Red }
        else                { Write-Host ("  {0,-8} allowed" -f $e.N) -ForegroundColor Green }
    }
    Write-Host ("Domains configured: {0}" -f ((Get-Domains) -join ', ')) -ForegroundColor Gray
    Write-Host ("AppLocker: {0}" -f (Get-AppLockerMode)) -ForegroundColor White
}

# ===================== dispatch =====================
if ($ListSites) { Write-Host ("Blocked domains:") -ForegroundColor Cyan; Get-Domains | ForEach-Object { Write-Host "  - $_" }; return }

if ($Status) { Show-Status; return }

if ($AddSite.Count -gt 0) {
    $new = Save-Domains ((Get-Domains) + $AddSite)
    Write-Host ("Added. Now blocking {0} domain(s):" -f $new.Count) -ForegroundColor Cyan
    $new | ForEach-Object { Write-Host "  - $_" }
    Write-Host "Re-applying site block..." -ForegroundColor White
    Set-SiteBlock
    Write-Host "Restart the browser to take effect." -ForegroundColor Yellow
    return
}

if ($RemoveSite.Count -gt 0) {
    $keep = (Get-Domains) | Where-Object { $RemoveSite -notcontains $_ }
    $new = Save-Domains $keep
    Write-Host ("Removed. Now blocking {0} domain(s):" -f $new.Count) -ForegroundColor Cyan
    $new | ForEach-Object { Write-Host "  - $_" }
    Write-Host "Re-applying site block..." -ForegroundColor White
    Set-SiteBlock
    Write-Host "Restart the browser to take effect." -ForegroundColor Yellow
    return
}

if ($Reset) {
    Write-Host "=== Reset: undoing everything ===" -ForegroundColor Cyan
    Write-Host "Removing site block..." -ForegroundColor White
    Remove-SiteBlock
    Write-Host "Clearing AppLocker..." -ForegroundColor White
    Clear-AppLocker
    if (Test-Path $StoreKey) { Remove-Item $StoreKey -Recurse -Force; Write-Host "  [ok] domain list store removed" -ForegroundColor Green }
    Write-Host ""
    Write-Host "Fully reset. Restart browsers to pick up the change." -ForegroundColor Yellow
    return
}

if ($Unblock) {
    Write-Host "=== Unblock: lifting site block (AppLocker untouched) ===" -ForegroundColor Yellow
    Remove-SiteBlock
    Write-Host ""
    Write-Host "UNBLOCKED. Restart the browser (or chrome://policy -> Reload policies)." -ForegroundColor Yellow
    Write-Host "Re-apply when done:  .\AIAccess.ps1 -Block" -ForegroundColor Yellow
    return
}

if ($Block) {
    Write-Host "=== Block: re-applying site block ===" -ForegroundColor Cyan
    Set-SiteBlock
    Write-Host ""
    Write-Host "BLOCKED. Restart the browser to take effect." -ForegroundColor Yellow
    return
}

if ($Init) {
    Write-Host "=== Init: site block + AppLocker hardening ===" -ForegroundColor Cyan
    if (-not (Test-Path $StoreKey)) { Save-Domains $DefaultDomains | Out-Null }
    Write-Host "Applying site block..." -ForegroundColor White
    Set-SiteBlock
    if (-not $SkipAppLocker) {
        $mode = if ($Enforce) { 'Enabled' } else { 'AuditOnly' }
        Write-Host ("Applying AppLocker ({0})..." -f $mode) -ForegroundColor White
        try { Set-AppLocker $mode } catch { Write-Host ("  [err] AppLocker failed: {0}" -f $_) -ForegroundColor Red }
        if (-not $Enforce) {
            Write-Host "  AUDIT mode: nothing blocked, only logged." -ForegroundColor Yellow
            Write-Host "  Check: Event Viewer -> Applications and Services Logs -> Microsoft -> Windows -> AppLocker" -ForegroundColor Gray
            Write-Host "  When clean, run:  .\AIAccess.ps1 -Init -Enforce" -ForegroundColor Gray
        }
    } else { Write-Host "  [skip] AppLocker (-SkipAppLocker)" -ForegroundColor DarkYellow }
    Write-Host ""
    Write-Host "Done. Restart browsers to take effect." -ForegroundColor Green
    Show-Status
    return
}

# no switch given
Write-Host "Usage:" -ForegroundColor Cyan
Write-Host "  .\AIAccess.ps1 -Init [-Enforce] [-SkipAppLocker]   apply site block + AppLocker"
Write-Host "  .\AIAccess.ps1 -Status                             show current state"
Write-Host "  .\AIAccess.ps1 -Unblock                            temporarily allow sites"
Write-Host "  .\AIAccess.ps1 -Block                              restore site block"
Write-Host "  .\AIAccess.ps1 -Reset                              undo everything"
Write-Host "  .\AIAccess.ps1 -AddSite a.com,b.com                add domains"
Write-Host "  .\AIAccess.ps1 -RemoveSite a.com                   remove domains"
Write-Host "  .\AIAccess.ps1 -ListSites                          list domains"
