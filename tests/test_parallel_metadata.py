import copy
import threading
from pathlib import Path
from test_workflows import service,flac


def test_parallel_metadata_changes_only_distinct_private_copies(service,monkeypatch):
    cfg=service.config.current()['values'];originals={}
    for number in range(2):
        source=Path(cfg['paths']['source'])/f'parallel-source-{number}'
        audio=flac(source/'song.flac');originals[audio]=audio.read_bytes()
        service.workflows.put({'kind':'workflow','job_id':f'metadata-{number}','batch_id':'batch','scene':'playlist','config':copy.deepcopy(cfg),'old_files':[],'targets':[],'stage':'metadata','label':'Album','progress':0,'prefer_24bit':True,'source_path':str(source)})
    enrich=service.workflows._enrich;barrier=threading.Barrier(2)
    def concurrent_enrich(job,prepared=None):
        barrier.wait(10)
        return enrich(job,prepared)
    monkeypatch.setattr(service.workflows,'_enrich',concurrent_enrich)
    assert service.workflows.advance()['parallel_count']==2
    rows=service.workflows.rows()
    assert {row['stage'] for row in rows}=={'validating'}
    assert len({row['prepared_root'] for row in rows})==2
    for row in rows:
        assert all(Path(item['path']).is_relative_to(Path(row['prepared_root'])) for item in row['prepared'])
    assert all(path.read_bytes()==before for path,before in originals.items())
    assert not service.store.list('transactions') and not service.store.list('download_tasks')
