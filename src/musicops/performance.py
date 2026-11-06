"""Own-process background scheduling; never modifies another application's policy."""
import ctypes
import os
import struct
import threading,contextlib,functools,time
from concurrent.futures import ThreadPoolExecutor
from collections import deque

_POLICY = None
_IO_CONDITION=threading.Condition()
_IO_LIMIT=2
_IO_ACTIVE=0
_IO_LOCAL=threading.local()

def configure_io_limit(limit):
    global _IO_LIMIT
    if type(limit) is not int or not 1<=limit<=8:raise ValueError('磁盘同时读写任务数须为 1–8')
    with _IO_CONDITION:_IO_LIMIT=limit;_IO_CONDITION.notify_all()

@contextlib.contextmanager
def io_checkpoints(callback):
    previous=getattr(_IO_LOCAL,'checkpoint',None);_IO_LOCAL.checkpoint=callback
    try:yield
    finally:_IO_LOCAL.checkpoint=previous

@contextlib.contextmanager
def io_slot():
    global _IO_ACTIVE
    if getattr(_IO_LOCAL,'depth',0):
        _IO_LOCAL.depth+=1
        try:yield
        finally:_IO_LOCAL.depth-=1
        return
    checked=0
    while True:
        with _IO_CONDITION:
            if _IO_ACTIVE<_IO_LIMIT:_IO_ACTIVE+=1;break
            _IO_CONDITION.wait(.1)
        callback=getattr(_IO_LOCAL,'checkpoint',None)
        if callback and time.monotonic()-checked>=1:
            callback();checked=time.monotonic()
    _IO_LOCAL.depth=1
    try:yield
    finally:
        _IO_LOCAL.depth=0
        with _IO_CONDITION:_IO_ACTIVE-=1;_IO_CONDITION.notify_all()

def io_bound(function):
    @functools.wraps(function)
    def run(*args,**kwargs):
        with io_slot():return function(*args,**kwargs)
    return run

def parse_cpu_sets(data):
    rows=[];offset=0
    while offset+8<=len(data):
        size,kind=struct.unpack_from('<II',data,offset)
        if size<8 or offset+size>len(data):raise ValueError('Malformed CPU set record')
        if kind==0 and size>=24:
            ident,group,logical,core,cache,numa,efficiency,flags=struct.unpack_from('<IHBBBBBB',data,offset+8)
            rows.append(dict(id=ident,group=group,logical=logical,core=core,efficiency=efficiency,flags=flags))
        offset+=size
    if offset!=len(data):raise ValueError('Truncated CPU set record')
    return rows

def efficiency_sets(rows):
    # Higher EfficiencyClass is faster, less efficient (Microsoft's definition).
    classes={r['efficiency'] for r in rows}
    if len(classes)<2:return []
    lowest=min(classes)
    return [r for r in rows if r['efficiency']==lowest and not (r['flags']&2 and not r['flags']&4)]

def _kernel():
    from ctypes import wintypes as w
    k=ctypes.WinDLL('kernel32',use_last_error=True)
    signatures={
      'GetCurrentProcess':([],w.HANDLE),
      'OpenProcess':([w.DWORD,w.BOOL,w.DWORD],w.HANDLE),
      'CloseHandle':([w.HANDLE],w.BOOL),
      'GetSystemCpuSetInformation':([ctypes.c_void_p,w.ULONG,ctypes.POINTER(w.ULONG),w.HANDLE,w.ULONG],w.BOOL),
      'SetProcessDefaultCpuSets':([w.HANDLE,ctypes.POINTER(w.ULONG),w.ULONG],w.BOOL),
      'GetProcessAffinityMask':([w.HANDLE,ctypes.POINTER(ctypes.c_size_t),ctypes.POINTER(ctypes.c_size_t)],w.BOOL),
      'SetProcessAffinityMask':([w.HANDLE,ctypes.c_size_t],w.BOOL),
      'SetPriorityClass':([w.HANDLE,w.DWORD],w.BOOL),
      'SetProcessInformation':([w.HANDLE,ctypes.c_int,ctypes.c_void_p,w.DWORD],w.BOOL)}
    for name,(args,result) in signatures.items():
        fn=getattr(k,name,None)
        if fn is not None:fn.argtypes=args;fn.restype=result
    return k

