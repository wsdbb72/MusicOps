"""Audit regressions use synthetic files and temporary SQLite profiles only."""
import json
from pathlib import Path
from types import SimpleNamespace
import uuid

import pytest

from musicops.clients.metainfo import bencode, parse_metainfo
from musicops.domain.errors import Conflict, PathBlocked, PolicyBlocked, ValidationFailed
from musicops.domain.identity import identities_same, match_track
from musicops.domain.quality import compare_quality, propose_duplicates
from musicops.library.index import index_library, sha256_file
from musicops.library.transactions import TransactionEngine
from musicops.media.validation import MediaValidator, prepare_media_copy
from musicops.media.ncm import decrypt_ncm
from musicops.state.store import StateStore, utcnow


def recording(**updates):
    value = dict(raw_title='Audit Song', artists=['Audit Artist'], album='Audit Album',
                 duration=10, edition='', track=1, disc=1, file_id='audit-file',
                 validation={'decode_state':'passed','roon_compatible':True},
                 media_info={'codec_name':'flac','channels':2}, sha256='same-audio')
    value.update(updates)
    return value


def fake_runner(command, timeout):
    if '-version' in command:
        body=b'Audit media tool v1\n'
    elif '-show_format' in command:
        body=json.dumps({'format':{'duration':'10'},'streams':[{'codec_type':'audio','codec_name':'flac'}]}).encode()
    else:
        body=b''
    return SimpleNamespace(returncode=0,stdout=body,stderr=b'')


def test_V06_audit_track_totals_and_compatible_coverage():
    target=recording(target_id='audit-target')
    assert match_track(target,[recording(track='01/12',disc='01/02')])['main_state']=='verified_present'
    incompatible=recording(validation={'decode_state':'passed','roon_compatible':False})
    assert match_track(target,[incompatible])['main_state']=='ambiguous'


@pytest.mark.parametrize('conflict',[{'album':'Other Album'},{'edition':'Remaster'},
                                   {'catalogue':'CAT-B'},{'source_album_id':'222'}])
def test_V20_audit_shared_release_id_does_not_erase_conflicts(conflict):
    first=recording(release_id='shared-id',catalogue='CAT-A',source_album_id='111')
    second=recording(file_id='other-file',release_id='shared-id',catalogue='CAT-A',source_album_id='111')
    second.update(conflict)
    assert not identities_same(first,second)
    assert propose_duplicates([first,second])[0]['action']=='review'
    assert match_track(first,[second])['main_state']=='version_mismatch'


def test_T16_audit_large_unique_catalogue_only_compares_equal_hashes(monkeypatch):
    import musicops.domain.quality as module
    files=[recording(file_id=str(i),sha256=f'unique-{i}') for i in range(27102)]
    calls=[]
    original=module.identities_same
    monkeypatch.setattr(module,'identities_same',lambda *a,**k: calls.append(a) or original(*a,**k))
    assert propose_duplicates(files)==[]
    assert calls==[]
    files[100]['pcm_hash']=files[200]['pcm_hash']='shared-pcm'
    files[200]['sha256']=files[100]['sha256']
    proposals=propose_duplicates(files)
    assert len(proposals)==1 and proposals[0]['file_ids']==['100','200']


def test_T16_audit_mass_duplicate_group_blocks_without_partial_proposals(monkeypatch):
    import musicops.domain.quality as module
    monkeypatch.setattr(module,'identities_same',lambda *a,**k:pytest.fail('oversized group must be bounded before planning'))
    with pytest.raises(PolicyBlocked,match='50000'):
        propose_duplicates([recording(file_id=str(i)) for i in range(500)])


def test_V20_audit_unknown_bit_depth_is_reviewable():
    first=recording(media_info={'codec_name':'flac','channels':2,'bits_per_raw_sample':'N/A'})
    second=recording(file_id='other-file')
    assert compare_quality(first,second,{'prefer_24bit':True})['action']=='prefer'


@pytest.mark.parametrize('conflict',[{'track':2},{'disc':2},{'albumartist':['Different Release Artist']}])
def test_T16_audit_repeated_recording_positions_are_preserved(conflict):
    first=recording(albumartist=['Album Artist'])
    second=recording(file_id='other-file',albumartist=['Album Artist'])
    second.update(conflict)
    assert not identities_same(first,second)
    assert propose_duplicates([first,second])[0]['action']=='review'


@pytest.mark.parametrize('duration',['nan','inf','-inf','0','-10'])
def test_V20_audit_nonfinite_or_nonpositive_duration_never_passes(tmp_path,monkeypatch,duration):
    import musicops.media.validation as module
    source=tmp_path/'audit.flac'; source.write_bytes(b'synthetic audio')
    def run(command,timeout):
        value=fake_runner(command,timeout)
        if '-show_format' in command:
            value.stdout=json.dumps({'format':{'duration':duration},'streams':[{'codec_type':'audio','codec_name':'flac'}]}).encode()
        return value
    monkeypatch.setattr(module,'_run',run)
    result=MediaValidator().validate(source)
    assert result['decode_state']=='corrupt' and result['error_class']=='DURATION_INVALID'
    assert not result['roon_compatible']


