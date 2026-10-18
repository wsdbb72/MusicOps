"""Resume verified tracks, then try configured full-audio sources independently."""
import hashlib,uuid
from pathlib import Path
import httpx
from musicops.clients.http import HttpError,PolicyBlocked
from musicops.library.index import sha256_file
from musicops.library.paths import reject_reparse_chain
from musicops.library.transactions import durable_copy
from musicops.providers.streaming import StreamingProvider,NAMES
MESSAGES={'NO_MATCH':'没有唯一完整匹配','AMBIGUOUS':'有多个无法区分的音源','AUTH_REQUIRED':'服务需要登录','AUTH_FAILED':'登录状态失效',
          'INCOMPLETE_AUDIO':'试听或音频不完整','SOURCE_UNAVAILABLE':'来源暂不可用','API_CONTRACT_ERROR':'返回结构改变','IDENTITY_CONFLICT':'音源身份不一致',
          'POLICY_BLOCKED':'身份或配置未通过核对','NETWORK_ERROR':'连接失败','ValueError':'资料或格式不支持','OSError':'本机文件或工具异常'}

def run(workflows,job):
    from musicops.providers.netease_download import download_track,import_match
    from musicops.providers.netease_match import resolve_netease_target
    cfg=job['config'];source=Path(cfg['paths']['source'])/job['job_id'];reject_reparse_chain(source);source.mkdir(parents=True,exist_ok=True)
    if job['scene']=='playlist' and not job.get('album_manifest_verified'):
        first=job['targets'][0]
        if first.get('source') in (None,'netease') and first.get('source_album_id'):
            from musicops.providers.netease_readonly import NetEaseReadOnlyProvider
            from musicops.clients.http import SafeHttpTransport
            from musicops.app.pipeline import identity
            try:
                options={**cfg['netease'],'base_url':'https://music.163.com'}
                album=NetEaseReadOnlyProvider(SafeHttpTransport(['music.163.com'],offline=False),options,workflows.s.vault.get).album(first['source_album_id'])
                if all(any(identity(t)==identity(r) and t.get('duration') and abs(t['duration']-r['duration'])<=2 for r in album) for t in job['targets']):
                    workflows.put(job,selected_track_count=len(job['targets']),targets=album,target_count=len(album),album_manifest_verified=True,expected_album_tracks=album)
            except (HttpError,ValueError,KeyError):
                workflows.put(job,album_manifest_note='专辑清单未能完全核验，本次仅处理歌单明确选择的曲目；可用“补全整张专辑”继续核对缺曲')
    workflows.put(job,source_path=str(source),stage='running',resume_stage='fallback',message='正在逐曲核对并获取完整音源')
    settings=cfg['streaming'];validator=workflows.s._validator(cfg);outcomes=job.get('streaming_outcomes') or {}
    targets=job['targets']
    if job['scene']=='fill':
        from musicops.app.pipeline import identity
        targets=[t for t in targets if not any(identity(t)==identity(r) and t.get('duration') and r.get('duration') and abs(t['duration']-r['duration'])<=2 for r in job['old_files'])]
        # Validate against the complete manifest later, including preserved local audio.
    for i,target in enumerate(targets):
        workflows.checkpoint(job,'streaming_next');key=hashlib.sha256(str(target.get('target_id') or target.get('source_song_id') or i).encode()).hexdigest()[:32]
        target['download_file_id']=key;record_id='streaming-track:'+job['job_id']+':'+key;previous=workflows.store.get('evidence',record_id)
        if previous:
            path=source/Path(previous.get('filename','')).name;reject_reparse_chain(path)
            if path.is_file() and sha256_file(path)==previous.get('sha256'):continue
            raise PolicyBlocked('此前已完成曲目发生变化，请先核对文件；不会静默覆盖')
        from musicops.app.pipeline import identity
        local=next((r for r in job.get('existing_album_files',[]) if identity(r)==identity(target) and r.get('duration') and target.get('duration') and abs(r['duration']-target['duration'])<=2),None)
        imported=local['path'] if local else import_match(target,cfg['workflow']['netease_import_dir']);order=settings['order'] if settings['enabled'] else ['netease'];attempts=[];result=None;proof=None
        for platform in (['import'] if imported else [])+order:
            workflows.checkpoint(job,'streaming_'+platform)
            directory=Path(cfg['paths']['work'])/'streaming'/job['job_id']/key/(platform+'-'+uuid.uuid4().hex[:8]);reject_reparse_chain(directory);directory.mkdir(parents=True,exist_ok=True)
            try:
                if platform=='import':result=Path(imported);proof={'source':'client-import'}
                elif platform=='netease':
                    reference=resolve_netease_target(target,cfg,workflows.s.vault.get,lambda phase:workflows.checkpoint(job,phase));target['netease_match']=reference
                    result=Path(download_track(reference,cfg,directory,workflows.s.vault.get,lambda phase:workflows.checkpoint(job,phase)));proof={'source':'netease','source_song_id':reference['source_song_id']}
                else:
                    provider=StreamingProvider(settings,workflows.s.vault.get)
                    result,proof=provider.download(platform,target,directory,validator,lambda phase:workflows.checkpoint(job,phase))
                if result.suffix.lower()!='.ncm':
                    validation=validator.validate(result)
                    if validation.get('decode_state')!='passed' or not validation.get('roon_compatible') or not target.get('duration') or not validation.get('duration') or abs(validation['duration']-target['duration'])>2:
                        raise HttpError('INCOMPLETE_AUDIO','音源未通过完整时长和解码核对')
                destination=source/(key+result.suffix.lower());durable_copy(result,destination)
                proof.update(filename=destination.name,sha256=sha256_file(destination),confirmed=True)
                target['download_source']=proof['source']
                workflows.store.put('evidence',record_id,proof);attempts.append({'source':platform,'status':'OK'});break
            except (HttpError,PolicyBlocked,httpx.HTTPError,OSError,ValueError) as exc:
                attempts.append({'source':platform,'status':getattr(exc,'error_class',type(exc).__name__)});result=None
        outcomes[key]=attempts;workflows.put(job,targets=job['targets'],streaming_outcomes=outcomes,message=f'完整音源获取：{i+1} / {len(targets)} 首')
        if result is None:
            workflows.put(job,stage='needs_review',review_kind='error',message='已尝试配置的流媒体，当前曲目没有可核验的完整音源',
                detail='；'.join(NAMES.get(r['source'],r['source'])+'：'+MESSAGES.get(r['status'],r['status']) for r in attempts)+'。已完成曲目已缓存；重试只处理尚未完成的曲目。');return
    if job['scene']=='fill':
        for i,row in enumerate(job['old_files']):
            if sha256_file(row['path'])!=row.get('original_hash'):raise PolicyBlocked('待补全的原专辑已经改变')
            durable_copy(row['path'],source/('preserved-'+str(i)+Path(row['path']).suffix))
    workflows.put(job,stage='metadata',progress=55,message='完整音源获取完成，开始整理与逐曲验证')
