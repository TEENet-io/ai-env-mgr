'use strict';
const fs = require('node:fs');
const path = require('node:path');
const cp = require('node:child_process');

function renderConfig(connection, catalogPath) {
  return `model = ${JSON.stringify(connection.model)}
model_provider = "gateway"
model_catalog_json = ${JSON.stringify(catalogPath.replaceAll('\\', '/'))}
stream_idle_timeout_ms = 7200000

[model_providers.gateway]
name = "Gateway"
base_url = ${JSON.stringify(connection.base_url)}
wire_api = "responses"
experimental_bearer_token = ${JSON.stringify(connection.api_key)}
`;
}

const vbsString = value => value.replaceAll('"', '""');
function launcherSource(testHome, originalLauncher) {
  return `Option Explicit
Dim shell
Set shell = CreateObject("WScript.Shell")
shell.Environment("PROCESS")("CODEX_HOME") = "${vbsString(testHome)}"
shell.Run "wscript.exe ""${vbsString(originalLauncher)}""", 1, False
`;
}

function prepare({ packageDir, installDir, profileDir, localAppDataDir, username, now = new Date(), protect, checkRunning }) {
  const connection = JSON.parse(fs.readFileSync(path.join(packageDir, 'connection.json'), 'utf8'));
  if (username.toLowerCase() !== String(connection.windows_user).toLowerCase()) {
    throw new Error(`Run setup as Windows user ${connection.windows_user}, the employee bound in the test console.`);
  }
  if (!connection.api_key || !connection.model || typeof connection.api_key !== 'string' || typeof connection.model !== 'string') {
    throw new Error('The connection package is incomplete.');
  }
  const expires = Date.parse(connection.expires);
  if (!Number.isFinite(expires) || expires <= now.getTime()) throw new Error('The test credential has expired.');
  const gateway = new URL(connection.base_url);
  if (gateway.protocol !== 'https:' || gateway.hostname !== 'litellm.teenet.app' || gateway.username || gateway.password || gateway.search || gateway.hash) {
    throw new Error('The connection package must point at the TEENet production gateway.');
  }
  const catalogText = fs.readFileSync(path.join(packageDir, 'models.json'), 'utf8');
  const catalog = JSON.parse(catalogText);
  if (!Array.isArray(catalog.models) || !catalog.models.some(model => model.slug === connection.model)) {
    throw new Error('The selected model is missing from the catalog.');
  }
  installDir = path.resolve(installDir || path.join(profileDir, 'Codex'));
  const originalLauncher = path.join(installDir, 'Codex.vbs');
  const appPath = path.join(installDir, '_internal', 'app', 'ChatGPT.exe');
  if (!fs.existsSync(originalLauncher) || !fs.existsSync(appPath)) throw new Error('Cannot find Codex b13 in the installation directory.');
  checkRunning(appPath);
  const testHome = path.join(localAppDataDir, 'TEENet', 'CodexSkillTest');
  const ownershipPath = path.join(testHome, '.skill-test-setup');
  if (fs.existsSync(testHome) && (!fs.existsSync(ownershipPath) || fs.readFileSync(ownershipPath, 'utf8') !== 'ai-env-mgr-skill-test-v1')) {
    throw new Error('The test directory already exists and is not managed by this setup.');
  }
  fs.mkdirSync(testHome, { recursive: true });
  protect(testHome);
  fs.writeFileSync(ownershipPath, 'ai-env-mgr-skill-test-v1');
  fs.writeFileSync(path.join(testHome, 'models.json'), catalogText);
  fs.writeFileSync(path.join(testHome, 'config.toml'), renderConfig(connection, path.join(testHome, 'models.json')));
  fs.writeFileSync(path.join(testHome, 'auth.json'), JSON.stringify({ auth_mode: 'apikey', OPENAI_API_KEY: connection.api_key }));
  const launcherPath = path.join(testHome, 'Launch-Codex-Skill-Test.vbs');
  fs.writeFileSync(launcherPath, Buffer.concat([Buffer.from([0xff, 0xfe]), Buffer.from(launcherSource(testHome, originalLauncher), 'utf16le')]));
  return { testHome, launcherPath, appPath, installDir, model: connection.model, expires: connection.expires, baseURL: connection.base_url };
}

