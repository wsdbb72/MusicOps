"""Build pinned standalone engines and bundle Chrome for Testing, on each host."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import stat
import sys
import urllib.request
import venv
import zipfile

ROOT = Path(__file__).resolve().parents[1]
PINS = {'go-music-dl': ('guohuiyuan/go-music-dl', 'b4aa4c2e00032ff5f636310834168e85bebca010'),
        'FlareSolverr': ('FlareSolverr/FlareSolverr', '93125d6ea6956906c8dfa29e169b2dc67d993dc3')}
CHROME = '155.0.8059.84'
BROWSER_SHA256={
    'win64':{'chrome':'507f2a031441d10c62b5e3d4e863f09ac1198a571c1419976cbd8387f639afde','chromedriver':'0e629a3ef7f2be8562b857253053ebb0f871f55240f80124cd31974380ad1f3a'},
    'mac-arm64':{'chrome':'13b9e98ce56f5ce18e100a009f455c9285f399e7c9dc8a76fdf2f3a9755bb5f2','chromedriver':'6db09f1272851bdcd5137d04797b5506993291e8499e8fa8153ab1c571bba20c'}}


def run(*args, **kwargs):
    subprocess.run([str(value) for value in args], check=True, **kwargs)


def source(name, cache):
    repository, revision = PINS[name]
    path = cache / name
    if not path.exists():
        run('git', 'clone', '--filter=blob:none', '--no-checkout', 'https://github.com/' + repository + '.git', path)
        run('git', '-C', path, 'checkout', '--detach', revision)
    actual = subprocess.check_output(['git', '-C', str(path), 'rev-parse', 'HEAD'], text=True).strip()
    if actual != revision: raise RuntimeError('Engine source revision mismatch')
    return path


def patch(path, before, after):
    text = path.read_text('utf-8')
    if after in text: return
    if text.count(before) != 1: raise RuntimeError('Upstream integration contract changed: ' + path.name)
    path.write_text(text.replace(before, after), 'utf-8')


def browser(cache, output, part):
    platform = 'win64' if os.name == 'nt' else 'mac-arm64'
    archive = cache / (part + '-' + platform + '-' + CHROME + '.zip')
    if not archive.exists():
        url = f'https://storage.googleapis.com/chrome-for-testing-public/{CHROME}/{platform}/{part}-{platform}.zip'
        temporary = archive.with_suffix('.partial')
        with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(url, timeout=120) as response, temporary.open('wb') as stream:
            shutil.copyfileobj(response, stream)
        temporary.replace(archive)
    with archive.open('rb') as stream:checksum=hashlib.file_digest(stream,'sha256').hexdigest()
    if checksum!=BROWSER_SHA256[platform][part]:raise RuntimeError('Bundled browser checksum mismatch')
    with zipfile.ZipFile(archive) as bundle:
        for entry in bundle.infolist():
            relative = Path(*Path(entry.filename).parts[1:])
            target = output / relative
            if not target.resolve().is_relative_to(output.resolve()): raise RuntimeError('Unsafe browser archive')
            if entry.is_dir(): target.mkdir(parents=True, exist_ok=True); continue
            target.parent.mkdir(parents=True, exist_ok=True)
            mode=entry.external_attr>>16
            if stat.S_ISLNK(mode):
                link=bundle.read(entry).decode('utf-8')
                if not (target.parent/link).resolve().is_relative_to(output.resolve()):raise RuntimeError('Unsafe browser symlink')
                if target.is_symlink():target.unlink()
                if not target.exists():target.symlink_to(link)
                continue
            with bundle.open(entry) as source, target.open('wb') as stream: shutil.copyfileobj(source, stream)
            mode = entry.external_attr >> 16
            if mode: target.chmod(mode & 0o777)
    return checksum


def build(output, cache):
    if os.name != 'nt' and sys.platform not in ('darwin','linux'): raise RuntimeError('Native Windows/macOS/Linux build required')
    output.mkdir(parents=True, exist_ok=True); cache.mkdir(parents=True, exist_ok=True)
    go = source('go-music-dl', cache)
    # Platform cookies live only in the engine's memory; MusicOps owns encrypted persistence.
    patch(go / 'core/service.go', 'func (m *CookieManager) Save() {', 'func (m *CookieManager) Save() {\n\treturn // MusicOps: encrypted credential vault owns persistence')
    patch(go / 'internal/web/server.go', 'r.Use(corsMiddleware())', '''r.Use(corsMiddleware())
    // MusicOps: protect even loopback requests from untrusted local webpages.
    r.Use(func(c *gin.Context) {
        token := os.Getenv("MUSICOPS_SERVICE_TOKEN")
        if token == "" || c.GetHeader("X-MusicOps-Service") != token { c.AbortWithStatus(403); return }
        c.Next()
    })''')
    suffix = '.exe' if os.name == 'nt' else ''
    destination = output / 'go-music-dl'; destination.mkdir(exist_ok=True)
    environment = os.environ.copy(); environment['CGO_ENABLED'] = '0'
    run('go', 'build', '-trimpath', '-ldflags=-s -w', '-o', destination / ('go-music-dl' + suffix), './cmd/music-dl', cwd=go, env=environment)
    flare = source('FlareSolverr', cache)
    runtime = cache / 'flare-build-env'
    python = runtime / ('Scripts/python.exe' if os.name == 'nt' else 'bin/python')
    if not python.exists(): venv.EnvBuilder(with_pip=True).create(runtime)
    run(python, '-m', 'pip', 'install', '-r', flare / 'requirements.txt', 'pyinstaller==6.20.0')
    run(python, '-m', 'PyInstaller', '--noconfirm', '--clean', '--onedir', '--name', 'flaresolverr',
        '--paths', flare / 'src', '--hidden-import', 'flaresolverr', '--hidden-import', 'waitress',
        '--collect-submodules', 'bottle_plugins', '--collect-submodules', 'undetected_chromedriver',
        '--distpath', output, '--workpath', cache / 'flare-build', '--specpath', cache,
        ROOT / 'packaging/flare_entry.py')
    run(python,ROOT/'packaging/collect_licenses.py',output/'flaresolverr')
    hashes = {}
    if sys.platform=='linux':
        browser_dir=output/'flaresolverr/browser';browser_dir.mkdir(exist_ok=True)
        (browser_dir/'chrome').symlink_to('/usr/bin/chromium')
        driver_dir=output/'flaresolverr/driver';driver_dir.mkdir(exist_ok=True)
        shutil.copy2('/usr/bin/chromedriver',driver_dir/'chromedriver')
        hashes={'chrome':'distribution-package','chromedriver':hashlib.sha256((driver_dir/'chromedriver').read_bytes()).hexdigest()}
    else:
        hashes['chrome'] = browser(cache, output / 'flaresolverr/browser', 'chrome')
        hashes['chromedriver'] = browser(cache, output / 'flaresolverr/driver', 'chromedriver')
    if sys.platform=='darwin':
        # ARM macOS validates executable signatures. Patch before signing the
        # shipped driver, so UC sees an already-patched private runtime copy.
        driver=output/'flaresolverr/driver/chromedriver'
        run(python,'-c',
            "import sys;sys.path.insert(0,sys.argv[1]);from undetected_chromedriver.patcher import Patcher;"
            "p=Patcher(executable_path=sys.argv[2]);p.patch_exe();assert p.is_binary_patched()",
            flare/'src',driver)
        run('/usr/bin/codesign','--force','--sign','-',driver)
    legal = output / 'licenses'; legal.mkdir(exist_ok=True)
    for name, path in (('go-music-dl', go), ('FlareSolverr', flare)):
        shutil.copy2(path / 'LICENSE', legal / (name + '-LICENSE.txt'))
    # The CLI's actual Go dependencies accompany its corresponding source.
    modules=subprocess.check_output(['go','list','-deps','-f','{{if .Module}}{{.Module.Dir}}{{end}}','./cmd/music-dl'],cwd=go,text=True,env=environment).splitlines()
    modules=sorted({Path(item) for item in modules if item.strip() and Path(item).resolve()!=go.resolve()})
    # Corresponding, patched engine source accompanies every binary package.
    with zipfile.ZipFile(legal / 'engine-sources.zip', 'w', zipfile.ZIP_DEFLATED) as archive:
        for name, path in (('go-music-dl', go), ('FlareSolverr', flare)):
            for item in path.rglob('*'):
                if item.is_file() and not any(part in ('.git','__pycache__') for part in item.relative_to(path).parts):
                    archive.write(item, name + '/' + item.relative_to(path).as_posix())
        for module in modules:
            module_id=module.as_posix().split('/pkg/mod/',1)[-1]
            if '/pkg/mod/' not in module.as_posix():raise RuntimeError('Unexpected Go module source location')
            for item in module.rglob('*'):
                if item.is_file() and '.git' not in item.relative_to(module).parts:
                    archive.write(item,'go-dependencies/'+module_id+'/'+item.relative_to(module).as_posix())
        for name in ('prepare_engines.py', 'flare_entry.py'):
            archive.write(ROOT / 'packaging' / name, 'MusicOps-build/' + name)
    browser_version=subprocess.check_output(['/usr/bin/chromium','--version'],text=True).strip() if sys.platform=='linux' else CHROME
    (output / 'engines.json').write_text(json.dumps({'sources': PINS, 'chrome_version': browser_version, 'browser_sha256': hashes}, indent=2), 'utf-8')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(); parser.add_argument('--output', type=Path, default=ROOT / 'work/engines'); parser.add_argument('--cache', type=Path, default=ROOT / 'work/engine-build')
    args = parser.parse_args(); build(args.output.resolve(), args.cache.resolve())
