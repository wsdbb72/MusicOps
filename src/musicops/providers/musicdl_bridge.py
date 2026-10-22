"""Opt-in external musicdl; validate every result in MusicOps before publication."""
import json
import os
from pathlib import Path
import subprocess
import time
from musicops.clients.http import HttpError
from musicops.providers.streaming import matches
from musicops.library.paths import reject_reparse_chain
from musicops.library.index import sha256_file
from musicops.media.tags import read_tag_fields
from musicops.clients.process_owner import ProcessOwner

CLIENTS={'tidal':'TIDALMusicClient','qobuz':'QobuzMusicClient','deezer':'DeezerMusicClient',
         'soundcloud':'SoundCloudMusicClient','joox':'JooxMusicClient','audius':'AudiusMusicClient',
         'qianqian':'QianqianMusicClient','streetvoice':'StreetVoiceMusicClient'}


def download(settings,platform,target,directory,validator,checkpoint,secret_getter):
    if not settings.get('enabled') or not settings.get('python_path'):
        raise HttpError('NOT_CONFIGURED','新平台需先配置独立安装 musicdl 的 Python；程序不捆绑第三方限制分发的软件')
    executable=Path(settings['python_path'])
    if not executable.is_absolute() or not executable.is_file():raise HttpError('NOT_CONFIGURED','musicdl Python 路径无效')
    directory=Path(directory).resolve();directory.mkdir(parents=True,exist_ok=True)
    cookies={}
    ref=settings.get('credential_refs',{}).get(platform)
    if ref:
        try:cookies=json.loads(secret_getter(ref,'musicdl:'+platform))
        except KeyError:pass
    payload={'client':CLIENTS[platform],'target':target,'directory':str(directory),'cookies':cookies}
    with open(os.devnull,'wb') as log:
        process=subprocess.Popen([str(executable),'-X','utf8',str(Path(__file__).with_name('musicdl_runner.py'))],
            stdin=subprocess.PIPE,stdout=log,stderr=log,cwd=directory,
            creationflags=0x08000000 if os.name=='nt' else 0,start_new_session=os.name!='nt')
        owner=None
        try:
            owner=ProcessOwner(process)
            process.stdin.write(json.dumps(payload).encode());process.stdin.close();started=time.monotonic()
            while process.poll() is None:
                checkpoint('external_musicdl')
                if time.monotonic()-started>120:raise HttpError('NETWORK_ERROR','此平台在 120 秒内未完成，保留其他来源的处理机会')
                time.sleep(.2)
        finally:
            if owner:owner.close()
            else:
                if process.poll() is None:process.kill()
                process.wait()
    receipt=directory/'receipt.json'
    if not receipt.is_file() or receipt.stat().st_size>50000:raise HttpError('SOURCE_UNAVAILABLE','musicdl 未返回有效结果')
    result=json.loads(receipt.read_text('utf-8'))
    if result.get('status')!='OK':raise HttpError(result.get('status','SOURCE_UNAVAILABLE'),'musicdl 未找到可核验的完整音源')
    path=Path(result['path']).resolve();reject_reparse_chain(path)
    if not path.is_relative_to(directory) or not path.is_file():raise HttpError('INVALID_PAYLOAD','音频必须位于本次临时目录')
    checked=validator.validate(path)
    if checked.get('decode_state')!='passed' or not checked.get('roon_compatible') or not checked.get('duration') or abs(checked['duration']-target['duration'])>2:
        raise HttpError('INCOMPLETE_AUDIO','音源未通过完整时长、解码和容器校验')
    tags=read_tag_fields(path)
    observed={'name':(tags.get('title') or [result['title']])[0],'artist':' / '.join(tags.get('artist') or result['artist']),
              'album':(tags.get('album') or [result['album']])[0],'duration':checked['duration']}
    if not matches(target,observed):raise HttpError('IDENTITY_CONFLICT','下载后的标签与歌曲身份不符')
    return path,{'source':platform,'engine':'external-musicdl','duration':checked['duration'],'sha256':sha256_file(path)}
