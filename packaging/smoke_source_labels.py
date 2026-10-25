"""Legacy read-model migration in a new, otherwise empty packaged profile."""
from pathlib import Path

FILE_ID='synthetic-source-credit'
JOB_ID='synthetic-source-credit'


def seed(store,library):
    path=Path(library)/'Fixture Artist'/'Fixture Album'/'missing-synthetic.flac'
    assert not path.exists()
    row={'file_id':FILE_ID,'path':str(path),'location_state':'library','raw_title':'Fixture Song','artists':['Fixture Artist'],'albumartist':['Qobuz UK'],'album':'Fixture Album','duration':120,'metadata_index_version':2,'size':1,'mtime_ns':1,'read_error':None}
    store.put('library_files',FILE_ID,row)
    store.put('evidence','workflow:'+JOB_ID,{'kind':'workflow','job_id':JOB_ID,'scene':'complete','stage':'completed','task_type':'album','label':'Fixture Album · Qobuz UK','old_files':[row]})


def check(client):
    rows=client.call('list_library_files',{'query':'Fixture Album'})['files'];row=next(row for row in rows if row.get('file_id')==FILE_ID)
    job=client.call('get_workflow',{'job_id':JOB_ID})['job'];albums=client.call('list_library_albums',{})['albums']
    return {'native_source_albumartist_migration':row.get('albumartist')==[] and row.get('raw_albumartist')==['Qobuz UK'] and bool(row.get('source_labels')) and job['label']=='Fixture Album · Fixture Artist' and not Path(row['path']).exists(),
            'native_source_albumartist_catalogue':len(albums)==1 and albums[0]['artists']==['Fixture Artist'] and albums[0]['release_confirmed'] is False}
