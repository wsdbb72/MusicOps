"""Own-process background scheduling; never modifies another application's policy."""
import ctypes
import os
import struct
from concurrent.futures import ThreadPoolExecutor
from collections import deque

_POLICY = None

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

def apply_background_policy(pid=None,process=None):
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
        return apply_background_policy(process=process)
    return {'mode':'inherited'}

def policy_status():return dict(_POLICY or {'mode':'system','efficiency_processors':0,'affinity':'system','low_priority':False})

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
