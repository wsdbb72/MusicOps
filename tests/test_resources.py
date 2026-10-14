import io
import json
import threading
import time
import wave
import os,shutil,subprocess
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
import pytest
from PIL import Image,ImageDraw
from mutagen.wave import WAVE
from mutagen.id3 import TXXX,APIC
from mutagen.flac import FLAC,Picture
from mutagen.mp4 import MP4,MP4Cover,MP4FreeForm
from musicops.clients.http import FakeTransport,Response,HttpError
from musicops.media.resources import validate_lrc,validate_cover,shift_lrc,companion_relationships,rewrite_cue,resource_change_allowed
from musicops.media.tags import plan_tag_changes,apply_tag_changes,read_tag_fields,apply_cover_changes,embedded_covers
from musicops.providers.lrcapi import LrcApiClient

def candidates(title='星河',artist='示例艺人',lyrics='[00:00.00]星河\n[00:01.00]测试歌词',**extra):
    return Response(200,json.dumps([dict(id='virtual-1',title=title,artist=artist,lyrics=lyrics,**extra)],ensure_ascii=False).encode())

def image_bytes():
    image=Image.new('RGB',(400,400),'#19334A');ImageDraw.Draw(image).rectangle((30,30,200,350),fill='#ad498e');out=io.BytesIO();image.save(out,'JPEG');return out.getvalue()

def config(**kwargs):
    return dict(primary=dict(base_url='https://private.example',auth_secret_ref='vault://lyrics/test'),direct_backup=dict(base_url='http://backup.example',enabled=False),public_fallback=dict(enabled=False),**kwargs)

def test_V23_raw_authorization_complete_virtual_value():
    transport=FakeTransport([candidates()]);secret='  virtual-original-key (no-Bearer)  '
    client=LrcApiClient(config(),transport,lambda ref,host:secret)
    result=client.candidates('星河','示例艺人',duration=3)
    assert result['state']=='VALID_PAYLOAD' and result['candidates'][0]['selected']
    assert transport.requests[0]['headers']['Authorization']==secret
    assert 'path' not in transport.requests[0]['params']
    assert transport.requests[0]['params']=={'title':'星河','artist':'示例艺人'}

def test_observed_kugou_id_metadata_is_not_an_unsynchronized_lyric_line():
    text='[id:virtual-resource-id]\n[ar:Fixture]\n[00:00.00]fixture\n[00:01.00]line'
    assert validate_lrc(text,duration=3)['valid']
    assert not validate_lrc('[unsupported:value]\n[00:00.00]fixture',duration=3)['valid']

def test_V24_fallback_isolated_and_disabled_http_zero_requests():
    private=FakeTransport([Response(200,b'[]')]);public=FakeTransport([Response(200,b'[00:00.00]public fixture')])
    settings=config();settings['public_fallback']={'enabled':True,'lyrics_url':'https://public.example/lyrics'}
    result=LrcApiClient(settings,{'private':private,'public':public},lambda *args:'virtual-only').candidates('星河','示例艺人')
    assert result['state']=='VALID_PAYLOAD' and len(private.requests)==1 and len(public.requests)==1
    assert all(key.lower() not in ('authorization','cookie','sid') for key in public.requests[0]['headers'])
    assert all('backup.example' not in r['url'] for r in private.requests)
    private=FakeTransport([Response(302,b'',{'Location':'https://public.example/lyrics'})]);public=FakeTransport([])
    settings['public_fallback']['enabled']=False
    result=LrcApiClient(settings,{'private':private,'public':public},lambda *a:'virtual-only').candidates('星河','示例艺人')
    assert result['state']=='POLICY_BLOCKED' and len(private.requests)==1 and not public.requests

def test_V25_auth_error_stops_private_group_and_revision_recovers():
    class Secret:
        rev=1
        def __call__(self,ref,host):return 'virtual-only'
        def revision(self,ref):return self.rev
    secret=Secret();transport=FakeTransport([Response(403,b'forbidden'),candidates()]);client=LrcApiClient(config(),transport,secret)
    assert client.candidates('星河','示例艺人')['state']=='AUTH_FAILED'
    assert client.candidates('另一首','示例艺人')['state']=='AUTH_FAILED'
    assert len(transport.requests)==1
    secret.rev=2
    assert client.candidates('星河','示例艺人')['state']=='VALID_PAYLOAD' and len(transport.requests)==2

