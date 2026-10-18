import copy,hashlib,io,json,os,shutil,subprocess,time,wave
from pathlib import Path
import pytest
from mutagen.flac import FLAC,Picture
from PIL import Image,ImageDraw
from musicops.app.service import AppService
from musicops.app.pipeline import Workflows,quality,improves,no_downgrade
from musicops.clients.http import FakeTransport,Response,PolicyBlocked
from musicops.clients.metainfo import bencode,parse_metainfo
from musicops.config.schema import default_config,ConfigManager,digest
from musicops.providers.pt_download import PTDownloadProvider
from musicops.providers.pt_readonly import HOSTS
from musicops.state.store import StateStore

@pytest.fixture
def service(tmp_path,monkeypatch):
    s=AppService(tmp_path/'profile',offline=False);s.stop.set();s.thread.join(3)
    current=s.config.current();cfg=copy.deepcopy(current['values']);cfg['paths']['library']=str(tmp_path/'music');Path(cfg['paths']['library']).mkdir()
    cfg['paths']['tools']={k:shutil.which(k) for k in ('ffmpeg','ffprobe')};assert all(cfg['paths']['tools'].values())
    cfg['catalog']['enabled']=False;cfg['catalog']['musicbrainz']['enabled']=False;cfg['catalog']['discogs']['enabled']=False;cfg['providers']['netease']['enabled']=False
    s.config.save(cfg,current['version']);monkeypatch.setattr(s,'_real_resources',lambda *args,**kwargs:{'candidates':[]})
    yield s

def image():
    img=Image.new('RGB',(400,400),'navy');ImageDraw.Draw(img).rectangle((20,20,200,350),fill='gold');out=io.BytesIO();img.save(out,'JPEG');return out.getvalue()
