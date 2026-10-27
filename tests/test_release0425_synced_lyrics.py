import shutil,subprocess,wave
from mutagen.flac import FLAC
from mutagen.id3 import ID3
from musicops.media.lyrics import player_lrc,write_sidecar,embed_sidecar,embedded_synchronized,player_ready_sidecar
from musicops.media.resources import validate_lrc
from musicops.media.tags import read_tag_fields


def audio(path,codec):
    wav=path.with_suffix('.wav')
    with wave.open(str(wav),'wb') as out:out.setparams((1,2,44100,0,'NONE','not compressed'));out.writeframes(b'\0'*88200)
    result=subprocess.run([shutil.which('ffmpeg'),'-v','error','-i',str(wav),'-c:a',codec,'-n',str(path)],capture_output=True,creationflags=getattr(subprocess,'CREATE_NO_WINDOW',0))
    assert result.returncode==0,result.stderr;wav.unlink();return path


def test_player_dialect_folds_offset_expands_repeats_and_drops_word_stamps():
    source=('﻿{"t":0,"c":[{"tx":"作词: "},{"tx":"A"}]}\r\n[ti:Song]\r\n[offset:+500]\r\n[by:x]\r\n'
            '[00:10.5]<00:10.50>Hel<00:11.00>lo\r\n[00:20:25][01:02.345]Chorus\r\n[00:20.25]副歌翻译\r\n[00:00.00]\r\n')
    assert player_lrc(source)==('[ti:Song]\n[by:x]\n[00:11.00]Hello\n[00:20.75]Chorus\n[00:20.75]副歌翻译\n[01:02.85]Chorus')
    assert validate_lrc(player_lrc(source),63)['valid'] and not validate_lrc(source)['valid']
    # A negative offset never produces a negative timestamp.
    assert player_lrc('[offset:-800]\n[00:00.50]a\n[00:02.00]b')=='[00:00.00]a\n[00:01.20]b'


def test_pseudo_synced_lyrics_that_never_advance_are_rejected():
    flat='\n'.join(f'[00:00.00]line {i}' for i in range(8))
    assert 'UNTIMED_LYRICS' in validate_lrc(flat)['errors']
    assert validate_lrc('[00:00.00]纯音乐，请欣赏')['valid']
    assert validate_lrc('\n'.join(f'[00:{i:02d}.00]line {i}' for i in range(8)))['valid']


def test_sidecar_is_utf8_without_bom_and_lf_for_plex(tmp_path):
    path=write_sidecar(tmp_path/'Song.lrc','﻿[00:01.000]a\r\n[00:02.000]b\r\n',3)
    data=(tmp_path/'Song.lrc').read_bytes()
    assert data==b'[00:01.00]a\n[00:02.00]b\n' and player_ready_sidecar(path,3)
    (tmp_path/'Old.lrc').write_text('[00:01.000]a\n[00:02.000]b','utf-8')
    assert not player_ready_sidecar(tmp_path/'Old.lrc',3)


def test_flac_embed_upgrades_old_plain_copy_but_keeps_different_user_lyrics(tmp_path):
    song=audio(tmp_path/'song.flac','flac');sidecar=tmp_path/'song.lrc'
    write_sidecar(sidecar,'[00:00.50]第一行\n[00:01.50]Second',2)
    tags=FLAC(song);tags['lyrics']=['第一行\nSecond'];tags.save()
    assert not embedded_synchronized(song,2)
    embed_sidecar(song,sidecar,2)
    assert FLAC(song)['lyrics']==['[00:00.50]第一行\n[00:01.50]Second'] and embedded_synchronized(song,2)
    locked=audio(tmp_path/'locked.flac','flac');tags=FLAC(locked);tags['lyrics']=['第一行\nSecond'];tags.save()
    embed_sidecar(locked,sidecar,2,upgrade=False)
    assert FLAC(locked)['lyrics']==['第一行\nSecond']
    other=audio(tmp_path/'other.flac','flac');tags=FLAC(other);tags['lyrics']=['User written words'];tags.save()
    embed_sidecar(other,sidecar,2)
    assert FLAC(other)['lyrics']==['User written words']


def test_mp3_gets_uslt_and_roon_sylt_with_millisecond_timeline(tmp_path):
    song=audio(tmp_path/'song.mp3','libmp3lame');sidecar=tmp_path/'song.lrc'
    write_sidecar(sidecar,'[00:00.50]one\n[00:01.25]two',2)
    embed_sidecar(song,sidecar,2)
    tags=ID3(song)
    assert [f.text for f in tags.getall('USLT')]==['[00:00.50]one\n[00:01.25]two']
    sylt=tags.getall('SYLT')
    assert len(sylt)==1 and sylt[0].format==2 and sylt[0].type==1 and sylt[0].text==[('one',500),('two',1250)]
    assert embedded_synchronized(song,2)
    # Rewriting lyrics replaces the old timeline instead of stacking frames.
    from musicops.media.tags import apply_tag_changes
    apply_tag_changes(song,{'lyrics':'plain words'})
    assert ID3(song).getall('SYLT')==[] and read_tag_fields(song)['lyrics']==['plain words']


def test_m4a_embeds_timed_lyrics(tmp_path):
    song=audio(tmp_path/'song.m4a','aac');sidecar=tmp_path/'song.lrc'
    write_sidecar(sidecar,'[00:00.50]one\n[00:01.25]two',2)
    embed_sidecar(song,sidecar,2)
    assert read_tag_fields(song)['lyrics']==['[00:00.50]one\n[00:01.25]two']