def test_V24_tls_failure_never_downgrades_even_backup_enabled():
    settings=config();settings['direct_backup']['enabled']=True;settings['public_fallback']={'enabled':True,'lyrics_url':'https://public.example/lyrics'}
    private=FakeTransport([HttpError('TLS_ERROR','certificate fixture')]);public=FakeTransport([Response(200,b'[00:00.00]public fixture')])
    result=LrcApiClient(settings,{'private':private,'public':public},lambda *a:'virtual-only').candidates('星河','示例艺人')
    assert result['state']=='VALID_PAYLOAD' and len(private.requests)==1 and len(public.requests)==1
    assert any(e['error_class']=='TLS_ERROR' for e in result['errors'])
    assert any(e['error_class']=='TLS_DOWNGRADE_BLOCKED' for e in result['errors'])

@pytest.mark.parametrize('response,state',[(Response(429,b'',{'Retry-After':'12'}),'RATE_LIMITED'),(Response(200,b'<html>error</html>'),'INVALID_PAYLOAD'),(Response(200,b'[]'),'NO_MATCH'),(Response(200,b'{"error":"schema"}'),'API_CONTRACT_ERROR')])
def test_V25_status_schema_not_no_match(response,state):
    client=LrcApiClient(config(),FakeTransport([response]),lambda *a:'virtual-only')
    assert client.candidates('星河','示例艺人')['state']==state

def test_V26_wrong_version_timestamps_plain_and_existing_preservation():
    client=LrcApiClient(config(),FakeTransport([candidates(title='星河 (Live)')]),lambda *a:'virtual-only')
    result=client.candidates('星河','示例艺人')
    assert result['state']=='MISMATCH' and not result['candidates'][0]['selected']
    assert 'UNORDERED_TIMESTAMP' in validate_lrc('[00:02]a\n[00:01]b')['errors']
    assert 'TIMESTAMP_OUT_OF_RANGE' in validate_lrc('[00:10]a',duration=2)['errors']
    assert not validate_lrc('plain lyrics')['valid']
    assert shift_lrc('[00:01.00]a',500)=='[00:01.50]a'
    with pytest.raises(ValueError):shift_lrc('[00:00.00]a',-100)
    valid={'validation':{'valid':True},'identity_state':'matched'}
    assert not resource_change_allowed(valid,valid)['allowed']
    assert not resource_change_allowed(None,valid,locked=True,approved=True)['allowed']

def test_V27_decodes_body_not_mime_and_placeholder_filter():
    client=LrcApiClient(config(),FakeTransport([Response(200,b'<html>bad</html>',{'Content-Type':'image/jpeg'})]),lambda *a:'virtual-only')
    assert client.cover('星河','示例艺人')['state']=='INVALID_PAYLOAD'
    assert not validate_cover(b'\xff\xd8corrupt')['valid']
    small=io.BytesIO();Image.new('RGB',(500,500),'white').save(small,'JPEG')
    assert 'PLACEHOLDER_IMAGE' in validate_cover(small.getvalue())['errors']
    assert validate_cover(image_bytes())['valid']

def test_V28_ttl_singleflight_and_response_limit():
    now=[0.0];transport=FakeTransport([candidates(),candidates()]);client=LrcApiClient(config(cache_ttl=10),transport,lambda *a:'virtual-only',clock=lambda:now[0])
    assert not client.candidates('星河','示例艺人')['cache_hit']
    now[0]=5;assert client.candidates('星河','示例艺人')['cache_hit']
    now[0]=11;assert not client.candidates('星河','示例艺人')['cache_hit'];assert len(transport.requests)==2
    def slow(_):time.sleep(.08);return candidates()
    transport=FakeTransport([slow]);client=LrcApiClient(config(),transport,lambda *a:'virtual-only')
    with ThreadPoolExecutor(max_workers=4) as executor:results=list(executor.map(lambda _:client.candidates('星河','示例艺人'),range(4)))
    assert all(r['state']=='VALID_PAYLOAD' for r in results) and len(transport.requests)==1
    assert sum(bool(r.get('singleflight_join')) for r in results)==3
    assert LrcApiClient(config(max_response_bytes=5),FakeTransport([candidates()]),lambda *a:'virtual-only').candidates('星河','示例艺人')['state']=='INVALID_PAYLOAD'

