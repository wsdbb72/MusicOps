import copy,json,hashlib,time
from types import SimpleNamespace
from pathlib import Path
import httpx,pytest
from PySide6.QtWidgets import QApplication
from musicops.app.service import AppService
from musicops.app.download_policy import approve
from musicops.clients.http import FakeTransport,Response,PolicyBlocked,HttpError
from musicops.clients.qbt import QbtClient
from musicops.config.schema import default_config
from musicops.providers.pt_download import PTDownloadProvider,TokenApprovalRequired
from musicops.providers.streaming import StreamingProvider,matches,SongCards
from musicops.providers.netease_readonly import NetEaseReadOnlyProvider
from musicops.domain.editions import compatible,rank,audio_count
from musicops.state.store import StateStore
from test_dicmusic_fl import CONTENT,HASH,CANDIDATE,DL,FL
from test_task_center import window_for,job as ui_job
from test_workflows import flac,service,job,until_terminal
from musicops.library.index import index_library,sha256_file
from mutagen.flac import FLAC

@pytest.fixture(scope='module')
def app():return QApplication.instance() or QApplication([])

def pt(monkeypatch,site,responses,torrent=None):
 host={'dicmusic':'dicmusic.com','orpheus':'orpheus.network'}[site]
 p=PTDownloadProvider(site,{'base_url':'https://'+host,'credential_ref':'vault://fixture'},FakeTransport(responses),lambda *_:json.dumps([{'domain':host,'path':'/','name':'session','value':'fixture'}]))
 monkeypatch.setattr(p,'detail',lambda _: {'group':{'id':2},'torrent':torrent or {'freeTorrent':False,'infoHash':HASH}})
 return p

@pytest.mark.parametrize('site',['dicmusic','orpheus'])
def test_global_free_cannot_silently_replace_fl(monkeypatch,tmp_path,site):
 p=pt(monkeypatch,site,[Response(200,DL.encode())],{'freeTorrent':True})
 with pytest.raises(TokenApprovalRequired):p.fetch({**CANDIDATE,'candidate_id':site+':3'},StateStore(tmp_path/'db'),'batch',2,tmp_path/'cache')
 assert len(p.transport.requests)==1

@pytest.mark.parametrize('site',['dicmusic','orpheus'])
def test_insufficient_tokens_makes_no_download_request(monkeypatch,tmp_path,site):
 p=pt(monkeypatch,site,[Response(200,(DL+FL).encode()),Response(200,b'{"status":"success","response":{"id":1}}'),Response(200,b'{"status":"success","response":{"personal":{"freeleechTokens":0}}}')])
 store=StateStore(tmp_path/'db')
 with pytest.raises(TokenApprovalRequired):p.fetch({**CANDIDATE,'candidate_id':site+':3'},store,'batch',2,tmp_path/'cache')
 assert len(p.transport.requests)==3 and not store.list('token_reservations')

def token_job(service,id='one',batch='batch'):
 return service.workflows.put({'job_id':id,'batch_id':batch,'stage':'needs_review','review_kind':'token','scene':'playlist','selected':{**CANDIDATE,'source':'dicmusic'},'config':service.config.current()['values'],'targets':[],'old_files':[]})

def test_once_and_batch_consent_are_scoped_and_never_defaults(service):
 first=token_job(service);second=token_job(service,'two');third=token_job(service,'three','another')
 approve(service.workflows,first,'once')
 assert service.store.get('evidence','workflow:two')['stage']=='needs_review'
 assert service.store.get('evidence','batch-nonfree:batch') is None
 first=token_job(service);approve(service.workflows,first,'batch')
 assert service.store.get('evidence','workflow:two')['stage']=='requesting'
 assert service.store.get('evidence','workflow:three')['stage']=='needs_review'
 assert service.config.current()['values']['workflow']['free_only'] is True

