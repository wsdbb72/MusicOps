"""One-time read-model repair; original audio, work copies and plans stay intact."""
import json
from pathlib import Path
from musicops.media.source_labels import normalize_album_artist,album_artist_credits,album_label,has_source_credit
from musicops.domain.errors import MusicOpsError

KEY='source-albumartist-repair-v1'
_HINTS=('qobuz','tidal','deezer','spotify','apple music','amazon music')


def normalize_job(job):
    changed=False
    for key in ('old_files','targets','prepared'):
        rows=job.get(key) or []
        if not isinstance(rows,list):continue
        for row in rows:
            if not isinstance(row,dict):continue
            changed=normalize_album_artist(row) or changed
            tags=row.get('tags') or {}
            if key=='prepared' and isinstance(tags,dict):changed=normalize_album_artist(row,tags.get('albumartist',[])) or changed
    artists=job.get('albumartists')
    if has_source_credit(artists):
        job.setdefault('raw_albumartists',artists);job['albumartists']=album_artist_credits(artists);changed=True
    if changed:
        first=next((row for key in ('prepared','old_files','targets') for row in (job.get(key) if isinstance(job.get(key),list) else []) if isinstance(row,dict)),None)
        if first:
            label=album_label({**first,'album':job.get('album') or first.get('album')})
            if label!=job.get('label'):
                job.setdefault('source_repair_previous_label',job.get('label',''));job['label']=label
        job['ai_assistance']=None
    return changed


def run(service,progress=None,*,force=False):
    store=service.store;previous=store.get('evidence',KEY) or {}
    if previous.get('state')=='completed' and not force:return previous
    if progress:progress('正在更新专辑来源标记索引；原始标签和文件事务保留')
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library']).absolute()
    planned=set(previous.get('file_ids',[]));files=[];jobs=[]
    with store.connection() as connection:
        for index,row in enumerate(connection.execute("SELECT id,json_quote(json_extract(payload,'$.albumartist')),json_extract(payload,'$.path') FROM library_files WHERE coalesce(json_extract(payload,'$.location_state'),'library')='library'")):
            key,artists,path=row
            if not isinstance(path,str) or not Path(path).absolute().is_relative_to(root):continue
            marker={'albumartist':json.loads(artists) if artists is not None else []}
            if normalize_album_artist(marker):planned.add(key)
            if progress and index and index%10000==0:progress('正在检查专辑来源标记：'+str(index)+' 个索引条目；无需解码音频')
        for key in planned:
            raw=connection.execute('SELECT payload FROM library_files WHERE id=?',(key,)).fetchone()
            if not raw:continue
            row=json.loads(raw[0])
            if not isinstance(row.get('path'),str) or not Path(row['path']).absolute().is_relative_to(root):continue
            normalize_album_artist(row);files.append((key,row))
        for key,label in connection.execute("SELECT id,label FROM workflow_overview WHERE hidden=0 AND job_kind NOT IN ('batch','activity')"):
            if not any(hint in str(label).casefold() for hint in _HINTS):continue
            row=json.loads(connection.execute('SELECT payload FROM evidence WHERE id=?',(key,)).fetchone()[0])
            if normalize_job(row):jobs.append((key,row))
    intent={'kind':'source_artist_migration','state':'planned','file_ids':sorted(planned)}
    store.put('evidence',KEY,intent)
    store.put_many('library_files',files);store.put_many('evidence',jobs)
    catalog='unchanged'
    if files:
        try:
            from musicops.library.catalog import AlbumCatalog
            AlbumCatalog(store).refresh_files(root,[row for _,row in files]);catalog='refreshed'
        except (OSError,ValueError,RuntimeError,MusicOpsError):
            from musicops.library.catalog import root_key
            key='library-catalog:'+root_key(root);entry=store.get('evidence',key)
            if entry:store.put('evidence',key,{**entry,'updated_at':None,'fingerprint':None})
            catalog='deferred_to_index'
    result={**intent,'state':'completed','file_count':len(files),'job_count':len(jobs),'catalog':catalog,'audio_writes':0,'transaction_writes':0}
    result=store.put('evidence',KEY,result);store.event('source_albumartist_repaired',{key:result[key] for key in ('file_count','job_count','catalog','audio_writes','transaction_writes')})
    return result
