from pathlib import Path
from test_workflows import service,flac,job,until_terminal
from musicops.library.transactions import TransactionEngine
from musicops.library.storage import seed_root


def test_ssd_import_uses_hdd_atomic_publish_and_cleans_all_owned_audio_copies(service,tmp_path,monkeypatch):
    library=tmp_path/'hdd/music';ssd=tmp_path/'ssd/cache'
    saved=service.dispatch('save_setup',{'library':str(library),'temporary_storage':{'enabled':True,'directory':str(ssd)}})
    cfg=saved['config']['values'];source=Path(cfg['paths']['source'])/'test_job';audio=flac(source/'song.flac')
    # Simulate separate device IDs on CI; the final rename must be staged
    # on the library side, never across the configured SSD/HDD boundary.
    monkeypatch.setattr(TransactionEngine,'_volume',staticmethod(lambda path:1 if Path(path).is_relative_to(tmp_path/'hdd') else 2))
    row=job(service,sources=source,targets=[{'raw_title':'Song','artists':['Artist'],'album':'Album','duration':2}])
    finished=until_terminal(service,limit=12)
    assert finished['stage']=='completed',finished
    assert (library/'Artist/Album/01 Song.flac').is_file()
    assert not list(ssd.rglob('*.flac'))
    assert not audio.exists() and finished['temporary_cleanup_done']
    tx=service.store.get('transactions',finished['transaction_id'])
    assert Path(tx['directory_publication']['staging']).is_relative_to(tmp_path/'hdd/MusicOps/work/publication')
    assert tx['directory_publication']['renamed']
    assert seed_root(library)==tmp_path/'hdd/MusicOps/seeding'
    assert TransactionEngine(service.store,finished['engine_paths'],validator=service._validator(cfg)).execute(finished['publication_plan'])['idempotent']


def test_enabling_ssd_does_not_relocate_existing_hdd_job_snapshot(service,tmp_path):
    from musicops.app.storage_migration import prepare
    library=tmp_path/'hdd/music'
    service.dispatch('save_setup',{'library':str(library)})
    cfg=service.config.current()['values'];old=job(service,sources=Path(cfg['paths']['source'])/'test_job')
    before=dict(old['config']['paths'])
    service.dispatch('save_setup',{'temporary_storage':{'enabled':True,'directory':str(tmp_path/'ssd/cache')}})
    assert prepare(service.workflows,old)
    assert old['config']['paths']==before
    assert service.workflows.work_paths(old)['work']==before['work']
