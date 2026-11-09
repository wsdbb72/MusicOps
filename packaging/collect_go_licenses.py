"""Collect third-party license texts for the Go distribution.

Build-time script (same status as packaging/prepare_engines.py): it runs
`go list -m -json all` in native/core and copies each Go module's
LICENSE/LICENCE/NOTICE files into <bundle-root>/licenses/<module>/, plus
the PlaylistOut license and the Inno Setup translation notices, which the
old Python collector also carried and which still apply.

It never touches the Python dependency set: the Go bundle contains no
PySide6, mutagen, Pillow or Python runtime, so shipping their licenses
would be factually wrong. Usage:

    python3 packaging/collect_go_licenses.py <bundle-root> [--go-root native/core]

The module cache must already be warm (the Go builds run first).
Modules without a license file in their directory are listed on stdout
as a warning, not an error: some small modules ship none.
"""
import json
import shutil
import subprocess
import sys
from pathlib import Path

LICENSE_NAMES = (
    'LICENSE', 'LICENSE.md', 'LICENSE.txt', 'LICENSE-MIT', 'LICENSE-APACHE',
    'LICENCE', 'LICENCE.md', 'LICENCE.txt',
    'NOTICE', 'NOTICE.md', 'NOTICE.txt',
    'COPYING', 'COPYING.md', 'COPYING.txt',
)


def iter_modules(go_root: Path):
    result = subprocess.run(
        ['go', 'list', '-m', '-json', 'all'],
        cwd=str(go_root), capture_output=True, text=True, check=True)
    decoder = json.JSONDecoder()
    text, index = result.stdout, 0
    while True:
        text = text[index:].lstrip()
        if not text:
            return
        module, index = decoder.raw_decode(text)
        yield module


def main(argv):
    if len(argv) < 2:
        print('usage: collect_go_licenses.py <bundle-root> [--go-root native/core]', file=sys.stderr)
        return 2
    root = Path(__file__).resolve().parents[1]
    bundle_root = Path(argv[1])
    go_root = root / 'native/core'
    if len(argv) > 3 and argv[2] == '--go-root':
        go_root = Path(argv[3])
    out = bundle_root / 'licenses'
    out.mkdir(parents=True, exist_ok=True)

    playlist_license = root / 'docs/references/PlaylistOut-LICENSE.txt'
    if playlist_license.is_file():
        target = out / 'PlaylistOut'
        target.mkdir(exist_ok=True)
        shutil.copy2(playlist_license, target / 'LICENSE.txt')

    installer_language = root / 'packaging/Languages'
    language_notices = out / 'Inno-Setup-Translation'
    language_notices.mkdir(exist_ok=True)
    for notice in ('LICENSE', 'README.md'):
        source = installer_language / notice
        if source.is_file():
            shutil.copy2(source, language_notices / notice)

    copied, missing, graph_only = 0, [], []
    for module in iter_modules(go_root):
        path = module.get('Path', '')
        directory = module.get('Dir', '')
        if not path or path == 'github.com/wsdbb72/MusicOps/native/core':
            continue
        if not directory:
            # In the module graph but with no source downloaded: it
            # cannot be linked into the binaries, so there is no
            # license to collect for it.
            graph_only.append(path)
            continue
        target = out.joinpath(*path.split('/'))
        target.mkdir(parents=True, exist_ok=True)
        version = module.get('Version', '')
        (target / 'package.txt').write_text(
            f'{path} {version}\n', encoding='utf-8')
        found = False
        for name in LICENSE_NAMES:
            candidate = Path(directory) / name
            if candidate.is_file():
                shutil.copy2(candidate, target / name)
                found = True
        if found:
            copied += 1
        else:
            missing.append(path)
    print(f'collect_go_licenses: {copied} modules with license files, '
          f'{len(graph_only)} graph-only modules skipped')
    if missing:
        print('collect_go_licenses: no license file found for:')
        for name in missing:
            print('  ' + name)
    return 0


if __name__ == '__main__':
    raise SystemExit(main(sys.argv))
