"""Native Installer product archive; the payload contains only the app bundle."""
from __future__ import annotations
import html,plistlib,re,subprocess,sys
from pathlib import Path
from xml.etree import ElementTree as ET

IDENTIFIER='com.wsdbb72.MusicOps.pkg'
APP_IDENTIFIER='com.wsdbb72.MusicOps'
ROOT=Path(__file__).resolve().parents[1]

def materials(directory,version,arch):
    if arch not in ('arm64','x86_64') or not re.fullmatch(r'\d+\.\d+\.\d+',version):raise ValueError('Invalid installer version or architecture')
    directory=Path(directory);resources=directory/'resources';scripts=directory/'scripts'
    resources.mkdir(parents=True,exist_ok=True);scripts.mkdir(parents=True,exist_ok=True)
    components=[{'RootRelativeBundlePath':'MusicOps.app','BundleIsRelocatable':False,'BundleIsVersionChecked':True,'BundleHasStrictIdentifier':True,'BundleOverwriteAction':'upgrade'}]
    (directory/'components.plist').write_bytes(plistlib.dumps(components))
    root=ET.Element('installer-gui-script',minSpecVersion='2')
    ET.SubElement(root,'title').text='MusicOps '+version
    ET.SubElement(root,'options',customize='never',hostArchitectures=arch,**{'require-scripts':'false'})
    ET.SubElement(root,'domains',enable_anywhere='false',enable_currentUserHome='false',enable_localSystem='true')
    allowed=ET.SubElement(ET.SubElement(root,'volume-check'),'allowed-os-versions');ET.SubElement(allowed,'os-version',min='15.0')
    for tag,file in (('welcome','Welcome.html'),('readme','ReadMe.html'),('license','License.html')):ET.SubElement(root,tag,file=file,**{'mime-type':'text/html'})
    outline=ET.SubElement(root,'choices-outline');ET.SubElement(outline,'line',choice='musicops')
    choice=ET.SubElement(root,'choice',id='musicops',visible='false',title='MusicOps');ET.SubElement(choice,'pkg-ref',id=IDENTIFIER)
    ET.SubElement(root,'pkg-ref',id=IDENTIFIER,version=version).text='MusicOps-component.pkg'
    close=ET.SubElement(ET.SubElement(root,'pkg-ref',id=IDENTIFIER),'must-close');ET.SubElement(close,'app',id=APP_IDENTIFIER)
    ET.ElementTree(root).write(directory/'Distribution.xml',encoding='utf-8',xml_declaration=True)
    style='<meta charset="utf-8"><style>body{font:15px -apple-system,sans-serif;color:#182838;line-height:1.6;padding:12px}h1{color:#147b75}code{font-size:13px}pre{white-space:pre-wrap}</style>'
    chip='Apple Silicon' if arch=='arm64' else 'Intel'
    (resources/'Welcome.html').write_text(style+f'<h1>MusicOps {version}</h1><p>{chip} · macOS 15 或更新版本</p><p>按向导完成安装，随后从“应用程序”打开 MusicOps。</p><p>应用安装到 <code>/Applications/MusicOps.app</code>，无需安装 Python。</p><p>已有音乐库、配置和钥匙串凭据会保留。</p>','utf-8')
    (resources/'ReadMe.html').write_text(style+'<h1>安装前请确认</h1><p>升级前请在 MusicOps“任务中心”点击“退出应用及后台”。有正在执行的任务时，请先等待完成，或取消对应任务后退出。</p><p>旧版没有此按钮时，请在确认任务已停止后，通过“活动监视器”结束 MusicOps 和 MusicOps-worker。安装器不会强制终止任务。</p><p>个人资料位于 <code>~/Library/Application Support/MusicOps/profile</code>，本安装包不写入该目录，也不访问钥匙串。</p><p>当前安装包没有 Developer ID Installer 签名和 Apple 公证。确认来自官方 GitHub Release 并核对校验值后，按 macOS“隐私与安全性”提示打开；无需关闭系统安全功能。</p><p>卸载时将“应用程序”中的 MusicOps 移到废纸篓即可，个人资料会保留。</p>','utf-8')
    (resources/'License.html').write_text(style+'<pre>'+html.escape((ROOT/'LICENSE').read_text('utf-8'))+'</pre>','utf-8')
    script='''#!/bin/sh
set -eu
if [ "${3:-/}" != "/" ]; then
  echo "MusicOps: please install on the startup volume." >&2
  exit 1
fi
if [ -L /Applications ] || [ -L /Applications/MusicOps.app ]; then
  echo "MusicOps: refusing a symbolic-link installation destination." >&2
  exit 1
fi
physical_arm=$(/usr/sbin/sysctl -n hw.optional.arm64 2>/dev/null || /bin/echo 0)
'''
    script+=('if [ "$physical_arm" != "1" ]; then\n' if arch=='arm64' else 'if [ "$physical_arm" = "1" ]; then\n')+'  echo "MusicOps: use the package matching your Mac chip (Apple Silicon / Intel)." >&2\n  exit 1\nfi\n'
    script+='''for task_name in MusicOps MusicOps-worker; do
  if /usr/bin/pgrep -x "$task_name" >/dev/null; then
    echo "MusicOps: quit the app and its background worker before installing. No process was terminated." >&2
    exit 1
  fi
done
exit 0
'''
    path=scripts/'preinstall';path.write_text(script,'utf-8');path.chmod(0o755)
    return directory

def build(app,output,version,arch):
    if sys.platform!='darwin':raise RuntimeError('Build the native installer on macOS')
    app=Path(app).resolve();output=Path(output).resolve();directory=materials(output/'installer-build',version,arch)
    payload=directory/'payload';payload.mkdir(exist_ok=True)
    subprocess.run(['/usr/bin/ditto',str(app),str(payload/'MusicOps.app')],check=True)
    component=directory/'MusicOps-component.pkg'
    subprocess.run(['/usr/bin/pkgbuild','--root',str(payload),'--component-plist',str(directory/'components.plist'),'--scripts',str(directory/'scripts'),'--identifier',IDENTIFIER,'--version',version,'--install-location','/Applications','--ownership','recommended',str(component)],check=True)
    package=output/f'MusicOps-{version}-macOS-{arch}.pkg'
    subprocess.run(['/usr/bin/productbuild','--distribution',str(directory/'Distribution.xml'),'--resources',str(directory/'resources'),'--package-path',str(directory),str(package)],check=True)
    return package