function checkedRun(file, args) {
  const result = cp.spawnSync(file, args, { encoding: 'utf8', windowsHide: true });
  if (result.error || result.status !== 0) throw new Error(`Setup step failed: ${path.basename(file)}. ${result.error ? result.error.message : result.stderr || result.stdout}`);
  return result.stdout;
}

function main() {
  if (process.platform !== 'win32') throw new Error('Run this setup on Windows.');
  const system32 = path.join(process.env.SystemRoot, 'System32');
  const ps = path.join(system32, 'WindowsPowerShell', 'v1.0', 'powershell.exe');
  const result = prepare({
    packageDir: __dirname, installDir: process.argv[2], profileDir: process.env.USERPROFILE,
    localAppDataDir: process.env.LOCALAPPDATA, username: process.env.USERNAME,
    checkRunning(appPath) {
      const quotedPath = "'" + appPath.replaceAll("'", "''") + "'";
      const code = `$s=(Get-Process -Id $PID).SessionId; $p=@(Get-Process -Name ChatGPT -ErrorAction SilentlyContinue | Where-Object { $_.SessionId -eq $s -and $_.Path -ieq ${quotedPath} }); if ($p.Count) { Write-Output 'Fully quit Codex first, then run setup again.'; exit 1 }`;
      checkedRun(ps, ['-NoProfile', '-NonInteractive', '-Command', code]);
    },
    protect(dir) {
      const identity = checkedRun(path.join(system32, 'whoami.exe'), ['/user', '/fo', 'csv', '/nh']);
      const sid = identity.match(/S-1-\d+(?:-\d+)+/i)?.[0];
      if (!sid) throw new Error('Cannot identify the current Windows account.');
      checkedRun(path.join(system32, 'icacls.exe'), [dir, '/inheritance:r', '/grant:r', `*${sid}:(OI)(CI)F`, '*S-1-5-18:(OI)(CI)F']);
    },
  });
  const shortcutSource = `Option Explicit
Dim shell, shortcut
Set shell = CreateObject("WScript.Shell")
Set shortcut = shell.CreateShortcut(shell.SpecialFolders("Desktop") & "\\Codex Skill Test.lnk")
shortcut.TargetPath = "${vbsString(path.join(system32, 'wscript.exe'))}"
shortcut.Arguments = """${vbsString(result.launcherPath)}"""
shortcut.WorkingDirectory = "${vbsString(result.installDir)}"
shortcut.IconLocation = "${vbsString(result.appPath)},0"
shortcut.Save
`;
  const shortcutScript = path.join(result.testHome, 'Create-Test-Shortcut.vbs');
  fs.writeFileSync(shortcutScript, Buffer.concat([Buffer.from([0xff, 0xfe]), Buffer.from(shortcutSource, 'utf16le')]));
  checkedRun(path.join(system32, 'cscript.exe'), ['//NoLogo', shortcutScript]);
  console.log(`Ready. Gateway: ${result.baseURL}; model: ${result.model}; expires: ${result.expires}`);
  console.log('Use the desktop shortcut: Codex Skill Test. The normal shortcut keeps its original configuration.');
  console.log('The setup ZIP contains a test key; remove the ZIP and extracted connection.json after setup.');
  if (!process.argv.includes('--no-launch')) {
    const child = cp.spawn(path.join(system32, 'wscript.exe'), [result.launcherPath], { detached: true, stdio: 'ignore', windowsHide: true });
    child.on('error', () => console.error('Open the desktop Codex Skill Test shortcut manually.'));
    child.unref();
  }
}

module.exports = { renderConfig, launcherSource, prepare };
if (require.main === module) {
  try { main(); } catch (error) { console.error(error.message); process.exitCode = 1; }
}
