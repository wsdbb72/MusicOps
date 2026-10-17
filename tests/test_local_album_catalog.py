import copy
from pathlib import Path
from types import SimpleNamespace
import pytest
from musicops.library.catalog import AlbumCatalog, root_key
from musicops.state.store import StateStore
from musicops.app.service import AppService


def record(root, store, name, album='Album', duration=120):
    path=root/name;path.parent.mkdir(parents=True,exist_ok=True);path.write_bytes(b'synthetic no real audio')
    stat=path.stat();row={'file_id':str(path),'path':str(path),'location_state':'library','raw_title':path.stem,'artists':['Artist'],'album':album,'duration':duration,'size':stat.st_size,'mtime_ns':stat.st_mtime_ns}
    store.put('library_files',row['file_id'],row);return row


def target(row, target_id='selected'):
    return {**{k:row[k] for k in ('raw_title','artists','album','duration')},'target_id':target_id}


def test_album_database_groups_discs_and_persists_one_to_one_track_mapping(tmp_path):
    root=tmp_path/'music';store=StateStore(tmp_path/'state.sqlite');catalog=AlbumCatalog(store)
    rows=[record(root,store,'Artist/Album/CD1/Song One.flac'),record(root,store,'Artist/Album/CD2/Song Two.flac')]
    summary=catalog.sync(root,rows)
    assert (summary['album_count'],summary['track_count'],summary['matched_track_count'])==(1,2,2)
    album=catalog.albums(root)['albums'][0]
    assert album['track_count']==2 and len(album['file_ids'])==2 and album['release_confirmed'] is False
    reopened=AlbumCatalog(StateStore(tmp_path/'state.sqlite'))
    assert reopened.matched_targets(root,[target(row,str(i)) for i,row in enumerate(rows)])=={'0','1'}
    with store.connection() as c:
        plan=c.execute('EXPLAIN QUERY PLAN SELECT file_id FROM library_matches WHERE root_key=? AND title_key=? AND artist_key=? AND album_key=?',('','','','')).fetchall()
    assert any('library_match_identity' in str(tuple(row)) for row in plan)


def test_unchanged_album_cache_reuses_normalization_and_changed_tags_rebuild(tmp_path,monkeypatch):
    root=tmp_path/'music';store=StateStore(tmp_path/'state.sqlite');catalog=AlbumCatalog(store);row=record(root,store,'Song.flac')
    catalog.sync(root,[row]);original=catalog.summary(root)['updated_at']
    with monkeypatch.context() as patch:
        patch.setattr('musicops.library.catalog.strict_text',lambda *a:pytest.fail('unchanged cache must not normalize every track again'))
        assert catalog.sync(root,[row])['reused'] is True
    changed=copy.deepcopy(row);changed['album']='Corrected Album';store.put('library_files',row['file_id'],changed)
    assert catalog.sync(root,[changed])['reused'] is False
    assert catalog.matched_targets(root,[target(row)])==set()
    assert catalog.matched_targets(root,[target(changed)])=={'selected'}


def test_missing_changed_or_incomplete_files_are_never_reported_present(tmp_path):
    root=tmp_path/'music';store=StateStore(tmp_path/'state.sqlite');catalog=AlbumCatalog(store)
    good=record(root,store,'Song.flac');unknown=record(root,store,'Unknown.flac',album='')
    assert catalog.sync(root,[good,unknown])['matched_track_count']==1
    assert not catalog.matched_targets(root,[{**target(good),'duration':130}])
    Path(good['path']).write_bytes(b'changed after scan')
    assert not catalog.matched_targets(root,[target(good)])
    Path(good['path']).unlink();assert not catalog.matched_targets(root,[target(good)])
    catalog.sync(root,[unknown]);assert catalog.albums(root)['total']==1


def test_album_index_separates_roots_and_excludes_invalid_duration(tmp_path):
    store=StateStore(tmp_path/'state.sqlite');catalog=AlbumCatalog(store)
    first=record(tmp_path/'one',store,'Song.flac');second=record(tmp_path/'two',store,'Song.flac',duration=0)
    catalog.sync(tmp_path/'one',[first]);catalog.sync(tmp_path/'two',[second])
    assert catalog.summary(tmp_path/'one')['matched_track_count']==1
    assert catalog.summary(tmp_path/'two')['matched_track_count']==0
    assert catalog.matched_targets(tmp_path/'two',[target(first)])==set()


def test_playlist_preparation_uses_persisted_local_album_index_and_reuses_audio_tags(tmp_path,monkeypatch):
    import mutagen
    monkeypatch.setattr(AppService,'_loop',lambda self:self.stop.wait())
    service=AppService(tmp_path/'profile',offline=False);current=service.config.current();cfg=copy.deepcopy(current['values'])
    library=Path(cfg['paths']['library']);library.mkdir();(library/'Song.flac').write_bytes(b'synthetic metadata fixture');calls=[]
    def read(*args,**kwargs):
        calls.append(1);return SimpleNamespace(tags={'title':['Song'],'album':['Album'],'artist':['Artist']},info=SimpleNamespace(length=120),pictures=[])
    monkeypatch.setattr(mutagen,'File',read)
    service.store.put('target_tracks','target',{'target_id':'target','raw_title':'Song','artists':['Artist'],'album':'Album','duration':120})
    try:
        first=service.workflows.start({'scene':'playlist','target_ids':['target']},cfg)
        assert first['album_count']==0 and len(calls)==1
        assert service.dispatch('list_library_albums')['summary']['matched_track_count']==1
        second=service.workflows.start({'scene':'playlist','target_ids':['target']},cfg)
        assert second['album_count']==0 and len(calls)==1
    finally:service.stop.set();service.thread.join(2)
