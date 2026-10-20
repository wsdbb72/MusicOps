from pathlib import Path
import pytest
from mutagen.flac import FLAC
from musicops.app.compilations import preview,execute
from musicops.app.duplicates import restore
from musicops.library.index import sha256_file
from musicops.library.catalog import AlbumCatalog
from musicops.media.tags import read_tag_fields
from test_workflows import service,flac


@pytest.fixture(autouse=True)
def qbt_readonly(monkeypatch):
    from musicops.clients.http import Response
    class Session:
        origin='http://localhost:8080'
        rows=[]
        def __init__(self,*_):pass
        def request(self,method,url):
            assert method=='GET' and url.endswith('/api/v2/torrents/info')
            import json
            return Response(200,json.dumps(self.rows).encode())
        def close(self):pass
    monkeypatch.setattr('musicops.clients.qbt_live.QbtSession',Session)
    return Session


def songs(service):
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library']);paths=[]
    for i,artist in enumerate(('原藝人 A','原艺人 B'),1):
        path=flac(root/f'Old{i}/song.flac',title=f'Song{i}',release='original-release')
        tags=FLAC(path);tags['artist']=[artist];tags['album']=[f'最新热歌慢摇{i}'];tags['musicbrainz_releasegroupid']=['old-group'];tags['musicbrainz_trackid']=['song-recording-'+str(i)];tags.save();paths.append(path)
    paths[0].with_suffix('.lrc').write_text('[00:00.00]Song\n[00:01.00]End',encoding='utf-8')
    service._index_real_library({},cfg)
    return cfg,paths,[r['file_id'] for r in service.store.list('library_files')]


def test_compilation_preserves_each_original_artist_and_recording_and_restores(service):
    cfg,paths,ids=songs(service);before={str(p):sha256_file(p) for p in paths}
    plan=preview(service,cfg,{'file_ids':ids,'album':'最新热歌慢摇','albumartist':'群星'})
    assert all(p.exists() and sha256_file(p)==before[str(p)] for p in paths)
    assert plan['preserve_track_artists'] and plan['tracks']==2
    result=execute(service,cfg,plan);target=Path(plan['keep_directory']);tracks=sorted(target.glob('*.flac'))
    assert len(tracks)==2 and not any(p.exists() for p in paths)
    assert [read_tag_fields(p)['artist'] for p in tracks]==[['原藝人 A'],['原艺人 B']]
    for p in tracks:
        tags=FLAC(p);assert tags['album']==['最新热歌慢摇'] and tags['albumartist']==['群星']
        assert not tags.get('musicbrainz_albumid') and not tags.get('musicbrainz_releasegroupid')
        assert tags.get('musicbrainz_trackid') and tags.get('compilation')==['1']
    assert len(list(target.glob('*.lrc')))==1
    assert AlbumCatalog(service.store).summary(cfg['paths']['library'])['track_count']==2
    assert all(Path(r['path']).suffix.lower()=='.flac' for r in service.store.list('library_files'))
    assert execute(service,cfg,plan)['transactions']==result['transactions']
    restored=restore(service,cfg,result['transactions'][0]);assert restored['state']=='ROLLED_BACK'
    assert all(p.exists() and sha256_file(p)==before[str(p)] for p in paths)
    assert paths[0].with_suffix('.lrc').is_file()


def test_changed_original_after_preview_cannot_be_quarantined(service):
    cfg,paths,ids=songs(service)
    plan=preview(service,cfg,{'file_ids':ids,'album':'合辑'})
    tags=FLAC(paths[0]);tags['artist']=['Changed'];tags.save()
    with pytest.raises(Exception,match='原歌曲发生变化'):execute(service,cfg,plan)
    assert all(p.is_file() for p in paths) and not Path(plan['keep_directory']).exists()


def test_compilation_rejects_path_injection_and_missing_original_artist(service):
    cfg,paths,ids=songs(service)
    with pytest.raises(ValueError):preview(service,cfg,{'file_ids':ids,'album':'../outside'})
    tags=FLAC(paths[0]);del tags['artist'];tags.save();service._index_real_library({},cfg)
    with pytest.raises(Exception,match='原艺人'):preview(service,cfg,{'file_ids':ids,'album':'合辑'})


def test_configured_seed_payload_cannot_be_moved_or_retagged(service,qbt_readonly):
    cfg,paths,ids=songs(service);service.vault.set(cfg['qbt']['credential_ref'],'synthetic credentials','127.0.0.1')
    qbt_readonly.rows=[{'content_path':str(paths[0].parent)}];before=sha256_file(paths[0])
    with pytest.raises(Exception,match='仍用于做种'):preview(service,cfg,{'file_ids':ids,'album':'合辑'})
    assert paths[0].is_file() and sha256_file(paths[0])==before


def test_torrent_ownership_is_checked_again_after_preview(service,qbt_readonly):
    cfg,paths,ids=songs(service);service.vault.set(cfg['qbt']['credential_ref'],'synthetic credentials','127.0.0.1')
    plan=preview(service,cfg,{'file_ids':ids,'album':'合辑'})
    qbt_readonly.rows=[{'content_path':str(paths[1])}]
    with pytest.raises(Exception,match='仍用于做种'):execute(service,cfg,plan)
    assert all(p.is_file() for p in paths) and not Path(plan['keep_directory']).exists()


def test_duplicate_merge_cleans_temporary_work_and_gui_selection_feedback(service):
    from musicops.app.duplicates import scan,merge
    from test_review_dialog import app,Owner
    from musicops.gui.duplicates import DuplicatePage
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library'])
    a=flac(root/'Copy1/01 Song.flac',title='Song');b=flac(root/'Copy2/01 Song.flac',title='Song')
    tags=FLAC(a);tags['genre']=['Pop'];tags.save()
    report=scan(service,cfg);assert report['ready_count']==1
    plan_id=report['groups'][0]['plan_id']
    work_dir=Path(cfg['paths']['work'])/plan_id
    assert work_dir.is_dir() and list(work_dir.rglob('*.flac'))
    merged=merge(service,cfg,{'scan_id':report['scan_id'],'group_ids':[report['groups'][0]['group_id']]})
    assert merged['merged_groups']==1 and not list(work_dir.rglob('*.flac'))
    restore_res=restore(service,cfg,merged['transactions'][0]);assert restore_res['state']=='ROLLED_BACK'
    assert a.is_file() and b.is_file()