def test_plain_download_requires_matching_human_consent(monkeypatch,tmp_path):
 store=StateStore(tmp_path/'db');store.put('evidence','nonfree-consent:fixture',{'approved_by':'user','scope':'once','batch_id':'batch','job_id':'one','candidate_id':CANDIDATE['candidate_id']})
 store.put('evidence','workflow:one',{'batch_id':'batch'})
 p=pt(monkeypatch,'dicmusic',[Response(200,DL.encode()),Response(200,CONTENT)])
 content,proof=p.fetch(CANDIDATE,store,'batch',0,tmp_path/'cache',consent_id='fixture')
 assert content==CONTENT and proof['free'] is False and proof['download_method']=='explicit-nonfree'
 assert all('usetoken' not in r['params'] for r in p.transport.requests)
 qbt=QbtClient(None,'http://127.0.0.1:8080',tmp_path/'source',store=store)
 from musicops.clients.metainfo import parse_metainfo
 assert qbt._free_matches(proof,parse_metainfo(content),'one')
 assert not qbt._free_matches(proof,parse_metainfo(content),'another')
 store.put('evidence','nonfree-consent:fixture',{'approved_by':'ai','scope':'batch','batch_id':'batch'})
 assert not qbt._free_matches(proof,parse_metainfo(content),'one')

@pytest.mark.parametrize('state,expected',[('active',{'running','searching'}),('review',{'needs_review'}),('failed',{'failed'}),('completed',{'completed','cancelled','skipped','restored'})])
def test_task_filters_and_selections_are_global(service,state,expected):
 for i,stage in enumerate(['running','searching','needs_review','failed','completed','cancelled','skipped','restored']):
  service.workflows.put({'job_id':str(i),'stage':stage,'scene':'playlist','created_at':'2026-01-01T00:00:'+str(i).zfill(2)+'Z'})
 result=service.workflows.list(state=state)
 assert {j['stage'] for j in result['jobs']}==expected
 assert set(service.workflows.list(state=state,selection=True)['job_ids'])=={j['job_id'] for j in result['jobs']}
 before=[j['job_id'] for j in service.workflows.list()['jobs']]
 oldest=service.store.get('evidence','workflow:0');service.workflows.put(oldest,message='new progress')
 assert [j['job_id'] for j in service.workflows.list()['jobs']]==before


def test_running_tree_refresh_preserves_viewport(app,tmp_path):
 window,client=window_for(tmp_path);window.resize(1280,850);window.nav.setCurrentRow(3);window.show()
 rows=[ui_job(str(i)) for i in range(160)];window.show_jobs({'jobs':rows,'total':160});app.processEvents()
 bar=window.job_table.verticalScrollBar();bar.setValue(75);app.processEvents();anchor=window.scroll_anchor()
 window.show_jobs({'jobs':[{**r,'progress':20} for r in rows],'total':160});app.processEvents()
 assert window.scroll_anchor()==anchor and bar.value()>50
 window.close()


def test_all_ai_registers_a_durable_backend_queue_without_gui_owned_chunks(app,tmp_path,monkeypatch):
 window,client=window_for(tmp_path);window.config['ai']['enabled']=True;calls=[]
 def call(command,payload,done=None,failed=None):
  calls.append((command,payload))
  if done:done({'registered':63,'message':'已登记后台核对队列'})
 monkeypatch.setattr(window,'call',call);monkeypatch.setattr(window,'refresh',lambda:None);window.all_task_action('ai')
 assert calls[0]==('queue_ai_reviews',{'all':True,'apply_verified':True})
 assert not any(command=='assist_workflows' for command,_ in calls)
 assert '后台核对队列' in window.notice.text()
 window.close()


def test_edition_preferences_use_tracks_not_cover_files():
 base={'source':'dicmusic','title':'Album','media':{'format':'FLAC','encoding':'16bit Lossless'},'track_count':15}
 hires={**base,'media':{'format':'FLAC','encoding':'24bit Lossless 96kHz'},'track_count':10}
 special={**base,'title':'Album (Deluxe Edition)','track_count':12}
 assert max([base,hires,special],key=lambda c:rank(c,'quality'))==hires
 assert max([base,hires,special],key=lambda c:rank(c,'complete'))==base
 assert max([base,hires,special],key=lambda c:rank(c,'special'))==special
 assert compatible('Album (Deluxe Edition)','Album') and not compatible('Album (Live)','Album')
 assert audio_count({'fileList':'01.flac{{{123}}}|||cover.jpg{{{20}}}|||02.flac{{{234}}}'})==2
 assert audio_count({'fileCount':99}) is None


