"""Persist platform-isolated playlist manifests into the shared album workflow."""
import copy,hashlib,json,math,re
from pathlib import Path
from musicops.clients.http import SafeHttpTransport,PolicyBlocked
from musicops.config.schema import digest
from musicops.state.store import utcnow
from musicops.providers.playlist_input import PLATFORMS,INPUT_HOSTS,parse_playlist_locator,resolve_share


def use_platform_reader(args):
    if args.get('file_path') or args.get('platform','auto') not in ('auto','netease'):return True
    if args.get('playlist_input'):
        locators=parse_playlist_locator(args['playlist_input'],args.get('platform','auto'))
        return any(item['platform']!='netease' or not item.get('playlist_id') for item in locators)
    return False


def reader(service,platform,cfg):
    if platform=='netease':
        from musicops.providers.netease_readonly import NetEaseReadOnlyProvider
        if not cfg['providers']['netease']['enabled']:raise PolicyBlocked('网易云歌单来源已关闭，请在设置中启用')
        return NetEaseReadOnlyProvider(SafeHttpTransport(INPUT_HOSTS['netease'],offline=False),cfg['providers']['netease'],service.vault.get,service._checkpoint,getattr(service,'_update_task_progress',None))
    settings=cfg['playlist_sources'][platform]
    if platform in ('qqmusic','qishui','kugou','kuwo','migu'):
        from musicops.providers.playlist_domestic import DomesticPlaylistProvider,DOMESTIC_HOSTS
        return DomesticPlaylistProvider(platform,SafeHttpTransport(set(DOMESTIC_HOSTS)|INPUT_HOSTS[platform],offline=False),settings,service.vault.get,service._checkpoint)
    from musicops.providers.playlist_global import GlobalPlaylistProvider,HOSTS
    return GlobalPlaylistProvider(platform,SafeHttpTransport(set(HOSTS[platform])|INPUT_HOSTS[platform],offline=False),settings,service.vault.get,service._checkpoint)


def _song(raw,source):
    if not isinstance(raw,dict):raise ValueError('歌单曲目结构无效')
    source=raw.get('source') or source
    if not isinstance(source,str) or not re.fullmatch(r'[a-z][a-z0-9_-]{0,31}',source):raise ValueError('歌单曲目的平台来源无效')
    original_id=raw.get('source_song_id');song_id=str(original_id or raw.get('id') or raw.get('target_id') or '')
    if song_id.startswith(source+':'):song_id=song_id[len(source)+1:]
    if not song_id or len(song_id)>512 or any(c in song_id for c in '\0\r\n'):raise ValueError('歌单曲目的来源 ID 无效')
    keys=('raw_title','artists','album','source_album_id','duration','version','recording_version','track','disc','date_raw','metadata_complete','metadata_missing_fields','evidence_level','source_url','isrc','uploader','availability')
    song={key:copy.deepcopy(raw[key]) for key in keys if key in raw}
    song.update(id=song_id,target_id=source+':'+song_id,source=source,source_song_id=str(original_id) if original_id is not None else None)
    title=song.get('raw_title','');album=song.get('album') or '';artists=song.get('artists',[])
    if not isinstance(title,str) or len(title)>2000 or not isinstance(album,str) or len(album)>2000:raise ValueError('歌单曲目名称过长或格式无效')
    if not isinstance(artists,list) or len(artists)>100 or any(not isinstance(a,str) or len(a)>500 for a in artists):raise ValueError('歌单艺人字段格式无效')
    song['raw_title']=title;song['album']=album;song['artists']=artists
    duration=song.get('duration')
    if duration is not None and (type(duration) not in (int,float) or not math.isfinite(duration) or not 0<duration<604800):song['duration']=None
    missing=[key for key,present in (('title',bool(title.strip())),('artists',bool(artists)),('album',bool(album.strip())),('duration',bool(song.get('duration')))) if not present]
    song['metadata_missing_fields']=missing;song['metadata_complete']=not missing
    return song


