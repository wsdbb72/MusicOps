"""Create a source archive from the public allowlist; never include run profiles."""
import argparse
import hashlib
import json
import zipfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
DIRECTORIES = ('src', 'tests', 'docs', 'config', 'scripts', 'packaging', '.github')
PRIVATE_PARTS = {'profile', 'profiles', 'vault', 'work', 'reports', 'outputs', '__pycache__', 'specifications'}
PRIVATE_SUFFIXES = {'.pyc', '.dpapi', '.sqlite', '.sqlite3', '.db', '.torrent', '.ncm', '.lrc', '.lnk'}
SYNTHETIC_FIXTURE = Path('tests/fixtures/providers/synthetic.ncm')


def allowed(path):
    relative = path.relative_to(ROOT)
    return (not path.is_symlink() and path.is_file() and not set(relative.parts) & PRIVATE_PARTS
            and (path.suffix.casefold() not in PRIVATE_SUFFIXES or relative == SYNTHETIC_FIXTURE)
            and not path.name.startswith(('trace-', 'legacy_inventory', 'packaging-build-source')))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', required=True, help='Local destination for the public source archive')
    output = Path(parser.parse_args().output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    paths = [path for directory in DIRECTORIES for path in (ROOT / directory).rglob('*') if allowed(path)]
    paths.extend(ROOT / name for name in ('README.md', 'LICENSE', '.gitignore', 'pyproject.toml', 'requirements.lock'))
    archive = output / 'MusicOps-source.zip'
    with zipfile.ZipFile(archive, 'w', compression=zipfile.ZIP_DEFLATED, compresslevel=6) as target:
        for path in sorted(paths):
            target.write(path, Path('MusicOps') / path.relative_to(ROOT))
    manifest = {'archive': archive.name, 'files': len(paths), 'bytes': archive.stat().st_size,
                'sha256': hashlib.sha256(archive.read_bytes()).hexdigest()}
    (output / 'source-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(manifest))


if __name__ == '__main__':
    main()
