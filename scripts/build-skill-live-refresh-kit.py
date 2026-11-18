#!/usr/bin/env python3
"""Package the test Agent and a fixed-version desktop adapter, without logins."""
import argparse
import hashlib
import json
import shutil
import subprocess
import tempfile
import zipfile
from pathlib import Path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--codex-repo', type=Path, required=True)
    parser.add_argument('--agent', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    repo = Path(__file__).resolve().parent.parent
    codex = args.codex_repo.resolve()
    # Resolve actual installed versions and preserve nested dependency paths.
    resolver = r'''
const fs=require('node:fs'),path=require('node:path'),{createRequire}=require('node:module');
const root=process.argv[1],base=fs.realpathSync(path.join(root,'node_modules')),seen=new Set();
function visit(name,from){const req=createRequire(path.join(from,'package.json'));let file;
try{file=req.resolve(name+'/package.json')}catch{file=req.resolve(name);while(!fs.existsSync(path.join(file,'package.json')))file=path.dirname(file);file=path.join(file,'package.json')}
const dir=fs.realpathSync(path.dirname(file));if(seen.has(dir))return;seen.add(dir);
const p=JSON.parse(fs.readFileSync(file));for(const dep of Object.keys(p.dependencies||{}))visit(dep,dir)}
for(const name of ['@electron/asar','acorn'])visit(name,root);
console.log(JSON.stringify({base,dirs:[...seen]}));
'''
    dependencies = json.loads(subprocess.check_output(['node', '-e', resolver, str(codex)], text=True))
    with tempfile.TemporaryDirectory(prefix='skill-refresh-kit-') as temp:
        stage = Path(temp)
        for name in ['install-skill-live-refresh.ps1', 'find-codex-skill-test-install.ps1']:
            shutil.copyfile(repo / 'scripts' / name, stage / name)
        desktop = stage / 'desktop'
        desktop.mkdir()
        for name in ['install-company-skills-refresh.cjs', 'company-skills-boundary.mjs', 'company-skills-refresh.cjs']:
            shutil.copyfile(codex / 'scripts' / 'enterprise' / name, desktop / name)
        base = Path(dependencies['base'])
        for value in dependencies['dirs']:
            package = Path(value)
            target = desktop / 'node_modules' / package.relative_to(base)
            shutil.copytree(package, target, dirs_exist_ok=True)
        shutil.copyfile(args.agent, stage / 'agent-skills-test.exe')
        (stage / 'README.txt').write_text(
            'Company Skill live refresh test kit\n\n'
            '1. Stop the OLD test Agent with Ctrl+C. Close Codex once.\n'
            '2. Extract this entire ZIP, preserving the desktop folder.\n'
            '3. In Administrator PowerShell in this folder, run:\n'
            '   powershell -NoProfile -ExecutionPolicy Bypass -File .\\install-skill-live-refresh.ps1\n'
            '   Add -CodexInstallDir "C:\\Tools\\Codex" if necessary.\n'
            '4. Run .\\agent-skills-test.exe skills-test from THIS new folder.\n'
            '5. Open the existing Codex Skill Test shortcut.\n'
            '6. Update a Skill while keeping Codex open; start a new conversation to verify it.\n\n'
            'The adapter supports desktop 26.901.51231, including b13.\n'
            'It backs up app.asar and does not change model login, personal skills or conversations.\n'
            'The backend shows when files are synchronized but a desktop refresh is still pending.\n'
            'Rollback: close Codex and rerun the installer with -Rollback.\n'
            'No production backend deployment is performed by this kit.\n', encoding='utf8')
        files = {str(p.relative_to(stage)).replace('\\', '/'): hashlib.sha256(p.read_bytes()).hexdigest()
                 for p in stage.rglob('*') if p.is_file()}
        manifest = {'format': 1, 'testOnly': True, 'desktopBase': '26.901.51231',
                    'agentSource': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip(),
                    'codexSource': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=codex, text=True).strip(),
                    'containsModelCredentials': False, 'files': files}
        (stage / 'kit-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n', encoding='utf8')
        args.output.parent.mkdir(parents=True, exist_ok=True)
        pending = args.output.with_suffix('.next.zip')
        with zipfile.ZipFile(pending, 'w', zipfile.ZIP_DEFLATED) as archive:
            for file in sorted(stage.rglob('*')):
                if file.is_file():
                    archive.write(file, str(file.relative_to(stage)).replace('\\', '/'))
        with zipfile.ZipFile(pending) as archive:
            for name, digest in files.items():
                assert hashlib.sha256(archive.read(name)).hexdigest() == digest
        pending.replace(args.output)
        print(json.dumps({'output': str(args.output), 'files': len(files), 'bytes': args.output.stat().st_size,
                          'sha256': hashlib.sha256(args.output.read_bytes()).hexdigest()}))


if __name__ == '__main__':
    main()
