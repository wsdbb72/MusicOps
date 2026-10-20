from pathlib import Path
import pytest
from musicops.media.local_cover import local_cover
from musicops.media.chinese import alias_key,display_text
from musicops.providers.cover_download import cover
from test_workflows import image


def test_album_named_chinese_front_is_accepted_but_unrelated_photo_is_not(tmp_path):
    (tmp_path/'張惠妹 - 你在看我嗎.jpg').write_bytes(image())
    art=local_cover([tmp_path],'你在看我吗',['张惠妹'])
    assert art==image() and art.provenance['provider']=='local'
    assert local_cover([tmp_path],'另一张专辑',['张惠妹']) is None


def test_conflicting_same_priority_fronts_are_not_arbitrarily_selected(tmp_path):
    from PIL import Image,ImageDraw
    import io
    (tmp_path/'cover.jpg').write_bytes(image())
    other=Image.new('RGB',(400,400),'red');ImageDraw.Draw(other).ellipse((10,20,300,350),fill='white');out=io.BytesIO();other.save(out,'JPEG')
    (tmp_path/'folder.jpg').write_bytes(out.getvalue())
    assert local_cover([tmp_path],'Album',['Artist']) is None


def test_script_aliases_do_not_translate_unknown_or_japanese_identity():
    assert alias_key('  梁詠琪  ')==alias_key('梁咏琪')
    assert display_text('魔幻季節',language='zho')=='魔幻季节'
    assert display_text('東京',language='ja')=='東京'
    assert display_text('東京')=='東京'
    assert display_text('夢の歌',language='zh')=='夢の歌'


def test_verified_album_api_accepts_chinese_script_variant_and_keeps_origin(monkeypatch):
    class Reply:
        status_code=200
        def json(self):return {'album':{'name':'魔幻季节','artists':[{'name':'梁咏琪'}],'picUrl':'https://p1.music.126.net/front.jpg'}}
    class Client:
        def __init__(self,**kwargs):pass
        def __enter__(self):return self
        def __exit__(self,*args):pass
        def get(self,*args,**kwargs):return Reply()
    monkeypatch.setattr('musicops.providers.cover_download.httpx.Client',Client)
    monkeypatch.setattr('musicops.providers.cover_download._image',lambda *args:image())
    art=cover('魔幻季節',['梁詠琪'],album_id='123')
    assert art==image() and art.provenance['album_id']=='123' and art.provenance['provider']=='netease'
    assert cover('另一张专辑',['梁詠琪'],album_id='123') is None