def background_io_priority(handle):
    """Lower only the supplied owned process; verify the actual NT I/O hint.

    ProcessIoPriority=33 / IoPriorityVeryLow=0 (phnt ntpsapi/ntioapi).
    Native API support varies: failure is visible and never blocks startup.
    """
    result={'io_priority':'unavailable'}
    try:
        from ctypes import wintypes as w
        nt=ctypes.WinDLL('ntdll',use_last_error=True)
        nt.NtSetInformationProcess.argtypes=[w.HANDLE,ctypes.c_int,ctypes.c_void_p,w.ULONG]
        nt.NtSetInformationProcess.restype=ctypes.c_int32
        nt.NtQueryInformationProcess.argtypes=[w.HANDLE,ctypes.c_int,ctypes.c_void_p,w.ULONG,ctypes.c_void_p]
        nt.NtQueryInformationProcess.restype=ctypes.c_int32
        value=w.ULONG(0)
        status=nt.NtSetInformationProcess(handle,33,ctypes.byref(value),ctypes.sizeof(value))
        result['io_set_status']=f'0x{status & 0xffffffff:08x}'
        if status<0:return result
        actual=w.ULONG()
        status=nt.NtQueryInformationProcess(handle,33,ctypes.byref(actual),ctypes.sizeof(actual),None)
        if status>=0:result.update(io_priority='very_low' if actual.value==0 else 'other',io_priority_value=actual.value)
    except (OSError,AttributeError,ValueError):pass
    return result


def apply_background_policy(pid=None,process=None,*,io_background=False):
    global _POLICY
    result={'mode':'system','logical_processors':os.cpu_count() or 1,'efficiency_processors':0,'affinity':'system','low_priority':False}
    if os.name!='nt':
        if pid is None and process is None and hasattr(os,'nice'):
            try:os.nice(5);result['low_priority']=True
            except OSError:pass
        if pid is None and process is None:_POLICY=result
        return result
    handle=None;k=None
    try:
        from ctypes import wintypes as w
        k=_kernel();handle=int(process._handle) if process is not None else k.OpenProcess(0x0200|0x0400|0x1000|0x2000,False,pid) if pid else k.GetCurrentProcess()
        if not handle:return result
        result['low_priority']=bool(k.SetPriorityClass(handle,0x4000)) # BELOW_NORMAL
        if io_background:result.update(background_io_priority(handle))
        size=w.ULONG();k.GetSystemCpuSetInformation(None,0,ctypes.byref(size),handle,0)
        if not 0<size.value<=1024*1024:return result
        buffer=ctypes.create_string_buffer(size.value)
        if not k.GetSystemCpuSetInformation(buffer,size.value,ctypes.byref(size),handle,0):return result
        selected=efficiency_sets(parse_cpu_sets(buffer.raw[:size.value]))
        if selected:
            ids=(w.ULONG*len(selected))(*(r['id'] for r in selected))
            if k.SetProcessDefaultCpuSets(handle,ids,len(selected)):
                result.update(mode='efficiency',affinity='cpu_sets',efficiency_processors=len(selected))
            # A single processor group permits a strict, child-inherited mask.
            # Do not interpret group-relative indexes as global indexes.
            all_rows=parse_cpu_sets(buffer.raw[:size.value])
            if {r['group'] for r in all_rows}=={0}:
                current=ctypes.c_size_t();system=ctypes.c_size_t()
                if k.GetProcessAffinityMask(handle,ctypes.byref(current),ctypes.byref(system)):
                    mask=sum(1<<r['logical'] for r in selected)&current.value
                    if mask and k.SetProcessAffinityMask(handle,mask):
                        result.update(mode='efficiency',affinity='strict',efficiency_processors=mask.bit_count())
        # EcoQoS hints still work on homogeneous systems; never infer core indexes.
        class Power(ctypes.Structure):_fields_=[('Version',w.ULONG),('ControlMask',w.ULONG),('StateMask',w.ULONG)]
        power=Power(1,1,1)
        fn=getattr(k,'SetProcessInformation',None)
        result['eco_qos']=bool(fn and fn(handle,4,ctypes.byref(power),ctypes.sizeof(power)))
    except (OSError,AttributeError,ValueError,struct.error):
        result['detection_unavailable']=True
    finally:
        if pid and handle and k:k.CloseHandle(handle)
        if pid is None and process is None:_POLICY=result
    return result

def configure_child(process):
    # Use the handle returned by our own Popen: a PID can exit and be reused.
    if os.name=='nt' and getattr(process,'_handle',None) is not None:
        return apply_background_policy(process=process,io_background=True)
    return {'mode':'inherited'}

def policy_status():
    result=dict(_POLICY or {'mode':'system','efficiency_processors':0,'affinity':'system','low_priority':False})
    with _IO_CONDITION:result.update(io_concurrency=_IO_LIMIT,io_active=_IO_ACTIVE)
    return result

def io_workers():
    count=policy_status().get('efficiency_processors') or os.cpu_count() or 2
    return max(2,min(8,count))

def bounded_map(function,items,workers=None):
    """Bounded parallel I/O; no eager future for every library file."""
    workers=workers or io_workers();iterator=iter(items)
    with ThreadPoolExecutor(max_workers=workers,thread_name_prefix='MusicOpsFiles') as pool:
        pending=deque()
        for _ in range(workers*2):
            try:pending.append(pool.submit(function,next(iterator)))
            except StopIteration:break
        while pending:
            yield pending.popleft().result()
            try:pending.append(pool.submit(function,next(iterator)))
            except StopIteration:pass
