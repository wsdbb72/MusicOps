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