def test_netease_complete_album_manifest_is_counted_and_bound():
 raw={'code':200,'album':{'id':2,'name':'Album','size':2},'songs':[{'id':i,'name':'Song '+str(i),'ar':[{'name':'Artist'}],'dt':1000} for i in (3,4)]}
 p=NetEaseReadOnlyProvider(FakeTransport([Response(200,json.dumps(raw).encode())]))
 tracks=p.album('2');assert len(tracks)==2 and tracks[1]['track']=='2' and all(t['source_album_id']=='2' for t in tracks)
 nested=copy.deepcopy(raw);nested['album']['songs']=nested.pop('songs')
 nested['album']['songs']=[{**r,'artists':r.pop('ar')} for r in nested['album']['songs']]
 p=NetEaseReadOnlyProvider(FakeTransport([Response(200,json.dumps(nested).encode())]));assert len(p.album('2'))==2
 raw['album']['size']=3;p=NetEaseReadOnlyProvider(FakeTransport([Response(200,json.dumps(raw).encode())]))
 with pytest.raises(HttpError):p.album('2')


def test_streaming_html_and_matching_reject_wrong_artist_or_preview():
 parser=SongCards();parser.feed('<li class="song-card" data-id="abc" data-source="qq" data-name="Song" data-artist="Artist" data-album="Album" data-duration="180" data-extra="{}"></li>')
 target={'raw_title':'Song','artists':['Artist'],'album':'Album','duration':180};song=parser.rows[0]
 assert matches(target,song)
 assert not matches(target,{**song,'artist':'Cover Artist'})
 assert not matches(target,{**song,'duration':30})


def test_go_bridge_real_audio_validation(service,tmp_path):
 audio=flac(tmp_path/'source.flac','Song');content=audio.read_bytes();seen=[]
 target={'raw_title':'Song','artists':['Artist'],'album':'Album','duration':1}
 def handler(request):
  seen.append(request)
  if request.url.path.endswith('/search'):
   return httpx.Response(200,text='<li class="song-card" data-id="abc" data-source="qq" data-name="Song" data-artist="Artist" data-album="Album" data-duration="1" data-extra="{}"></li>')
  return httpx.Response(200,content=content)
 client=httpx.Client(transport=httpx.MockTransport(handler));settings=service.config.current()['values']['streaming']
 provider=StreamingProvider(settings,lambda *_:None,client)
 result,proof=provider.download('qqmusic',target,tmp_path/'attempt',service._validator(service.config.current()['values']))
 assert result.suffix=='.flac' and proof['source']=='qqmusic' and result.read_bytes()==content
 assert len(seen)==2 and seen[1].url.params['source']=='qq'
 client.close()


def test_fill_publishes_only_missing_track_and_preserves_original(service,tmp_path):
 cfg=service.config.current()['values'];old=flac(Path(cfg['paths']['library'])/'Artist'/'Album'/'01 Song One.flac','Song One')
 before=old.read_bytes();source=tmp_path/'download';one=flac(source/'01.flac','Song One');two=flac(source/'02.flac','Song Two');tags=FLAC(two);tags['tracknumber']=['2'];tags.save()
 originals=index_library(cfg['paths']['library'],metadata_only=True);originals[0]['original_hash']=sha256_file(old)
 targets=[{'raw_title':title,'artists':['Artist'],'album':'Album','duration':1} for title in ('Song One','Song Two')]
 job(service,scene='fill',sources=source,old=originals,targets=targets)
 finished=until_terminal(service)
 assert finished['stage']=='completed' and old.read_bytes()==before
 files=list(Path(cfg['paths']['library']).rglob('*.flac'));assert len(files)==2
 tx=service.store.get('transactions',finished['transaction_id'])
 assert len([o for o in tx['operations'] if o['kind']=='publish' and o['destination'].endswith('.flac')])==1

