from test_workflows import service  # noqa: F401  (fixture)


def test_empty_or_corrupt_flac_becomes_review_naming_the_file(service):
    from pathlib import Path
    from test_workflows import flac,job
    cfg=service.config.current()['values'];library=Path(cfg['paths']['library'])
    good=flac(library/'Artist/Album/01 Good.flac');bad=good.with_name('02 Ghost.flac');bad.write_bytes(b'\0'*4096)
    empty=good.with_name('03 Empty.flac');empty.write_bytes(b'')
    rows=[{'path':str(p),'raw_title':p.stem,'artists':['Artist'],'album':'Album','duration':2,'file_id':p.stem,'companions':[]} for p in (good,bad,empty)]
    row=job(service,scene='complete',old=rows);row['old_files']=rows;service.workflows.put(row)
    service.workflows.advance();done=service.workflows.rows()[0]
    assert done['stage']=='needs_review' and done['error_class']=='UNREADABLE_AUDIO',done
    assert '02 Ghost.flac' in done['message'] and '03 Empty.flac' in done['message'] and '01 Good.flac' not in done['message']
    assert bad.read_bytes()==b'\0'*4096 and empty.read_bytes()==b''


def test_untagged_disc_image_skips_web_lookup_instead_of_failing():
    from musicops.providers.catalog import CatalogResolver
    resolver=CatalogResolver.__new__(CatalogResolver);resolver.offline=False;resolver.config={'enabled':True,'web_enabled':True}
    resolver.website=lambda:(_ for _ in ()).throw(AssertionError('must not search'))
    assert resolver.web_identify({'title':'','album':'','artists':['Somebody']})['status']=='NO_QUERY'


def test_local_completion_without_any_cover_still_publishes_other_repairs(service):
    from pathlib import Path
    from mutagen.flac import FLAC
    from test_workflows import flac,until_terminal
    cfg=service.config.current()['values'];library=Path(cfg['paths']['library'])
    one=flac(library/'Artist/Album/CD1/one.flac',disc='1/1');two=flac(library/'Artist/Album/CD2/two.flac',title='Two',disc='2/1')
    for p in (one,two):
        tags=FLAC(p);tags.clear_pictures();tags.save()
    assert service.workflows.start({'scene':'complete'},cfg)['album_count']==1
    finished=until_terminal(service)
    assert finished['stage']=='completed' and finished.get('cover_missing')==2,finished
    assert FLAC(one)['discnumber']==['1/2'] and FLAC(two)['discnumber']==['2/2']
    assert '未找到可靠封面：2 首' in finished['detail']


def test_store_download_with_blank_picture_mime_is_repaired_not_rejected(service):
    from pathlib import Path
    from mutagen.flac import FLAC,Picture
    from test_workflows import flac,until_terminal,image
    cfg=service.config.current()['values'];library=Path(cfg['paths']['library'])
    song=flac(library/'Artist/Album/01 Song.flac',disc='1/1')
    tags=FLAC(song);pic=Picture();pic.type=3;pic.mime='';pic.width=pic.height=0;pic.data=image();tags.clear_pictures();tags.add_picture(pic)
    junk=Picture();junk.type=4;junk.mime='image/jpeg';junk.data=b'not an image';tags.add_picture(junk);tags.save()
    assert service.workflows.start({'scene':'complete','refresh_resources':True},cfg)['album_count']==1
    finished=until_terminal(service)
    assert finished['stage']=='completed',finished
    pictures=FLAC(song).pictures
    assert [(p.type,p.mime,p.width,p.height) for p in pictures]==[(3,'image/jpeg',400,400)] and pictures[0].data==image()
