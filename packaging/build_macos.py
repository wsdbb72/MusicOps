"""Build and package a native, ad-hoc-signed MusicOps.app on macOS.

Developer ID signing and notarization are deliberately absent until the owner
provides the corresponding credentials. The runner smoke test is mandatory
before a release artifact is produced.
"""
from __future__ import annotations
import argparse,hashlib,json,os,platform,plistlib,re,shutil,subprocess,sys,tomllib
from pathlib import Path

ROOT=Path(__file__).resolve().parents[1]

def run(*args,**kwargs):return subprocess.run([str(arg) for arg in args],check=True,**kwargs)

def verify_crypto_static(extension):
    """Reject OpenSSL dylib collisions before and after PyInstaller freezing.

    Prevent frozen Python/OpenSSL dylib collisions. Use the upstream
    macOS static build instructions if a source build is needed:
    https://cryptography.io/en/latest/installation/#building-cryptography-on-macos
    """
    linked=run('/usr/bin/otool','-L',extension,capture_output=True,text=True).stdout
    for line in linked.splitlines()[1:]:
        dependency=line.strip().split(' (',1)[0]
        if re.fullmatch(r'lib(?:ssl|crypto)(?:[.-].*)?\.dylib',Path(dependency).name):
            raise RuntimeError('cryptography must statically link OpenSSL before packaging; reinstall with OPENSSL_STATIC=1, --no-binary=cryptography and --no-cache-dir. Dynamic dependency: '+Path(dependency).name)
    print('Verified static OpenSSL linkage: '+str(extension),flush=True)

def version():
    value=tomllib.loads((ROOT/'pyproject.toml').read_text('utf-8'))['project']['version']
    if not __import__('re').fullmatch(r'\d+\.\d+\.\d+(?:[a-z0-9.-]+)?',value):raise ValueError('Invalid application version')
    return value

