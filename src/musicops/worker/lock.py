import os
from pathlib import Path
from musicops.platforms import lock_file,private_directory,is_windows

class WriterLock:
    def __init__(self,base): self.path=Path(base)/'state'/'writer.lock'; self.file=None
    def acquire(self):
        if self.file is not None:raise RuntimeError('WRITER_LOCK_ALREADY_ACQUIRED')
        private_directory(self.path.parent); self.file=open(self.path,'a+b'); self.file.seek(0)
        if not is_windows():os.chmod(self.path,0o600)
        try:
            lock_file(self.file)
            # Lock the first byte (including an empty file) before initializing it.
            # A competing initializer must never write into an already locked byte.
            self.file.seek(0,os.SEEK_END)
            if self.file.tell()==0:self.file.write(b'0');self.file.flush()
            self.file.seek(0)
        except OSError:
            self.file.close(); self.file=None; raise RuntimeError('WORKER_ALREADY_RUNNING')
        return self
    def release(self):
        if self.file:
            self.file.seek(0)
            lock_file(self.file,unlock=True)
            self.file.close(); self.file=None
    def __enter__(self): return self.acquire()
    def __exit__(self,*args): self.release()