def test_continuous_manual_review_submits_then_opens_next(app,tmp_path,monkeypatch):
 from musicops.gui.review import ReviewDialog
 window,client=window_for(tmp_path);sent=[]
 def call(command,payload,done=None,failed=None):
  if command=='get_workflow':result={'job':{'job_id':payload['job_id'],'stage':'needs_review','review_kind':'metadata','task_type':'identification','scene':'playlist','review_revision':'fixture','prepared':[{'raw_title':'Song','artists':['Artist'],'album':'Album','duration':1}]}}
  elif command=='review_workflow':sent.append(payload);result={'status':'OK'}
  else:result=client.call(command,payload)
  if done:done(result)
 monkeypatch.setattr(window,'call',call);window.manual_review_queue=['one','two'];window.next_manual_review()
 assert window.review_window.job['job_id']=='one'
 window.review_window.submit_metadata();app.processEvents()
 assert len(sent)==1 and sent[0]['job_id']=='one' and window.review_window.job['job_id']=='two'
 window.review_window.reject();app.processEvents();assert len(sent)==1
 window.close()

@pytest.mark.parametrize('exact',[True,False])
def test_full_ai_applies_only_locally_exact_candidate_and_never_paid_consent(service,monkeypatch,exact):
 from musicops.app.assistance import assist_workflows
 from musicops.app.recognition import RecognitionFallback
 candidate={'candidate_id':'dicmusic:3','source':'dicmusic','resource_id':'3','title':'Album','artists':['Artist'],'identity_exact':exact}
 row=job(service,targets=[{'raw_title':'Song','album':'Album','artists':['Artist'],'duration':1}])
 service.workflows.put(row,stage='needs_review',task_type='search',review_kind='candidate',choices=[candidate])
 fake=SimpleNamespace(recognize_candidate=lambda *_:{'candidate_id':'dicmusic:3','confidence':.99,'ambiguous':False,'reason':'observed fixture'})
 monkeypatch.setattr(RecognitionFallback,'_provider',lambda _:(fake,None))
 result=assist_workflows(service,{'job_ids':[row['job_id']],'apply_verified':True},row['config'])
 current=service.store.get('evidence','workflow:'+row['job_id'])
 assert current['stage']==('completed' if exact else 'needs_review')
 assert result['results'][0]['status']==('APPLIED' if exact else 'READY')
 assert not any(r.get('approved_by') for r in service.store.list('evidence'))
 assert not service.store.list('download_tasks') and not service.store.list('token_reservations')


def test_streaming_resume_skips_verified_completed_tracks(service,tmp_path,monkeypatch):
 from musicops.app.streaming_fallback import run
 import musicops.providers.netease_match as mapping
 import musicops.providers.netease_download as downloader
 targets=[{'target_id':str(i),'raw_title':name,'artists':['Artist'],'album':'Album','duration':1} for i,name in enumerate(['Song One','Song Two'])]
 row=job(service,targets=targets);calls=[];allow=[False]
 monkeypatch.setattr(mapping,'resolve_netease_target',lambda target,*a,**k:{**target,'source_song_id':target['target_id']})
 def download(reference,cfg,directory,*args):
  name=reference['raw_title'];calls.append(name)
  if name=='Song Two' and not allow[0]:raise HttpError('SOURCE_UNAVAILABLE','fixture')
  return str(flac(Path(directory)/'download.flac',name))
 monkeypatch.setattr(downloader,'download_track',download)
 run(service.workflows,row);assert row['stage']=='needs_review'
 allow[0]=True;run(service.workflows,row)
 assert row['stage']=='metadata' and calls==['Song One','Song Two','Song Two']
 assert len(list(Path(row['source_path']).glob('*.flac')))==2


