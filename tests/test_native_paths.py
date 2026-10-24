"""Actual POSIX publication/rollback with long paths and UTF-8 file names."""
import hashlib
import os
from pathlib import Path
import pytest
from PIL import Image,ImageDraw
from musicops.domain.errors import PathBlocked
from musicops.library.paths import safe_destination
from musicops.library.transactions import TransactionEngine
from musicops.state.store import StateStore

pytestmark=pytest.mark.skipif(os.name=='nt',reason='Native POSIX filesystem contract; Windows guards covered separately')


def test_existing_posix_paths_beyond_windows_max_path_are_valid(tmp_path):
    relative=Path('a'*85)/('b'*85)/'song.flac'
    target=safe_destination(tmp_path,relative)
    assert len(str(target))>240
    target.parent.mkdir(parents=True);target.write_bytes(b'unchanged fixture')
    assert safe_destination(tmp_path,relative).read_bytes()==b'unchanged fixture'


def test_native_component_bounds_count_utf8_bytes_before_filesystem_access(tmp_path):
    with pytest.raises(PathBlocked,match='单个名称'):
        safe_destination(tmp_path,'曲'*86+'.flac')
    accepted=safe_destination(tmp_path,'x'*240+'.flac')
    accepted.write_bytes(b'valid native name')
    assert accepted.is_file()


def test_existing_native_punctuation_and_reserved_words_are_preserved(tmp_path):
    for name in ('CON.flac','album/Title: question?.flac','album /song.flac'):
        target=safe_destination(tmp_path,name)
        target.parent.mkdir(parents=True,exist_ok=True);target.write_bytes(b'valid POSIX name')
        assert target.read_bytes()==b'valid POSIX name'


def test_case_alias_guard_follows_actual_filesystem(tmp_path):
    (tmp_path/'Album').mkdir()
    if (tmp_path/'album').exists():
        with pytest.raises(PathBlocked):safe_destination(tmp_path,'album/song.flac')
    else:assert safe_destination(tmp_path,'album/song.flac')==tmp_path/'album/song.flac'


@pytest.mark.parametrize('name',['../outside.flac','a/./track.flac','a\\..\\outside.flac','C:\\outside.flac'])
def test_native_long_path_support_does_not_relax_root_containment(tmp_path,name):
    with pytest.raises(PathBlocked):safe_destination(tmp_path,name)


def test_native_symlink_escape_and_explicit_portable_limit_stay_blocked(tmp_path):
    outside=tmp_path/'outside';outside.mkdir();root=tmp_path/'library';root.mkdir()
    (root/'link').symlink_to(outside,target_is_directory=True)
    with pytest.raises(PathBlocked):safe_destination(root,'link/song.flac')
    with pytest.raises(PathBlocked):safe_destination(root,Path('a'*85)/('b'*85)/'song.flac',max_length=240)


def test_long_utf8_destination_can_publish_and_restore_without_long_temp_names(tmp_path):
    paths={key:str(tmp_path/key) for key in ('library','source','work','backup','quarantine')}
    for value in paths.values():Path(value).mkdir()
    relative=Path('artist-'+'a'*75)/('album-'+'b'*75)/('封面'*35+'.jpg')
    target=Path(paths['library'])/relative;target.parent.mkdir(parents=True)
    artwork=Image.new('RGB',(400,400),'red');ImageDraw.Draw(artwork).rectangle((20,30,200,350),fill='gold');artwork.save(target)
    before=target.read_bytes();source=Path(paths['source'])/'cover.jpg'
    artwork=Image.new('RGB',(400,400),'blue');ImageDraw.Draw(artwork).rectangle((20,30,200,350),fill='gold');artwork.save(source);new=source.read_bytes()
    engine=TransactionEngine(StateStore(tmp_path/'state.sqlite'),paths)
    result=engine.execute({'schema_version':1,'plan_id':'native-long-replace','config_hash':'synthetic',
        'operations':[{'operation_id':'cover','kind':'replace','source':str(source),'destination':str(relative),'before_hash':hashlib.sha256(before).hexdigest()}]})
    assert result['state']=='COMMITTED' and target.read_bytes()==new
    operation=result['operations'][0]
    assert len(Path(operation['publish_temporary']).name)<80 and len(Path(operation['restore_temporary']).name)<80
    assert engine.rollback(result['tx_id'])['state']=='ROLLED_BACK'
    assert target.read_bytes()==before and source.read_bytes()==new
    assert not list(target.parent.glob('.musicops-*'))


def test_new_utf8_names_are_compacted_by_bytes_and_keep_distinct_hashes():
    from musicops.app.pipeline import compact_segment
    first=compact_segment('歌'*120,100);second=compact_segment('歌'*119+'曲',100)
    assert len(os.fsencode(first))<=240 and first!=second