def refresh_platform_playlists(service,args,cfg):
    imported=bool(args.get('file_path'));locators=[];outcomes=[]
    if imported:
        from musicops.providers.playlist_files import import_playlist_file
        path=Path(args['file_path'])
        if not path.is_absolute():raise ValueError('请选择完整的歌单文件路径')
        service._checkpoint('before_playlist_file')
        outcome=import_playlist_file(path,platform=args.get('platform','auto'),text_order=args.get('text_order'),checkpoint=service._checkpoint)
        outcomes.append((outcome,{'platform':outcome.get('snapshot',{}).get('source','imported'),'playlist_id':'file:'+hashlib.sha256(str(path.resolve()).encode()).hexdigest()}))
    else:
        locators=parse_playlist_locator(args.get('playlist_input') or args.get('playlist_id',''),args.get('platform','auto'))
        for locator in locators:
            service._checkpoint('before_platform_playlist')
            platform=locator['platform']
            locator=resolve_share(locator,SafeHttpTransport(INPUT_HOSTS[platform],offline=False),service._checkpoint)
            provider=reader(service,platform,cfg)
            outcome=provider.refresh(locator['playlist_id'],batch_size=cfg['netease']['batch_size']) if platform=='netease' else provider.refresh(locator)
            outcomes.append((outcome,locator))
    targets={};summaries=[];complete_ids=set();all_complete=True;errors=[];track_count=0
    for outcome,locator in outcomes:
        snapshot=outcome.get('snapshot');platform=locator['platform'];status=outcome.get('status','OK')
        if not isinstance(snapshot,dict):
            errors.append(outcome.get('reason') or outcome.get('error') or outcome.get('status','读取失败'));all_complete=False
            summaries.append({'platform':platform,'status':outcome.get('status','PARSE_ERROR'),'reason':errors[-1]});continue
        if status not in ('OK','SNAPSHOT_INCOMPLETE'):
            errors.append(outcome.get('reason') or outcome.get('error') or '歌单读取未完成：'+str(status))
        songs=snapshot.get('songs')
        if not isinstance(songs,list) or len(songs)>50000:raise ValueError('歌单超过 50000 首或返回结构无效')
        track_count+=len(songs)
        if track_count>50000:raise ValueError('一次最多导入 50000 首，请分批导入歌单')
        complete=snapshot.get('complete') is True and status=='OK';all_complete=all_complete and complete
        membership=locator['playlist_id'] if platform=='netease' and not imported else platform+':playlist:'+str(locator['playlist_id'])
        snapshot=copy.deepcopy(snapshot);snapshot['complete']=complete;snapshot['songs']=[_song(song,platform) for song in songs]
        sid=snapshot.get('snapshot_id') or platform+':'+digest({'membership':membership,'songs':snapshot['songs'],'complete':complete})
        snapshot.update(snapshot_id=sid,source=platform,fetched_at=snapshot.get('fetched_at') or utcnow(),hash=digest(snapshot))
        if complete:service.store.put('playlist_snapshots',sid,snapshot);complete_ids.add(membership)
        else:service.store.put('evidence','incomplete:'+sid,{'kind':'incomplete_platform_snapshot','snapshot':snapshot})
        summaries.append({'platform':platform,'name':snapshot.get('name',''),'status':status,'reason':outcome.get('reason',''),'complete':complete,'declared_count':snapshot.get('declared_count'),'song_count':len(songs),'warnings':snapshot.get('warnings',[])[:100]})
        for song in snapshot['songs']:
            target=copy.deepcopy(song);target.update(snapshot_id=sid,snapshot_ids=[sid],snapshot_complete=complete,playlist_ids=[membership])
            prior=targets.get(target['target_id'])
            if prior:target.update(snapshot_ids=prior['snapshot_ids']+[sid],playlist_ids=prior['playlist_ids']+[membership],snapshot_complete=prior['snapshot_complete'] and complete)
            targets[target['target_id']]=target
    if len(json.dumps(targets,ensure_ascii=False).encode('utf-8'))>40_000_000:raise ValueError('歌单元数据超过本机传输限制，请分批导入')
    membership=service._persist_playlist_targets(targets,complete_ids)
    reason='；'.join(errors) if errors else '' if all_complete else '歌单仅返回可见部分或缺少完整数量证据；可选择已显示曲目，不能把它当成完整歌单。'
    return {'status':'OK' if all_complete else 'SNAPSHOT_INCOMPLETE','complete':all_complete,'snapshots':summaries,'targets':list(targets.values()),'track_count':track_count,'reason':reason,**membership,'read_only':True,'music_writes':0}