def test_missing_album_manifest_needs_choice_before_full_completion(service,monkeypatch):
 from musicops.app.album_completion import manifest
 first={'raw_title':'Song','artists':['Artist'],'album':'Album','duration':100}
 choices=[{'candidate_id':'release-'+str(i),'source':'musicbrainz','source_id':'release:'+str(i),'entity_kind':'release'} for i in (1,2)]
 tracks=[{'raw_title':'Song','artists':['Artist'],'duration':100,'disc':1,'position':1},
         {'raw_title':'Song','artists':['Artist'],'duration':200,'disc':1,'position':2},
         {'raw_title':'Bonus','artists':['Artist'],'duration':300,'disc':1,'position':3}]
 def resolve(source,id):return {'candidate':{'source':source,'source_id':id,'album':'Album','title':'Album','tracklist_complete':True,'tracklist':tracks[:2] if id.endswith('1') else tracks}}
 monkeypatch.setattr(service,'_recognize_metadata',lambda *a,**k:{'candidates':choices})
 monkeypatch.setattr(service,'_catalog_resolver',lambda _:SimpleNamespace(resolve_candidate=resolve))
 row=job(service,scene='fill',old=[first]);service.workflows.put(row,stage='identifying',task_type='identification')
 manifest(service.workflows,row);assert row['stage']=='needs_review' and not row.get('search_job_id')
 service.workflows.review(row['job_id'],'completion_release:release-2')
 row=service.store.get('evidence','workflow:'+row['job_id']);manifest(service.workflows,row)
 assert row['stage']=='completed' and row['album_completion_missing']==2 and row['target_count']==3
 child=service.store.get('evidence','workflow:'+row['search_job_id']);assert len(child['targets'])==3


def test_fl_cost_in_title_and_conflicting_costs_are_checked(monkeypatch):
 p=pt(monkeypatch,'orpheus',[])
 page='<a href="torrents.php?action=download&amp;id=3&amp;usetoken=1" title="Use 3 tokens">FL</a>'
 assert p._button(page,CANDIDATE,False)[2]==3
 with pytest.raises(PolicyBlocked):p._button(page.replace('>FL<','>FL (2)<'),CANDIDATE,False)

def test_cached_torrent_never_proves_current_token_free_status(monkeypatch,tmp_path):
 from musicops.clients.metainfo import parse_metainfo
 store=StateStore(tmp_path/'db');cache=tmp_path/'cache';cache.write_bytes(CONTENT)
 store.put('evidence','pt-request:dicmusic:3',{'state':'confirmed','metainfo_sha256':parse_metainfo(CONTENT)['metainfo_sha256'],'free_evidence':{'confirmed':True,'infohash':HASH,'source':'dicmusic','resource_id':'3'}})
 p=pt(monkeypatch,'dicmusic',[Response(200,DL.encode())])
 with pytest.raises(TokenApprovalRequired):p.fetch(CANDIDATE,store,'batch',1,cache)
 assert len(p.transport.requests)==1 and p.transport.requests[0]['params']=={'id':'2','torrentid':'3'}


def test_bridge_rejects_actual_short_audio_even_when_metadata_claims_full_length(service,tmp_path):
 audio=flac(tmp_path/'preview.flac','Song');content=audio.read_bytes()
 def handler(request):
  if request.url.path.endswith('/search'):return httpx.Response(200,json={'songs':[{'id':'abc','source':'qq','name':'Song','artist':'Artist','album':'Album','duration':6}]})
  return httpx.Response(200,content=content)
 with httpx.Client(transport=httpx.MockTransport(handler)) as client:
  provider=StreamingProvider(service.config.current()['values']['streaming'],lambda *_:None,client)
  with pytest.raises(HttpError,match='试听'):
   provider.download('qqmusic',{'raw_title':'Song','artists':['Artist'],'album':'Album','duration':6},tmp_path/'attempt',service._validator(service.config.current()['values']))
 assert not list((tmp_path/'attempt').glob('*'))


def test_ai_cannot_grant_token_exception(service,monkeypatch):
 from musicops.app.assistance import assist_workflows
 from musicops.app.recognition import RecognitionFallback
 row=token_job(service);monkeypatch.setattr(RecognitionFallback,'_provider',lambda _:(object(),None))
 result=assist_workflows(service,{'job_ids':[row['job_id']],'apply_verified':True},row['config'])
 assert result['results'][0]['status']=='NEEDS_INPUT'
 assert service.store.get('evidence','workflow:'+row['job_id'])['stage']=='needs_review'
 assert not any(r.get('approved_by') for r in service.store.list('evidence'))
