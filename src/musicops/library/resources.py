"""Type-aware sidecar relationships; shared covers never follow a single track removal."""
import re
from pathlib import Path

from .paths import safe_destination, reparse

IMAGE_EXTENSIONS = {'.jpg', '.jpeg', '.png', '.webp', '.bmp'}


def companion_relations(audio):
    audio = Path(audio)
    relations = []
    if not audio.parent.is_dir():
        return relations
    for path in sorted(audio.parent.iterdir()):
        if reparse(path) or not path.is_file():
            continue
        suffix = path.suffix.casefold()
        if suffix == '.lrc' and path.stem.casefold() == audio.stem.casefold():
            relations.append({'schema_version': 1, 'path': str(path), 'type': 'lyrics', 'scope': 'track', 'shared': False})
        elif suffix in IMAGE_EXTENSIONS and path.stem.casefold() in {'cover', 'folder', 'front', audio.stem.casefold()}:
            relations.append({'schema_version': 1, 'path': str(path), 'type': 'cover', 'scope': 'release', 'shared': True})
        elif suffix == '.cue':
            try:
                text = path.read_text(encoding='utf-8-sig')
            except (UnicodeError, OSError) as exc:
                relations.append({'schema_version': 1, 'path': str(path), 'type': 'cue', 'scope': 'release', 'shared': True, 'read_error': 'ENCODING_UNKNOWN' if isinstance(exc, UnicodeError) else 'FILE_READ_ERROR'})
                continue
            references = re.findall(r'^\s*FILE\s+(?:"([^"]+)"|(\S+))\s+\S+', text, re.I | re.M)
            names = [a or b for a, b in references]
            if any(Path(name).name.casefold() == audio.name.casefold() for name in names):
                relations.append({'schema_version': 1, 'path': str(path), 'type': 'cue', 'scope': 'release', 'shared': True, 'file_references': names})
    return relations


def rewrite_cue_reference(text, old_name, new_name, root):
    safe_destination(root, new_name)
    def rewrite(match):
        name = match.group(2) or match.group(3)
        if name.casefold() != old_name.casefold():
            return match.group(0)
        return f'{match.group(1)}"{new_name}"{match.group(4)}'
    result, _ = re.subn(r'(^\s*FILE\s+)(?:"([^"]+)"|(\S+))(\s+\S+)', rewrite, text, flags=re.I | re.M)
    if result == text:
        raise ValueError('CUE没有引用待改名音频')
    for first, second in re.findall(r'^\s*FILE\s+(?:"([^"]+)"|(\S+))\s+\S+', result, re.I | re.M):
        safe_destination(root, first or second)
    return result