def test_V21_audit_cached_evidence_cannot_be_mutated_by_caller(tmp_path,monkeypatch):
    import musicops.media.validation as module
    monkeypatch.setattr(module,'_run',fake_runner)
    source=tmp_path/'audit.flac'; source.write_bytes(b'synthetic audio')
    validator=MediaValidator()
    first=validator.validate(source)
    first['media_info']['codec_name']='altered'
    cached=validator.validate(source)
    assert cached['cache_hit'] and cached['media_info']['codec_name']=='flac'
    cached['streams'][0]['codec_type']='altered'
    assert validator.validate(source)['streams'][0]['codec_type']=='audio'


@pytest.mark.parametrize('probe',[[],{'streams':None},{'streams':[None]}, {'streams':[],'format':[]}])
def test_V20_audit_malformed_probe_output_is_structured_tool_error(tmp_path,monkeypatch,probe):
    import musicops.media.validation as module
    source=tmp_path/'audit.flac'; source.write_bytes(b'synthetic audio')
    def run(command,timeout):
        value=fake_runner(command,timeout)
        if '-show_format' in command: value.stdout=json.dumps(probe).encode()
        return value
    monkeypatch.setattr(module,'_run',run)
    assert MediaValidator().validate(source)['decode_state']=='tool_error'


def test_V21_audit_cache_hit_rechecks_current_bytes(tmp_path,monkeypatch):
    import musicops.media.validation as module
    monkeypatch.setattr(module,'_run',fake_runner)
    source=tmp_path/'audit.flac'; source.write_bytes(b'synthetic audio')
    validator=MediaValidator(); validator.validate(source)
    original=validator.tool_versions
    def changed_versions():
        source.write_bytes(b'external revision')
        return original()
    monkeypatch.setattr(validator,'tool_versions',changed_versions)
    result=validator.validate(source)
    assert result['decode_state']=='changed_during_check' and not result['roon_compatible']


class CopyValidator:
    ffmpeg='audit-tool'; timeout=2
    def validate(self,path):
        return {'decode_state':'passed','roon_compatible':True,'media_info':{'codec_name':'flac'},
                'file_hash':sha256_file(path),'path':str(path)}


def test_V22_audit_copy_racing_target_is_preserved(tmp_path):
    source=tmp_path/'source.flac'; source.write_bytes(b'synthetic source')
    destination=tmp_path/'destination.flac'
    class RacingValidator(CopyValidator):
        def validate(self,path):
            if Path(path)!=source:
                destination.write_bytes(b'external target')
            return super().validate(path)
    with pytest.raises(Conflict):
        prepare_media_copy(source,destination,RacingValidator())
    assert destination.read_bytes()==b'external target'
    assert not list(tmp_path.glob('.media-*'))
    assert source.read_bytes()==b'synthetic source'


def test_V22_audit_failed_conversion_leaves_no_output(tmp_path,monkeypatch):
    import musicops.media.validation as module
    source=tmp_path/'source.flac'; source.write_bytes(b'synthetic source')
    destination=tmp_path/'failed.flac'
    def fail_conversion(command,timeout):
        Path(command[-1]).write_bytes(b'partial conversion')
        return SimpleNamespace(returncode=1,stderr=b'audit conversion failed',stdout=b'')
    monkeypatch.setattr(module,'_run',fail_conversion)
    with pytest.raises(ValidationFailed):
        prepare_media_copy(source,destination,CopyValidator(),convert_to='flac')
    assert not destination.exists() and not list(tmp_path.glob('.media-*'))


@pytest.mark.parametrize('source_removed',[True,False])
def test_V35_audit_rollback_uses_backup_after_source_cleanup(tmp_path,source_removed):
    paths={key:str(tmp_path/key) for key in ('library','source','work','backup','quarantine')}
    for path in paths.values(): Path(path).mkdir()
    source=Path(paths['source'])/'song.lrc'; source.write_text('[00:00.00]New lyric',encoding='utf-8')
    target=Path(paths['library'])/'song.lrc'; original='[00:00.00]Old lyric'
    target.write_text(original,encoding='utf-8')
    store=StateStore(tmp_path/'state.sqlite')
    engine=TransactionEngine(store,paths)
    plan={'plan_id':str(uuid.uuid4()),'config_hash':'audit','operations':[{
        'operation_id':str(uuid.uuid4()),'source':str(source),'destination':'song.lrc',
        'before_hash':sha256_file(target),'kind':'replace'}]}
    transaction=engine.execute(plan)
    if source_removed: source.unlink()
    else: source.write_text('External new source',encoding='utf-8')
    assert engine.execute(plan)['idempotent'] is True
    assert engine.rollback(transaction['tx_id'])['state']=='ROLLED_BACK'
    assert target.read_text(encoding='utf-8')==original
    if not source_removed: assert source.read_text(encoding='utf-8')=='External new source'


