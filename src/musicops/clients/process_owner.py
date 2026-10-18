"""Terminate only the service process tree created by this worker."""
import os
import signal
import subprocess


class ProcessOwner:
    def __init__(self, process):
        self.process = process
        self.handle = None
        if os.name == 'nt':
            import ctypes
            from ctypes import wintypes
            kernel = ctypes.WinDLL('kernel32', use_last_error=True)
            class Basic(ctypes.Structure):
                _fields_ = [('ProcessTime', ctypes.c_int64), ('JobTime', ctypes.c_int64), ('Flags', wintypes.DWORD),
                    ('MinWorking', ctypes.c_size_t), ('MaxWorking', ctypes.c_size_t), ('ActiveLimit', wintypes.DWORD),
                    ('Affinity', ctypes.c_size_t), ('Priority', wintypes.DWORD), ('Scheduling', wintypes.DWORD)]
            class IO(ctypes.Structure):
                _fields_ = [(name, ctypes.c_uint64) for name in ('ReadOp', 'WriteOp', 'OtherOp', 'ReadBytes', 'WriteBytes', 'OtherBytes')]
            class Extended(ctypes.Structure):
                _fields_ = [('Basic', Basic), ('IO', IO), ('ProcessMemory', ctypes.c_size_t), ('JobMemory', ctypes.c_size_t),
                    ('PeakProcess', ctypes.c_size_t), ('PeakJob', ctypes.c_size_t)]
            kernel.CreateJobObjectW.restype = wintypes.HANDLE
            kernel.SetInformationJobObject.argtypes = [wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD]
            kernel.AssignProcessToJobObject.argtypes = [wintypes.HANDLE, wintypes.HANDLE]
            kernel.CloseHandle.argtypes = [wintypes.HANDLE]
            handle = kernel.CreateJobObjectW(None, None)
            limits = Extended(); limits.Basic.Flags = 0x2000  # KILL_ON_JOB_CLOSE
            if not handle or not kernel.SetInformationJobObject(handle, 9, ctypes.byref(limits), ctypes.sizeof(limits)) or not kernel.AssignProcessToJobObject(handle, wintypes.HANDLE(process._handle)):
                if handle: kernel.CloseHandle(handle)
                process.terminate()
                raise OSError('无法为内置服务建立独立进程组')
            self.handle, self.kernel = handle, kernel

    def close(self):
        if self.handle:
            self.kernel.CloseHandle(self.handle); self.handle = None
        elif os.name != 'nt':
            try: os.killpg(self.process.pid, signal.SIGTERM)
            except ProcessLookupError: pass
        elif self.process.poll() is None:
            self.process.terminate()
        try: self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            if os.name != 'nt':
                try: os.killpg(self.process.pid, signal.SIGKILL)
                except ProcessLookupError: pass
            else: self.process.kill()
            self.process.wait(timeout=5)
