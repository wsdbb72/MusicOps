import os,sys,json,traceback
from pathlib import Path
if getattr(sys,'frozen',False):os.environ['PATH']=str(Path(sys.executable).parent/'tools')+os.pathsep+os.environ.get('PATH','')
if __name__=='__main__':
    try:
        from musicops.worker.server import main
        raise SystemExit(main())
    except Exception:
        base=Path(sys.argv[sys.argv.index('--base')+1]) if '--base' in sys.argv else Path(sys.executable).parent/'profile'
        base.mkdir(parents=True,exist_ok=True)
        (base/'worker-startup-error.txt').write_text(traceback.format_exc(),encoding='utf-8')
        raise
