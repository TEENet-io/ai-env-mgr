'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const { prepare } = require('./setup-codex-skill-test.cjs');

function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'codex-skill-setup-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const installDir = path.join(dir, 'Codex');
  fs.mkdirSync(path.join(installDir, '_internal', 'app'), { recursive: true });
  fs.writeFileSync(path.join(installDir, 'Codex.vbs'), 'existing launcher');
  fs.writeFileSync(path.join(installDir, '_internal', 'app', 'ChatGPT.exe'), 'fixture');
  const profileDir = path.join(dir, 'user');
  fs.mkdirSync(path.join(profileDir, '.codex'), { recursive: true });
  fs.writeFileSync(path.join(profileDir, '.codex', 'config.toml'), 'original employee config');
  fs.writeFileSync(path.join(profileDir, '.codex', 'auth.json'), 'original employee auth');
  const connection = { windows_user: 'weipeng', model: 'gpt-fixture', base_url: 'https://litellm.teenet.app/v1', api_key: 'fixture-not-a-real-key', expires: '2099-01-01T00:00:00Z' };
  fs.writeFileSync(path.join(dir, 'connection.json'), JSON.stringify(connection));
  fs.writeFileSync(path.join(dir, 'models.json'), JSON.stringify({ models: [{ slug: connection.model }] }));
  return { packageDir: dir, installDir, profileDir, localAppDataDir: path.join(dir, 'local'), username: 'weipeng', protect() {}, checkRunning() {}, connection };
}

test('isolated setup preserves original login and reads the scoped gateway key', t => {
  const f = fixture(t);
  let protectedBeforeWrite = false;
  f.protect = dir => { assert.equal(fs.existsSync(path.join(dir, 'auth.json')), false); protectedBeforeWrite = true; };
  const r = prepare(f);
  assert.equal(protectedBeforeWrite, true);
  assert.equal(fs.readFileSync(path.join(f.profileDir, '.codex', 'config.toml'), 'utf8'), 'original employee config');
  assert.equal(fs.readFileSync(path.join(f.profileDir, '.codex', 'auth.json'), 'utf8'), 'original employee auth');
  const config = fs.readFileSync(path.join(r.testHome, 'config.toml'), 'utf8');
  assert.match(config, /model_provider = "gateway"/);
  assert.match(config, /base_url = "https:\/\/litellm\.teenet\.app\/v1"/);
  assert.equal(JSON.parse(fs.readFileSync(path.join(r.testHome, 'auth.json'))).OPENAI_API_KEY, f.connection.api_key);
  const launcher = fs.readFileSync(r.launcherPath).subarray(2).toString('utf16le');
  assert.ok(launcher.includes(r.testHome));
  assert.ok(launcher.includes(path.join(f.installDir, 'Codex.vbs')));
});

test('wrong employee, expired key and failed ACL protection never write credentials', t => {
  for (const kind of ['wrong-user', 'expired', 'acl-failure', 'running-codex']) {
    const f = fixture(t);
    if (kind === 'wrong-user') f.username = 'other-employee';
    if (kind === 'expired') { f.connection.expires = '2000-01-01T00:00:00Z'; fs.writeFileSync(path.join(f.packageDir, 'connection.json'), JSON.stringify(f.connection)); }
    if (kind === 'acl-failure') f.protect = () => { throw new Error('ACL denied'); };
    if (kind === 'running-codex') f.checkRunning = () => { throw new Error('Codex is running'); };
    assert.throws(() => prepare(f));
    assert.equal(fs.existsSync(path.join(f.localAppDataDir, 'TEENet', 'CodexSkillTest', 'auth.json')), false);
  }
});

test('an unrelated existing test directory is preserved', t => {
  const f = fixture(t);
  const existing = path.join(f.localAppDataDir, 'TEENet', 'CodexSkillTest');
  fs.mkdirSync(existing, { recursive: true });
  fs.writeFileSync(path.join(existing, 'auth.json'), 'keep this file');
  assert.throws(() => prepare(f), /not managed/);
  assert.equal(fs.readFileSync(path.join(existing, 'auth.json'), 'utf8'), 'keep this file');
});