def flac(path,title='Song',bits=16,disc='1/1',release=None):
    path.parent.mkdir(parents=True,exist_ok=True);wav=path.with_suffix('.wav')
    with wave.open(str(wav),'wb') as out:out.setparams((1,bits//8,44100,0,'NONE','not compressed'));out.writeframes(b'\0'*(44100*2*bits//8))
    value=subprocess.run([shutil.which('ffmpeg'),'-v','error','-i',str(wav),'-c:a','flac','-n',str(path)],capture_output=True,creationflags=getattr(subprocess,'CREATE_NO_WINDOW',0));assert value.returncode==0;wav.unlink()
    tags=FLAC(path);tags.update(title=[title],artist=['Artist'],album=['Album'],albumartist=['Artist'],tracknumber=['1'],discnumber=[disc],totaldiscs=['1'])
    if release:tags['musicbrainz_albumid']=[release]
    pic=Picture();pic.type=3;pic.mime='image/jpeg';pic.data=image();tags.add_picture(pic);tags.save();return path

def job(service,scene='playlist',sources=None,old=None,targets=None):
    cfg=service.config.current()['values'];row={'kind':'workflow','job_id':'test_job','batch_id':'batch','scene':scene,'config':cfg,'old_files':old or [],'targets':targets or [],'stage':'metadata','label':'Album','progress':0,'message':'','prefer_24bit':True}
    if sources:row['source_path']=str(sources)
    service.workflows.put(row);return row
def until_terminal(service,limit=8):
    for _ in range(limit):
        service.workflows.advance();row=service.workflows.rows()[0]
        if row['stage'] in ('completed','needs_review','failed','skipped'):return row
    pytest.fail('workflow did not finish')

def test_multi_disc_repair_publishes_consistent_totals_and_verified_backups(service):
    cfg=service.config.current()['values'];library=Path(cfg['paths']['library']);one=flac(library/'Artist/Album/CD1 - One/one.flac',disc='1/1');two=flac(library/'Artist/Album/CD2 - Two/two.flac',title='Two',disc='2/1')
    original={str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in (one,two)}
    for p in (one,two):p.with_suffix('.lrc').write_text('[00:00.00]fixture\n[00:01.00]test',encoding='utf-8')
    result=service.workflows.start({'scene':'complete'},cfg);assert result['album_count']==1
    finished=until_terminal(service);assert finished['stage']=='completed',finished
    assert FLAC(one)['discnumber']==['1/2'] and FLAC(two)['discnumber']==['2/2']
    assert FLAC(one)['totaldiscs']==['2'] and FLAC(two)['totaldiscs']==['2']
    tx=service.store.get('transactions',finished['transaction_id']);assert tx['state']=='COMMITTED'
    assert all(hashlib.sha256(Path(b['backup']).read_bytes()).hexdigest()==original[b['path']] for b in tx['backup_manifest'] if b['path'] in original)
    assert service.workflows.start({'scene':'complete'},cfg)['album_count']==0

def test_complete_album_is_validated_and_atomically_published_outside_watch_root(service):
    cfg=service.config.current()['values'];source=Path(cfg['paths']['source'])/'test';audio=flac(source/'song.flac');before=audio.read_bytes()
    job(service,sources=source,targets=[{'raw_title':'Song','artists':['Artist'],'album':'Album','duration':2,'source_song_id':'42'}]);finished=until_terminal(service)
    assert finished['stage']=='completed',finished
    output=Path(cfg['paths']['library'])/'Artist/Album/01 Song.flac';assert output.is_file() and audio.read_bytes()==before
    tx=service.store.get('transactions',finished['transaction_id']);group=tx['directory_publication'];assert group and group['renamed'] and not Path(group['staging']).is_relative_to(Path(cfg['paths']['library']))

def test_wrong_recording_never_enters_library(service):
    cfg=service.config.current()['values'];source=Path(cfg['paths']['source'])/'wrong';flac(source/'song.flac',title='Wrong Song')
    job(service,sources=source,targets=[{'raw_title':'Song','artists':['Artist'],'album':'Album','duration':2}]);finished=until_terminal(service)
    assert finished['stage']=='needs_review' and not list(Path(cfg['paths']['library']).rglob('*.flac'))

def test_corrupt_audio_never_retires_old_version(service):
    cfg=service.config.current()['values'];old=flac(Path(cfg['paths']['library'])/'Artist/Album/old.flac');before=old.read_bytes();source=Path(cfg['paths']['source'])/'new';bad=flac(source/'new.flac',bits=24)
    from musicops.library.index import index_library
    originals=index_library(cfg['paths']['library'],metadata_only=True);originals[0]['original_hash']=hashlib.sha256(before).hexdigest();row=job(service,scene='upgrade',sources=source,old=originals)
    service.workflows.advance();ready=service.workflows.rows()[0];assert ready['stage']=='validating';Path(ready['prepared'][0]['path']).write_bytes(b'corrupt')
    finished=until_terminal(service);assert finished['stage']=='needs_review' and old.read_bytes()==before and not list(Path(cfg['paths']['quarantine']).rglob('*.flac'))

def test_verified_quality_upgrade_retires_library_copy_but_preserves_seed_source(service):
    cfg=service.config.current()['values'];mbid='12345678-1234-1234-1234-123456789abc';old=flac(Path(cfg['paths']['library'])/'Artist/Album/old.flac',release=mbid);source=Path(cfg['paths']['source'])/'new';new=flac(source/'new.flac',bits=24,release=mbid);seed=new.read_bytes()
    from musicops.library.index import index_library
    originals=index_library(cfg['paths']['library'],metadata_only=True);originals[0]['original_hash']=hashlib.sha256(old.read_bytes()).hexdigest();job(service,scene='upgrade',sources=source,old=originals)
    finished=until_terminal(service);assert finished['stage']=='completed',finished
    assert not old.exists() and new.read_bytes()==seed
    outputs=list(Path(cfg['paths']['library']).rglob('*.flac'));assert len(outputs)==1 and FLAC(outputs[0]).info.bits_per_sample==24
    assert list(Path(cfg['paths']['quarantine']).rglob('*.flac'))
    assert service.workflows.restore(finished['job_id'])['restored']
    assert old.is_file() and FLAC(old).info.bits_per_sample==16 and new.read_bytes()==seed
    assert list(Path(cfg['paths']['library']).rglob('*.flac'))==[old]

def test_higher_compressed_lossless_bitrate_is_not_upgrade():
    old={'codec':'flac','bits_per_sample':16,'sample_rate':44100,'bitrate':400000,'channels':2};new={**old,'bitrate':900000}
    assert not improves(old,new) and no_downgrade(old,new)
    assert improves(old,{**new,'bits_per_sample':24})
    assert not improves(old,{**new,'bits_per_sample':24,'channels':6})
    assert not improves({**old,'bits_per_sample':24},{**new,'bits_per_sample':16})

def test_failed_site_search_is_not_no_resource_or_netease_fallback(service,monkeypatch):
    cfg=service.config.current()['values'];cfg['providers']['dicmusic']['enabled']=True
    row=job(service,targets=[{'raw_title':'Song','album':'Album','artists':['Artist']}]);row['config']=cfg;service.workflows.put(row,stage='searching')
    class Provider:
        def search(self,query):return {'status':'AUTH_FAILED','candidates':[]}
    monkeypatch.setattr(service.workflows,'provider',lambda *a:Provider());finished=until_terminal(service)
    assert finished['stage']=='needs_review' and finished['review_kind']=='search'

def test_redacted_shares_cookie_and_endpoint_contract_without_live_network(tmp_path):
    transport=FakeTransport([Response(200,json.dumps({'status':'success','response':{'group':{'id':2},'torrent':{'id':3,'freeTorrent':'Free'}}}).encode())])
    p=PTDownloadProvider('redacted',{'base_url':'https://redacted.sh','credential_ref':'vault://test'},transport,lambda *a:json.dumps([{'domain':'redacted.sh','path':'/','name':'session','value':'virtual'}]))
    assert p.detail({'resource_id':'3'})['torrent']['freeTorrent']=='Free'
    assert transport.requests[0]['url'].startswith('https://redacted.sh/') and transport.requests[0]['headers']['Cookie']=='session=virtual'

def test_fl_timeout_is_not_retried_or_double_spent(tmp_path,monkeypatch):
    content=bencode({b'info':{b'name':b'song.flac',b'piece length':16384,b'length':100,b'pieces':b'a'*20}})
    page='<a href="torrents.php?action=download&amp;id=3&amp;usetoken=1">FL</a><span>令牌 : 4</span>'
    responses=[Response(200,page.encode()),Response(200,json.dumps({'status':'success','response':{'id':1,'userstats':{}}}).encode()),Response(200,json.dumps({'status':'success','response':{'personal':{}}}).encode()),TimeoutError('fixture timeout')]
    transport=FakeTransport(responses);p=PTDownloadProvider('dicmusic',{'base_url':'https://dicmusic.com','credential_ref':'vault://test'},transport,lambda *a:json.dumps([{'domain':'dicmusic.com','path':'/','name':'session','value':'virtual'}]));monkeypatch.setattr(p,'detail',lambda _: {'group':{'id':2},'torrent':{'freeTorrent':False}})
    store=StateStore(tmp_path/'state.sqlite');candidate={'candidate_id':'dicmusic:3','resource_id':'3','group_id':'2'}
    with pytest.raises(TimeoutError):p.fetch(candidate,store,'batch',3,tmp_path/'private.torrent')
    assert store.get('evidence','pt-request:dicmusic:3')['state']=='unknown'
    requests=len(transport.requests)
    # A retry may read the exact row to reconcile personal free status, but
    # must never repeat the token-bearing download when it is not confirmed.
    transport.responses.append(Response(200,page.encode()))
    with pytest.raises(PolicyBlocked):p.fetch(candidate,store,'batch',3,tmp_path/'private.torrent')
    assert len(transport.requests)==requests+1
    assert transport.requests[-1]['params']=={'id':'2','torrentid':'3'}
    with store.connection() as c:assert c.execute("SELECT state,COUNT(*) FROM token_reservations GROUP BY state").fetchone()[:]==('REQUEST_UNKNOWN',1)

def test_provider_addition_migrates_existing_credentials_without_keyerror(tmp_path):
    store=StateStore(tmp_path/'state.sqlite');old=default_config(tmp_path,offline=False);old['providers'].pop('redacted');old.pop('workflow');old['providers']['dicmusic']['credential_ref']='vault://saved'
    store.put('config_versions','1',{'version':1,'values':old});cfg=ConfigManager(store,tmp_path).current()['values']
    assert not cfg['providers']['redacted']['enabled'] and cfg['providers']['dicmusic']['credential_ref']=='vault://saved' and not cfg['workflow']['setup_done']

def test_lyrics_auth_failure_requires_action_not_no_lyrics(service,monkeypatch):
    cfg=service.config.current()['values'];cfg['lyrics']['primary']['base_url']='https://configured.example.invalid';cfg['lyrics']['primary']['base_url']='https://lyrics.virtual.invalid'
    source=Path(cfg['paths']['source'])/'lyrics';flac(source/'song.flac');row=job(service,sources=source);row['config']=cfg;service.workflows.put(row)
    monkeypatch.setattr(service,'_real_resources',lambda *a,**k:{'lyrics':{'state':'AUTH_FAILED'},'candidates':[]})
    finished=until_terminal(service);assert finished['stage']=='needs_review' and finished['review_kind']=='resources'
    assert not list(Path(cfg['paths']['library']).rglob('*.flac'))

def test_ncm_client_import_and_decryption_preserve_original(service,tmp_path):
    from test_providers import make_ncm
    from musicops.providers.netease_download import import_match
    from musicops.media.ncm import ncm_metadata
    cfg=service.config.current()['values'];download=tmp_path/'client-downloads';download.mkdir();audio=flac(download/'payload.flac');payload=audio.read_bytes();audio.unlink()
    ncm=download/'original.ncm';ncm.write_bytes(make_ncm(payload,{'format':'flac','musicName':'Song','artist':[['Artist',1]],'album':'Album'}));before=ncm.read_bytes()
    target={'raw_title':'Song','artists':['Artist'],'album':'Album','duration':2,'source_song_id':'42'}
    assert ncm_metadata(ncm)['album']=='Album' and import_match(target,str(download))==str(ncm)
    source=Path(cfg['paths']['source'])/'ncm';source.mkdir(parents=True);shutil.copy2(ncm,source/'42.ncm');job(service,sources=source,targets=[target]);finished=until_terminal(service)
    # Original embedded FLAC cover is preserved by decrypt/tag preparation.
    assert finished['stage']=='completed',finished
    assert ncm.read_bytes()==before and (source/'42.ncm').read_bytes()==before
    assert list(Path(cfg['paths']['library']).rglob('*.flac')) and not list(Path(cfg['paths']['library']).rglob('*.ncm'))

def test_open_cd_uses_cookie_button_instead_of_passkey_share_url(tmp_path):
    p=PTDownloadProvider('open.cd',{'base_url':'https://www.open.cd','credential_ref':''},FakeTransport(),lambda *a:None)
    page='<a href="download.php?id=3">下載種子</a><a href="https://www.open.cd/download.php?id=3&amp;passkey=virtual">share</a>'
    path,params,cost=p._button(page,{'resource_id':'3'},True)
    assert path=='/download.php' and params=={'id':'3'} and cost==0

def test_cancel_at_safe_end_does_not_reactivate_job(service):
    row=job(service);service.workflows.cancel(row['job_id']);service.workflows.put(row,stage='notifying',message='stale result')
    current=service.workflows.rows()[0];assert current['stage']=='cancelled' and current['cancel_requested'] and service.workflows.next_job() is None

def test_missing_cover_needs_explicit_user_skip_before_publication(service):
    cfg=service.config.current()['values'];source=Path(cfg['paths']['source'])/'cover';p=flac(source/'song.flac');tags=FLAC(p);tags.clear_pictures();tags.save();job(service,sources=source)
    finished=until_terminal(service);assert finished['review_kind']=='cover' and not list(Path(cfg['paths']['library']).rglob('*.flac'))
    service.workflows.review(finished['job_id'],'skip');finished=until_terminal(service);assert finished['stage']=='completed'

def test_portable_profile_precedes_stale_remembered_profile(tmp_path):
    from musicops.startup import default_profile,remember_profile
    root=tmp_path/'portable';(root/'profile').mkdir(parents=True);stale=tmp_path/'old-profile';(stale/'state').mkdir(parents=True);(stale/'state/musicops.sqlite').touch();data=tmp_path/'data';remember_profile(stale,local_appdata=data)
    assert default_profile(root/'MusicOps.exe',data)==root/'profile'

def test_setup_keeps_existing_vault_values_when_inputs_blank(service):
    cfg=service.config.current();values=copy.deepcopy(cfg['values']);values['qbt']['credential_ref']='vault://setup/qbt';service.config.save(values,cfg['version']);service.vault.set('vault://setup/qbt','{"username":"fixture","password":"private-virtual"}','127.0.0.1')
    saved=service.dispatch('save_setup',{'library':values['paths']['library'],'qbt':{'url':values['qbt']['base_url'],'username':'','password':''},'sites':{'redacted':{'enabled':False,'cookie_json':''}}})
    assert saved['config']['values']['qbt']['credential_ref']=='vault://setup/qbt' and 'private-virtual' not in json.dumps(saved)
    assert service.vault.get('vault://setup/qbt','127.0.0.1') is not None

def test_gazelle_free_download_confirms_hash_before_qbt_start(tmp_path,monkeypatch):
    content=bencode({b'info':{b'name':b'song.flac',b'piece length':16384,b'length':100,b'pieces':b'a'*20}});meta=parse_metainfo(content)
    responses=[Response(200,b'<a href="torrents.php?action=download&amp;id=3">DL</a>'),Response(200,content)]
    p=PTDownloadProvider('orpheus',{'base_url':'https://orpheus.network','credential_ref':'vault://test'},FakeTransport(responses),lambda *a:json.dumps([{'domain':'orpheus.network','path':'/','name':'session','value':'virtual'}]));monkeypatch.setattr(p,'detail',lambda _: {'group':{'id':2},'torrent':{'freeTorrent':'Personal','infoHash':meta['infohash_v1']}})
    store=StateStore(tmp_path/'state.sqlite');got,proof=p.fetch({'candidate_id':'orpheus:3','resource_id':'3'},store,'batch',0,tmp_path/'private.torrent')
    assert got==content and proof['confirmed'] and proof['infohash']==meta['infohash_v1']
    assert store.get('evidence','pt-request:orpheus:3')['state']=='confirmed'

def test_cover_at_album_root_is_inherited_by_all_discs(service):
    cfg=service.config.current()['values'];source=Path(cfg['paths']['source'])/'box/Album'
    for disc,title in ((1,'One'),(2,'Two')):
        path=flac(source/('CD'+str(disc))/(title+'.flac'),title=title,disc=str(disc));tags=FLAC(path);tags.clear_pictures();tags.save()
    (source/'cover.jpg').write_bytes(image());job(service,sources=source)
    finished=until_terminal(service);assert finished['stage']=='completed',finished
    outputs=list(Path(cfg['paths']['library']).rglob('*.flac'));assert len(outputs)==2 and all(FLAC(p).pictures for p in outputs)

def test_real_gazelle_empty_shape_confirms_no_resource_without_pagination():
    from musicops.providers.pt_readonly import PTReadOnlyProvider
    for site in ('dicmusic','orpheus','redacted'):
        host=HOSTS[site];p=PTReadOnlyProvider(site,{'base_url':'https://'+host,'credential_ref':'vault://test'},FakeTransport([Response(200,b'{"status":"success","response":{"results":[]}}')]),lambda *a:json.dumps([{'domain':host,'path':'/','name':'session','value':'virtual'}]))
        result=p.search('no matching album');assert result['status']=='NO_RESOURCE' and result['search_complete'] and not result['candidates']
