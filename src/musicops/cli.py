import argparse,json,math,time
from pathlib import Path
from musicops.worker.client import WorkerClient

def main(argv=None):
    p=argparse.ArgumentParser(description='MusicOps shared-core CLI'); p.add_argument('--base',default=str(Path.cwd()/'profile')); p.add_argument('command',nargs='?',default='get_status'); p.add_argument('--payload',default='{}'); p.add_argument('--wait',action='store_true');p.add_argument('--wait-timeout',type=float,default=600,help='等待任务的秒数；0表示不限时');args=p.parse_args(argv)
    try:
        payload=json.loads(args.payload)
        if not isinstance(payload,dict):raise ValueError('CLI_PAYLOAD_MUST_BE_OBJECT')
        if not math.isfinite(args.wait_timeout) or args.wait_timeout<0:raise ValueError('CLI_INVALID_WAIT_TIMEOUT')
        client=WorkerClient(args.base); client.ensure_running(); result=client.call(args.command,payload)
        if args.wait and result.get('task_id'):
            deadline=time.monotonic()+args.wait_timeout if args.wait_timeout else None
            while True:
                task=client.call('get_task',{'task_id':result['task_id']})['task']
                if not task:raise RuntimeError('TASK_NOT_FOUND')
                if task['state'] not in ('queued','running','paused'): result=task; break
                if deadline is not None and time.monotonic()>=deadline:
                    print(json.dumps({'task_id':result['task_id'],'state':task['state'],'error':'CLI_WAIT_TIMEOUT；任务仍在后台保留'},ensure_ascii=False));return 3
                time.sleep(.2)
        print(json.dumps(result,ensure_ascii=False,indent=2))
        return 0 if result.get('state') not in ('failed','blocked','cancelled','recovery_required') else 2
    except Exception as e: print(json.dumps({'error':str(e)},ensure_ascii=False)); return 7
if __name__=='__main__': raise SystemExit(main())