def test_V29_companion_scope_and_cue_references(tmp_path):
    audio=tmp_path/'01 song.flac';audio.write_bytes(b'synthetic');audio.with_suffix('.lrc').write_text('[00:00]a');(tmp_path/'cover.jpg').write_bytes(image_bytes());(tmp_path/'01 song.jpg').write_bytes(image_bytes());(tmp_path/'album.cue').write_text('FILE "01 song.flac" WAVE\nTRACK 01 AUDIO\n',encoding='utf-8')
    companions=companion_relationships(audio)
    assert len([r for r in companions if r['kind']=='lyrics'])==1
    assert len([r for r in companions if r['kind']=='cover'])==2
    assert all(not r['move_with_track'] for r in companions if r['scope']=='release')
    assert 'FILE "renamed.flac" WAVE' in rewrite_cue((tmp_path/'album.cue').read_text(),{'01 song.flac':'renamed.flac'})
    with pytest.raises(ValueError):rewrite_cue('FILE "01.flac" WAVE',{'01.flac':'../outside.flac'})

def test_T14_multivalue_unknown_pictures_field_lock_work_copy(tmp_path):
    source=tmp_path/'source.wav'
    with wave.open(str(source),'wb') as wav:wav.setparams((1,2,8000,0,'NONE','NONE'));wav.writeframes(b'\x00\x00'*800)
    tags=WAVE(source);tags.add_tags();tags.tags.add(TXXX(encoding=3,desc='private',text=['unknown','retained']));tags.tags.add(APIC(encoding=3,mime='image/jpeg',type=3,data=image_bytes()));tags.save()
    original=source.read_bytes();copy=tmp_path/'copy.wav';copy.write_bytes(original)
    plan=plan_tag_changes(source,{'artist':['A','B'],'album':['locked album']},locks=['album'])
    assert 'album' not in plan['changes']
    result=apply_tag_changes(copy,plan['changes']);readback=WAVE(copy)
    assert result['verified'] and read_tag_fields(copy)['artist']==['A','B']
    assert readback.tags['TXXX:private'].text==['unknown','retained'] and readback.tags.getall('APIC')[0].data==image_bytes()
    assert source.read_bytes()==original
    check=apply_cover_changes(copy,image_bytes());assert check['verified'] and image_bytes() in embedded_covers(copy)
    assert WAVE(copy).tags['TXXX:private'].text==['unknown','retained'] and source.read_bytes()==original

def test_V22_embedded_cover_flac_mp4_unknown_back_picture_and_unsupported(tmp_path):
    ffmpeg=shutil.which('ffmpeg')
    if not ffmpeg:pytest.skip('合成格式样本需要ffmpeg；缺工具不伪通过')
    wav=tmp_path/'synthetic.wav'
    with wave.open(str(wav),'wb') as stream:stream.setparams((1,2,8000,0,'NONE','NONE'));stream.writeframes(b'\x00\x00'*8000)
    for extension,codec in [('flac','flac'),('m4a','aac')]:
        path=tmp_path/('synthetic.'+extension)
        subprocess.run([ffmpeg,'-v','error','-i',str(wav),'-c:a',codec,str(path)],check=True,capture_output=True,creationflags=0x08000000 if os.name=='nt' else 0)
        if extension=='flac':
            obj=FLAC(path);obj['private_unknown']=['keep']
            for kind in (3,4):picture=Picture();picture.type=kind;picture.mime='image/jpeg';picture.data=image_bytes();obj.add_picture(picture)
        else:
            obj=MP4(path);obj['----:com.musicops:private']=[MP4FreeForm(b'keep')];obj['covr']=[MP4Cover(image_bytes()),MP4Cover(image_bytes())]
        obj.save();original=path.read_bytes();work=tmp_path/('copy.'+extension);work.write_bytes(original)
        assert apply_cover_changes(work,image_bytes())['verified'] and len(embedded_covers(work))==2
        if extension=='flac':assert FLAC(work)['private_unknown']==['keep'] and any(p.type==4 for p in FLAC(work).pictures)
        else:assert bytes(MP4(work)['----:com.musicops:private'][0])==b'keep'
        assert path.read_bytes()==original
    bad=tmp_path/'unknown.bin';bad.write_bytes(b'not audio')
    with pytest.raises(ValueError,match='UNSUPPORTED_TAG_FORMAT'):apply_cover_changes(bad,image_bytes())
    linked=tmp_path/'linked.flac';os.link(tmp_path/'synthetic.flac',linked)
    with pytest.raises(ValueError,match='HARDLINK_EDIT_FORBIDDEN'):apply_cover_changes(linked,image_bytes())
