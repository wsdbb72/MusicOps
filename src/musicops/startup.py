"""Stable production profile selection; demonstrations use a separate profile."""
from pathlib import Path
import os,sys,json,tempfile

def default_profile(executable=None,local_appdata=None):
    root=Path(executable or sys.executable).resolve().parent
    for parent in (root,*list(root.parents)[:3]):
        profile=parent/'profile'
        if (parent/'pyproject.toml').is_file() and (profile/'state/musicops.sqlite').is_file():return profile
    location=Path(local_appdata or os.environ.get('LOCALAPPDATA') or Path.home()/'AppData/Local')
    state=location/'MusicOps'/'active-profile.json'
    try:
        if state.stat().st_size<=4096:
            value=json.loads(state.read_text('utf-8'));profile=Path(value['profile'])
            if profile.is_absolute() and (profile/'state/musicops.sqlite').is_file():return profile.resolve()
    except (OSError,ValueError,KeyError,TypeError):pass
    return location/'MusicOps'/'profile'

def remember_profile(base,local_appdata=None):
    location=Path(local_appdata or os.environ.get('LOCALAPPDATA') or Path.home()/'AppData/Local')/'MusicOps'
    location.mkdir(parents=True,exist_ok=True)
    handle,name=tempfile.mkstemp(prefix='active-profile-',suffix='.tmp',dir=location)
    tmp=Path(name)
    try:
        with os.fdopen(handle,'w',encoding='utf-8') as f:json.dump({'profile':str(Path(base).resolve())},f,ensure_ascii=False)
        os.replace(tmp,location/'active-profile.json')
    finally:tmp.unlink(missing_ok=True)
