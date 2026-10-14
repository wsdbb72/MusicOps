import os
from pathlib import Path

class WriterLock:
    def __init__(self,base): self.path=Path(base)/'state'/'writer.lock'; self.file=None
    def acquire(self):
        if self.file is not None:raise RuntimeError('WRITER_LOCK_ALREADY_ACQUIRED')
        self.path.parent.mkdir(parents=True,exist_ok=True); self.file=open(self.path,'a+b'); self.file.seek(0)
        try:
            if os.name=='nt':
                import msvcrt; msvcrt.locking(self.file.fileno(),msvcrt.LK_NBLCK,1)
            else:
                import fcntl; fcntl.flock(self.file,fcntl.LOCK_EX|fcntl.LOCK_NB)
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
            if os.name=='nt':
                import msvcrt; msvcrt.locking(self.file.fileno(),msvcrt.LK_UNLCK,1)
            self.file.close(); self.file=None
    def __enter__(self): return self.acquire()
    def __exit__(self,*args): self.release()
