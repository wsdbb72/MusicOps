"""Stable profile selection for the installed or portable desktop application."""
from pathlib import Path
import os,sys,json,tempfile
from musicops.platforms import is_macos,user_data_root,private_directory

def default_profile(executable=None,local_appdata=None):
    root=Path(executable or sys.executable).resolve().parent
    if not is_macos() and (root/'profile').is_dir() and (executable or getattr(sys,'frozen',False)):
        return root/'profile'
    for parent in (() if is_macos() else (root,*list(root.parents)[:3])):
        profile=parent/'profile'
        if (parent/'pyproject.toml').is_file() and (profile/'state/musicops.sqlite').is_file():return profile
    location=user_data_root(local_appdata)
    state=location/'MusicOps'/'active-profile.json'
    try:
        if state.stat().st_size<=4096:
            value=json.loads(state.read_text('utf-8'));profile=Path(value['profile'])
            if profile.is_absolute() and not (is_macos() and any(p.endswith('.app') for p in profile.parts)) and (profile/'state/musicops.sqlite').is_file():return profile.resolve()
    except (OSError,ValueError,KeyError,TypeError):pass
    return location/'MusicOps'/'profile'

def remember_profile(base,local_appdata=None):
    location=user_data_root(local_appdata)/'MusicOps'
    private_directory(location)
    handle,name=tempfile.mkstemp(prefix='active-profile-',suffix='.tmp',dir=location)
    tmp=Path(name)
    try:
        with os.fdopen(handle,'w',encoding='utf-8') as f:json.dump({'profile':str(Path(base).resolve())},f,ensure_ascii=False)
        os.replace(tmp,location/'active-profile.json')
    finally:tmp.unlink(missing_ok=True)
