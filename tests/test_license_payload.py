"""License directories may contain Python modules; compiled caches are not notices."""
import importlib.metadata
from pathlib import Path
import runpy
import sys


def test_license_collection_excludes_python_source_and_private_build_caches(tmp_path,monkeypatch):
    base=tmp_path/'distribution';folder=base/'packaging/licenses';folder.mkdir(parents=True)
    for name in ('_spdx.pyc','__init__.pyc','__init__.py','LICENSE'):
        (folder/name).write_text('synthetic notice' if name=='LICENSE' else 'synthetic-private-build-path','utf-8')
    class Distribution:
        version='synthetic';metadata={}
        files=[Path('packaging/licenses')/name for name in ('_spdx.pyc','__init__.pyc','__init__.py','LICENSE')]
        def locate_file(self,item):return base/item
    def distribution(name):
        if name=='packaging':return Distribution()
        raise importlib.metadata.PackageNotFoundError(name)
    monkeypatch.setattr(importlib.metadata,'distribution',distribution)
    output=tmp_path/'output';monkeypatch.setattr(sys,'argv',['collect_licenses.py',str(output)])
    runpy.run_path(str(Path(__file__).resolve().parents[1]/'packaging/collect_licenses.py'),run_name='__main__')
    assert (output/'licenses/packaging/LICENSE').read_text('utf-8')=='synthetic notice'
    assert not list(output.rglob('*.pyc')) and not list(output.rglob('*.py'))