@pytest.mark.parametrize('invalid',[(True,1,1),(1,float('inf'),1),(1,1,float('nan')),(1,1,'1')])
def test_V11_audit_unknown_or_malformed_token_budget_rejected(tmp_path,invalid):
    store=StateStore(tmp_path/'state.sqlite')
    with pytest.raises(RuntimeError,match='POLICY_BLOCKED'):
        store.reserve_token('audit','resource',*invalid,utcnow())
    assert store.list('token_reservations')==[]


@pytest.mark.parametrize('observed',['2026-01-01T12:00:00','invalid',float('nan')])
def test_V11_audit_invalid_balance_timestamp_is_blocked(tmp_path,observed):
    with pytest.raises(RuntimeError,match='POLICY_BLOCKED'):
        StateStore(tmp_path/'state.sqlite').reserve_token('audit','resource',1,1,1,observed)


def test_V12_audit_consumed_token_cannot_refund_or_restart(tmp_path):
    store=StateStore(tmp_path/'state.sqlite')
    reservation=store.reserve_token('audit','resource',1,1,1,utcnow())
    store.token_result(reservation['id'],'CONFIRMED',{'audit':'acknowledged consumption'})
    for state in ('RELEASED','REJECTED','RESERVED','REQUESTING'):
        with pytest.raises(ValueError,match='INVALID_TOKEN_TRANSITION'):
            store.token_result(reservation['id'],state,{'audit':'attempted reset'})
    assert store.get('token_reservations',reservation['id'])['state']=='CONFIRMED'
    with pytest.raises(RuntimeError):
        store.reserve_token('audit','new-resource',1,1,1,utcnow())


def test_V12_audit_unknown_token_release_requires_reconciliation(tmp_path):
    store=StateStore(tmp_path/'state.sqlite')
    reservation=store.reserve_token('audit','resource',1,1,1,utcnow())
    store.token_result(reservation['id'],'REQUESTING')
    store.token_result(reservation['id'],'UNKNOWN')
    with pytest.raises(ValueError,match='TOKEN_RECONCILIATION_REQUIRED'):
        store.token_result(reservation['id'],'RELEASED')
    assert store.get('token_reservations',reservation['id'])['state']=='UNKNOWN'
    store.token_result(reservation['id'],'RELEASED',{'kind':'audit reconciliation','state':'NOT_CONSUMED'})
    assert store.reserve_token('audit','new-resource',1,1,1,utcnow())['state']=='RESERVED'


def test_V10_audit_torrent_file_directory_collisions_rejected():
    info={b'name':b'audit',b'piece length':16384,b'pieces':b'x'*20,b'files':[
        {b'path':[b'Album'],b'length':1},{b'path':[b'Album',b'Track.flac'],b'length':1}]}
    with pytest.raises(ValueError,match='file/directory'):
        parse_metainfo(bencode({b'info':info}))


@pytest.mark.parametrize('attribute',[1,[b'p'],{b'x':b'p'}])
def test_V10_audit_torrent_malformed_attributes_have_parse_error(attribute):
    info={b'name':b'audit',b'piece length':16384,b'pieces':b'x'*20,b'length':1,b'attr':attribute}
    with pytest.raises(ValueError,match='attributes'):
        parse_metainfo(bencode({b'info':info}))


def test_V09_audit_index_retry_does_not_retain_busy_state(tmp_path,monkeypatch):
    import musicops.library.index as module
    source=tmp_path/'audit.wav'; source.write_bytes(b'synthetic wav')
    original=module.file_version; calls=[]
    def changed_version(path):
        calls.append(path)
        value=original(path)
        return (value[0],value[1]+1,value[2]) if len(calls)==2 else value
    monkeypatch.setattr(module,'file_version',changed_version)
    # Valid tag reader fixture prevents a synthetic container failure from
    # obscuring the retry behavior under test.
    import mutagen
    monkeypatch.setattr(mutagen,'File',lambda *a,**k:SimpleNamespace(tags={'title':['audit'],'artist':['artist']},info=SimpleNamespace(length=10)))
    row=index_library(tmp_path)[0]
    assert len(calls)==4 and row['read_error'] is None


def test_V19_audit_ncm_read_failure_is_a_structured_failure(tmp_path,monkeypatch):
    import musicops.media.ncm as module
    source=tmp_path/'audit.ncm'; source.write_bytes(b'synthetic container')
    def unreadable(path): raise PermissionError('audit read denied')
    monkeypatch.setattr(module,'_hash_file',unreadable)
    result=decrypt_ncm(source,tmp_path/'work.flac')
    assert result['status']=='DECRYPT_FAILED' and not (tmp_path/'work.flac').exists()


def test_V19_audit_ncm_reparse_path_rejected_before_read(tmp_path,monkeypatch):
    import musicops.media.ncm as module
    def reparse(path): raise PathBlocked('audit reparse fixture')
    monkeypatch.setattr(module,'reject_reparse_chain',reparse)
    result=decrypt_ncm(tmp_path/'audit.ncm',tmp_path/'work.flac')
    assert result['error_class']=='PATH_BLOCKED' and not (tmp_path/'work.flac').exists()
