"""Original subprocess adapter for a separately installed musicdl (not bundled).

Only the documented MusicClient interface is used. Third-party stdout is discarded
to avoid storing credentials from third-party diagnostics. A single JSON receipt crosses the boundary.
"""
import contextlib
import json
import os
from pathlib import Path
import re
import sys
import unicodedata


def normalize(value):
    return ' '.join(unicodedata.normalize('NFKC',str(value or '')).casefold().split())


def main():
    request=json.load(sys.stdin);directory=Path(request['directory']).resolve()
    result={'status':'SOURCE_UNAVAILABLE'}
    try:
        with open(os.devnull,'w') as log,contextlib.redirect_stdout(log),contextlib.redirect_stderr(log):
            from musicdl import musicdl
            source=request['client'];target=request['target'];cookies=request.get('cookies',{})
            client=musicdl.MusicClient(music_sources=[source],init_music_clients_cfg={source:{
                'work_dir':str(directory),'search_size_per_source':8,'disable_print':True,
                'max_retries':1,'default_search_cookies':cookies,'default_download_cookies':cookies,
                'default_parse_cookies':cookies}},clients_threadings={source:1})
            rows=client.search(keyword=target['raw_title']+' '+' '.join(target['artists'])).get(source,[])
            matching=[]
            for row in rows:
                duration=getattr(row,'duration_s',None)
                if not duration:
                    try:
                        parts=str(row.duration).split(':');duration=sum(float(p)*60**i for i,p in enumerate(reversed(parts)))
                    except (ValueError,AttributeError):continue
                singers=getattr(row,'singers','');singers=singers if isinstance(singers,list) else re.split(r'\s+/\s+|;|、',str(singers))
                if normalize(row.song_name)!=normalize(target['raw_title']) or {normalize(a) for a in singers}!={normalize(a) for a in target['artists']} or abs(float(duration)-float(target['duration']))>2:continue
                if getattr(row,'album',None) and target.get('album') and normalize(row.album)!=normalize(target['album']):continue
                matching.append(row)
            # Do not choose between distinct recordings just because titles match.
            if len(matching)!=1:result={'status':'AMBIGUOUS' if matching else 'NO_MATCH'}
            else:
                downloaded=client.download([matching[0]])
                if len(downloaded)==1:
                    path=Path(downloaded[0].save_path).resolve()
                    if path.is_relative_to(directory) and path.is_file():
                        result={'status':'OK','path':str(path),'title':str(matching[0].song_name),
                                'artist':(matching[0].singers if isinstance(matching[0].singers,list) else re.split(r'\s+/\s+|;|、',str(matching[0].singers))),'album':str(getattr(matching[0],'album','') or target.get('album',''))}
    except ImportError:result={'status':'NOT_INSTALLED'}
    except Exception as exc:result={'status':'SOURCE_UNAVAILABLE','error_class':type(exc).__name__}
    (directory/'receipt.json').write_text(json.dumps(result),encoding='utf-8')


if __name__=='__main__':main()