def make_icon(destination):
    """Use Pillow to produce all standard ICNS sizes from the shared PNG."""
    from PIL import Image,ImageOps
    image=Image.open(ROOT/'src/musicops/assets/musicops.png').convert('RGBA')
    image=ImageOps.contain(image,(1024,1024),Image.Resampling.LANCZOS)
    canvas=Image.new('RGBA',(1024,1024));canvas.alpha_composite(image,((1024-image.width)//2,(1024-image.height)//2))
    destination.parent.mkdir(parents=True,exist_ok=True);canvas.save(destination,format='ICNS',sizes=[(16,16),(32,32),(64,64),(128,128),(256,256),(512,512),(1024,1024)])
    if destination.stat().st_size<1000:raise RuntimeError('ICNS generation failed')

def audit_bundle(app):
    forbidden={'.git','.env','profile','profiles','vault','state','gui-startup-error.txt','worker-startup-error.txt'}
    for item in app.rglob('*'):
        if item.name in forbidden or item.suffix.lower() in ('.sqlite','.sqlite3','.dpapi','.keychain','.keychain-db','.torrent','.ncm','.lnk') or item.name.endswith(('.sqlite-wal','.sqlite-shm')):raise RuntimeError('Private/runtime state in distributable: '+item.name)
        if item.is_symlink():
            try:item.resolve().relative_to(app.resolve())
            except ValueError:raise RuntimeError('Bundle symlink escapes application') from None

def package(args):
    if args.arch!='arm64':raise SystemExit('Only Apple Silicon arm64 is published')
    if sys.platform!='darwin':raise SystemExit('macOS bundles must be built and smoke-tested on macOS')
    if platform.machine()!=args.arch:raise SystemExit('Use a native runner for each architecture; cross-builds are not accepted')
    from cryptography.hazmat.bindings import _rust
    verify_crypto_static(Path(_rust.__file__))
    output=Path(args.output).resolve();output.mkdir(parents=True,exist_ok=True);app_version=version();stem=f'MusicOps-{app_version}-macOS-{args.arch}'
    icon=output/'build-assets/musicops.icns';make_icon(icon)
    legal=output/'build-assets/legal';legal.mkdir(parents=True,exist_ok=True)
    run(sys.executable,ROOT/'packaging/collect_licenses.py',legal,cwd=ROOT)
    shutil.copy2(ROOT/'LICENSE',legal/'LICENSE');shutil.copy2(ROOT/'packaging/THIRD-PARTY-NOTICES.md',legal/'THIRD-PARTY-NOTICES.md')
    env=os.environ.copy();env.update(MUSICOPS_MAC_ARCH=args.arch,MUSICOPS_MAC_ICON=str(icon),MUSICOPS_MAC_LEGAL=str(legal),MACOSX_DEPLOYMENT_TARGET='15.0',PYINSTALLER_STRICT_BUNDLE_CODESIGN_ERROR='1')
    dist=output/'dist';run(sys.executable,'-m','PyInstaller','--noconfirm','--clean','--workpath',output/'build','--distpath',dist,ROOT/'packaging/MusicOps-mac.spec',cwd=ROOT,env=env)
    app=dist/'MusicOps.app'
    run(sys.executable,ROOT/'packaging/prepare_engines.py','--output',app/'Contents/Resources/tools/services','--cache',output/'engine-build')
    # Preserve Chrome's upstream helper signatures/entitlements. Re-seal only
    # MusicOps after adding resources; the patched driver is signed separately.
    run('/usr/bin/codesign','--force','--sign','-',app)
    info=plistlib.loads((app/'Contents/Info.plist').read_bytes())
    if info.get('CFBundleIdentifier')!='com.wsdbb72.MusicOps' or info.get('CFBundleShortVersionString')!=app_version or info.get('CFBundleExecutable')!='MusicOps' or info.get('LSBackgroundOnly') is not False:raise RuntimeError('App identity/version/GUI mode mismatch')
    crypto_extensions={path.resolve() for path in (app/'Contents/Frameworks/cryptography/hazmat/bindings').glob('_rust*.so') if path.is_file()}
    if not crypto_extensions:raise RuntimeError('Packaged cryptography extension missing')
    for extension in crypto_extensions:verify_crypto_static(extension)
    for executable in ('MusicOps','MusicOps-worker'):
        path=app/'Contents/MacOS'/executable
        if not path.is_file() or not os.access(path,os.X_OK):raise RuntimeError('Packaged executable missing: '+executable)
        run('/usr/bin/lipo',path,'-verify_arch',args.arch)
    # PyInstaller signs the complete app during BUNDLE construction. Verify the
    # signature and explicitly record that it provides no Developer ID identity.
    run('/usr/bin/codesign','--verify','--deep','--strict',app)
    signature=subprocess.run(['/usr/bin/codesign','-dvv',str(app)],capture_output=True,text=True,check=True).stderr
    if 'Signature=adhoc' not in signature or 'Authority=Developer ID' in signature:raise RuntimeError('Unexpected signing identity; review the release policy')
    reports=output/'reports';reports.mkdir(exist_ok=True)
    run(sys.executable,ROOT/'packaging/smoke_engines.py','--root',app/'Contents/Resources/tools/services','--profile',output/'engine-smoke-profile','--report',reports/(stem+'-engines.json'),cwd=ROOT,env=env)
    run(sys.executable,ROOT/'packaging/smoke_macos.py','--app',app,'--work',output/'smoke','--report',reports/(stem+'-smoke.json'),'--expected-arch',args.arch,cwd=ROOT,env=env)
    audit_bundle(app)
    stage=output/'package'/stem;stage.mkdir(parents=True,exist_ok=True)
    run('/usr/bin/ditto',app,stage/'MusicOps.app')
    shutil.copy2(ROOT/'LICENSE',stage/'LICENSE');shutil.copy2(ROOT/'packaging/THIRD-PARTY-NOTICES.md',stage/'THIRD-PARTY-NOTICES.md')
    run(sys.executable,ROOT/'packaging/collect_licenses.py',stage,cwd=ROOT)
    (stage/'macOS 使用说明.txt').write_text(
        'MusicOps '+app_version+' · macOS 15 或更新版本\n\n'
        '推荐使用同版本 .pkg 安装向导；ZIP 用户可将 MusicOps.app 拖入“应用程序”后打开。\n'
        '此包仅作 ad-hoc 完整性签名，没有 Apple Developer ID 签名和公证。\n'
        '首次打开时如被 Gatekeeper 阻止，请核对 GitHub Release 的 SHA-256，\n'
        '确认来源后使用“系统设置 → 隐私与安全性 → 仍要打开”，依系统提示确认。\n'
        '无需关闭系统安全功能。Apple 官方说明：https://support.apple.com/102445\n\n'
        '默认用户资料：~/Library/Application Support/MusicOps/profile\n'
        '凭据由当前 macOS 用户的钥匙串保护；不会写入 .app，也不能直接复制 Windows DPAPI 凭据。\n'
        '首次打开请使用连接向导；没有的服务可以跳过。\n'
        'FFmpeg / ffprobe 未包含，可在设置中选择已安装的 macOS 可执行程序。\n'
        'Windows 路径和 .exe 媒体工具不能在 Mac 上使用。\n',encoding='utf-8')
    archive=output/(stem+'.zip');run('/usr/bin/ditto','-c','-k','--sequesterRsrc','--keepParent',stage,archive)
    from macos_installer import build as build_installer
    installer=build_installer(app,output,app_version,args.arch)
    installer_report=reports/(stem+'-installer.json')
    run(sys.executable,ROOT/'packaging/smoke_macos_installer.py','--pkg',installer,'--app',app,'--work',output/'installer-smoke','--report',installer_report,'--arch',args.arch,cwd=ROOT,env=env)
    artifacts={}
    for artifact in (archive,installer):
        hashes={}
        for algorithm in ('sha256','sha512'):
            with artifact.open('rb') as stream:hashes[algorithm]=hashlib.file_digest(stream,algorithm).hexdigest()
        artifacts[artifact.name]=hashes
    digests=artifacts[archive.name]
    for algorithm in ('sha256','sha512'):(output/(stem+'.'+algorithm)).write_text(''.join(values[algorithm]+'  '+name+'\n' for name,values in artifacts.items()),'utf-8')
    commit=subprocess.run(['git','rev-parse','HEAD'],cwd=ROOT,capture_output=True,text=True,check=True).stdout.strip()
    engines=json.loads((reports/(stem+'-engines.json')).read_text('utf-8'))
    if not engines.get('passed') or len(engines.get('checks',{}))!=9:raise RuntimeError('Native bundled engines acceptance is incomplete')
    metadata={'managed_engines_passed':True,'managed_engine_checks':engines['checks'],'version':app_version,'architecture':args.arch,'minimum_macos':'15.0','commit':commit,'archive':archive.name,'digests':digests,'artifacts':artifacts,'installer':installer.name,'installer_smoke_passed':True,'installer_signed':False,'signing':'ad-hoc','developer_id_signed':False,'notarized':False,'cryptography_openssl_static':True,'smoke_passed':True,'runner_macos':platform.mac_ver()[0]}
    (output/(stem+'-build.json')).write_text(json.dumps(metadata,ensure_ascii=False,indent=2)+'\n','utf-8')
    print(json.dumps(metadata,ensure_ascii=True))

def main():
    parser=argparse.ArgumentParser();parser.add_argument('--arch',choices=('arm64',),default=platform.machine());parser.add_argument('--output',default='dist/macos');args=parser.parse_args();package(args)

if __name__=='__main__':main()
