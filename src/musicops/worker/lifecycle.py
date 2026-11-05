"""Small process diagnostics independent of the SQLite write path."""
import atexit,json,os,threading,time,traceback
from pathlib import Path


def append(base,event,**fields):
    path=Path(base)/'state'/'lifecycle.jsonl'
    path.parent.mkdir(parents=True,exist_ok=True)
    try:
        if path.exists() and path.stat().st_size>2_000_000:path.replace(path.with_suffix('.previous.jsonl'))
        with path.open('a',encoding='utf-8') as stream:
            stream.write(json.dumps({'at':time.time(),'pid':os.getpid(),'event':event,**fields},ensure_ascii=False)+'\n')
    except OSError:pass


def install(base,role):
    import faulthandler,sys
    append(base,role+'_started')
    output=(Path(base)/'state'/(role+'-fault.log')).open('a',encoding='utf-8')
    faulthandler.enable(file=output)
    old=sys.excepthook
    def exception(kind,value,tb):
        # Stack locations only: exception values can contain URLs or secrets.
        append(base,role+'_unhandled',error_class=kind.__name__,frames=[{'file':Path(f.filename).name,'line':f.lineno,'function':f.name} for f in traceback.extract_tb(tb)[-12:]])
        old(kind,value,tb)
    sys.excepthook=exception
    atexit.register(lambda:append(base,role+'_stopped',reason='process_exit'))
    return output


def health(service):
    return {'pid':os.getpid(),'at':time.time(),'queue_alive':service.thread.is_alive(),
            'active':service.active,'paused':service.paused,
            'phase':getattr(service,'_last_phase',None),
            'progress_at':getattr(service,'_progress_at',None),
            'queue_error':getattr(service,'_queue_error',None)}


def monitor(service):
    def run():
        path=service.base/'state'/'worker-health.json'
        while not service.stop.is_set():
            try:
                tmp=path.with_suffix('.tmp');tmp.write_text(json.dumps(health(service)),encoding='utf-8');os.replace(tmp,path)
            except OSError:pass
            service.stop.wait(5)
    thread=threading.Thread(target=run,daemon=True,name='MusicOpsHealth');thread.start()
    return thread
