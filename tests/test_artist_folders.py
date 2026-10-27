import copy
from pathlib import Path
from mutagen.flac import FLAC
from musicops.app.artist_folders import folder_artist
from test_workflows import service,flac,until_terminal  # noqa: F401  (fixture)


def album(path,title='Song',artist='陈奕迅',album_name='阴天快乐',albumartist=None):
    song=flac(path,title=title)
    tags=FLAC(song);tags['artist']=[artist];tags['album']=[album_name];tags['albumartist']=[albumartist or artist];tags.save()
    return song


def run(service,cfg=None):
    cfg=cfg or service.config.current()['values']
    result=service.workflows.start({'scene':'complete','refresh_resources':True},cfg)
    rows=[]
    for _ in range(result['album_count']):rows.append(until_terminal(service,12))
    return result,rows


def library_paths(service):
    return sorted(Path(r['path']).as_posix() for r in service.store.list('library_files') if r.get('location_state')=='library')


def test_joined_credits_become_one_slash_folder():
    assert folder_artist(['陈奕迅'])=='陈奕迅'
    assert folder_artist(['A feat. B'])==folder_artist(['A','B'])==folder_artist(['A & B'])=='A／B'
    assert folder_artist(['A','a','B ft. C'])=='A／B／C'
    assert folder_artist(['Florence and the Machine'])=='Florence and the Machine'


def test_root_album_folder_moves_with_its_cover_cue_and_log(service):
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library'])
    source=root/'陈奕迅 - 阴天快乐 (2010)';album(source/'01 Song.flac')
    (source/'folder.log').write_text('rip log','utf-8');(source/'Scans').mkdir();(source/'Scans'/'back.png').write_bytes(b'png')
    result,rows=run(service)
    assert rows[0]['stage']=='completed',rows[0]
    target=root/'陈奕迅'/'阴天快乐'
    assert (target/'01 Song.flac').is_file() and (target/'folder.log').read_text('utf-8')=='rip log' and (target/'Scans'/'back.png').is_file()
    assert not source.exists() and '陈奕迅/阴天快乐' in rows[0]['detail'].replace('\\','/')
    assert library_paths(service)==[(target/'01 Song.flac').as_posix()]


def test_loose_root_files_and_collaboration_folder(service):
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library'])
    song=album(root/'Duet.flac',title='Duet',artist='A & B',album_name='Together',albumartist='A feat. B')
    song.with_suffix('.lrc').write_text('[00:00.00]x\n[00:01.00]y','utf-8')
    result,rows=run(service)
    target=root/'A／B'/'Together'
    assert rows[0]['stage']=='completed',rows[0]
    assert (target/'Duet.flac').is_file() and (target/'Duet.lrc').is_file() and not song.exists()


def test_albums_already_in_artist_folders_and_ambiguous_folders_stay(service):
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library'])
    kept=album(root/'陈奕迅'/'阴天快乐'/'01 Song.flac')
    mixed=root/'Mixed';album(mixed/'a.flac',album_name='One');album(mixed/'b.flac',title='Other',album_name='Two')
    result,rows=run(service)
    assert kept.is_file() and (mixed/'a.flac').is_file() and (mixed/'b.flac').is_file()
    reasons=[r.get('detail','') for r in rows]
    assert any('其他专辑的音频' in d for d in reasons),reasons


def test_existing_target_and_disabled_setting_keep_the_folder(service):
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library'])
    source=root/'陈奕迅 - 阴天快乐';album(source/'01 Song.flac')
    (root/'陈奕迅'/'阴天快乐').mkdir(parents=True);(root/'陈奕迅'/'阴天快乐'/'note.txt').write_text('x','utf-8')
    result,rows=run(service)
    assert (source/'01 Song.flac').is_file() and '目标文件夹已存在' in rows[0]['detail']
    (root/'陈奕迅'/'阴天快乐'/'note.txt').unlink();(root/'陈奕迅'/'阴天快乐').rmdir()
    current=service.config.current();values=copy.deepcopy(current['values']);values['workflow']['artist_folders']=False
    service.config.save(values,current['version'])
    result,rows=run(service,service.config.current()['values'])
    assert (source/'01 Song.flac').is_file()


def test_already_complete_album_still_moves(service,monkeypatch):
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library'])
    source=root/'陈奕迅 - 阴天快乐';song=album(source/'01 Song.flac')
    tags=FLAC(song);tags['discnumber']=['1/1'];tags['lyrics']=['[00:00.00]x\n[00:01.00]y'];tags.save()
    song.with_suffix('.lrc').write_text('[00:00.00]x\n[00:01.00]y\n','utf-8',newline='\n')
    result,rows=run(service)
    assert rows[0]['stage']=='completed' and rows[0]['message']=='资料已齐全 · 已整理到音乐人文件夹',rows[0]
    assert (root/'陈奕迅'/'阴天快乐'/'01 Song.flac').is_file() and (root/'陈奕迅'/'阴天快乐'/'01 Song.lrc').is_file()
