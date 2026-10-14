"""Generate generic public configuration examples without workstation defaults."""
import json
from pathlib import Path

from musicops.config.schema import default_config


ROOT = Path(__file__).resolve().parents[1]


def schema(value, key=''):
    if isinstance(value, dict):
        return {'type': 'object', 'required': list(value), 'properties': {k: schema(v, k) for k, v in value.items()}, 'additionalProperties': False}
    if isinstance(value, bool):
        return {'type': 'boolean', 'default': value, 'readOnly': key == 'rules_verified'}
    if isinstance(value, int):
        return {'type': 'integer', 'default': value, 'minimum': 0}
    if isinstance(value, float):
        return {'type': 'number', 'default': value}
    if isinstance(value, list):
        return {'type': 'array', 'items': schema(value[0]) if value else {}, 'default': value}
    return {'type': 'string', 'default': value}


def main():
    values = default_config(ROOT / 'profile', offline=False)
    # These are illustrative Windows paths, independent of the build machine.
    for name in ('library', 'source', 'work', 'backup', 'quarantine', 'reports'):
        values['paths'][name] = f'C:/MusicOpsExample/{name}'
    values['paths']['tools'] = {'ffmpeg': '', 'ffprobe': ''}
    values['playlist_ids'] = []
    values['music_servers']['roon'].update(library='', watched_folders=[], enabled=False)
    directory = ROOT / 'config'
    directory.mkdir(exist_ok=True)
    (directory / 'example.json').write_text(json.dumps(values, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    document = {'$schema': 'https://json-schema.org/draft/2020-12/schema', '$id': 'musicops.config.v1', **schema(values)}
    (directory / 'config.schema.json').write_text(json.dumps(document, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')


if __name__ == '__main__':
    main()
