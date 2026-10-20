"""Packaged GUI + named-pipe worker + real media transaction in an isolated profile."""
import argparse,copy,hashlib,io,json,os,shutil,subprocess,time,wave
from pathlib import Path
from mutagen.flac import FLAC,Picture
from PIL import Image,ImageDraw
from musicops.worker.client import WorkerClient
from musicops.state.store import StateStore
from musicops.config.schema import ConfigManager

def main():
    p=argparse.ArgumentParser();p.add_argument('--bundle',required=True);p.add_argument('--profile',required=True);p.add_argument('--report',required=True);p.add_argument('--screenshot',required=True);p.add_argument('--ffmpeg',required=True);p.add_argument('--ffprobe',required=True);args=p.parse_args()
    bundle=Path(args.bundle).resolve();base=Path(args.profile).resolve();base.mkdir(parents=True,exist_ok=True)
    manager=ConfigManager(StateStore(base/'state/musicops.sqlite'),base,initial_offline=False);current=manager.current();cfg=current['values'];cfg['paths']['tools']={'ffmpeg':args.ffmpeg,'ffprobe':args.ffprobe};cfg['workflow']['setup_done']=True;cfg['catalog']['enabled']=False;cfg['catalog']['musicbrainz']['enabled']=False;cfg['catalog']['discogs']['enabled']=False;cfg['providers']['netease']['enabled']=False;manager.save(cfg,current['version'])
    library=Path(cfg['paths']['library']);original={};img=Image.new('RGB',(400,400),'navy');ImageDraw.Draw(img).rectangle((20,20,200,350),fill='gold');image=io.BytesIO();img.save(image,'JPEG')
    for disc,title in ((1,'One'),(2,'Two')):
        folder=library/'Fixture Artist/Fixture Album'/('CD'+str(disc));folder.mkdir(parents=True,exist_ok=True);wav=folder/(title+'.wav');audio=wav.with_suffix('.flac')
        with wave.open(str(wav),'wb') as out:out.setparams((1,2,44100,0,'NONE','not compressed'));out.writeframes(b'\0'*176400)
        result=subprocess.run([args.ffmpeg,'-v','error','-i',str(wav),'-c:a','flac','-n',str(audio)],capture_output=True,creationflags=getattr(subprocess,'CREATE_NO_WINDOW',0));assert result.returncode==0;wav.unlink()
        tags=FLAC(audio);tags.update(title=[title],artist=['Fixture Artist'],album=['Fixture Album'],albumartist=['Fixture Artist'],tracknumber=['1'],discnumber=[str(disc)+'/1'],totaldiscs=['1']);pic=Picture();pic.type=3;pic.mime='image/jpeg';pic.data=image.getvalue();tags.add_picture(pic);tags.save();original[str(audio)]=hashlib.sha256(audio.read_bytes()).hexdigest();audio.with_suffix('.lrc').write_text('[00:00.00]Fixture\n[00:01.00]Test','utf-8')
    env=os.environ.copy();env['PATH']=str(Path(os.environ['SystemRoot'])/'System32');env['QT_QPA_PLATFORM']='offscreen'
    for key in ('PYTHONPATH','PYTHONHOME','VIRTUAL_ENV'):env.pop(key,None)
    screenshot=Path(args.screenshot).resolve();screenshot.parent.mkdir(parents=True,exist_ok=True)
    process=subprocess.Popen([str(bundle/'MusicOps.exe'),'--base',str(base),'--screenshot',str(screenshot),'--quit-after','180000'],cwd=bundle,env=env,creationflags=getattr(subprocess,'CREATE_NO_WINDOW',0));client=WorkerClient(base);report={'version':__import__('musicops').__version__,'external_requests':0,'evidence':'packaged_exe_named_pipe_synthetic_media','checks':{}}
    try:
        deadline=time.monotonic()+90
        while True:
            try:status=client.call('get_status',{'compact':True});break
            except (OSError,EOFError):
                if time.monotonic()>deadline:raise
                time.sleep(.1)
        report['checks']['configured_profile_attached']=Path(status['base'])==base
        reply=client.call('start_workflow',{'scene':'complete'});deadline=time.monotonic()+20
        initial=client.call('list_workflows')['jobs']
        report['checks']['preparation_visible_immediately']=any(j['job_id']==reply['job_id'] and j.get('job_kind')=='batch' for j in initial)
        while time.monotonic()<deadline:
            task=client.call('get_task',{'task_id':reply['task_id']})['task']
            if task['state']=='completed':break
            if task['state'] not in ('queued','running'):raise RuntimeError(task.get('error','workflow start failed'))
            time.sleep(.1)
        report['checks']['one_album_not_two_discs']=task['result']['album_count']==1
        deadline=time.monotonic()+60
        while time.monotonic()<deadline:
            all_jobs=client.call('list_workflows')['jobs'];jobs=[j for j in all_jobs if j.get('job_kind')!='batch']
            if jobs and jobs[0]['stage'] in ('completed','failed','needs_review'):break
            time.sleep(.2)
        report['checks']['native_workflow_completed']=len(jobs)==1 and jobs[0]['stage']=='completed'
        report['checks']['consistent_multi_disc_tags']=all(FLAC(path)['discnumber']==[str(i)+'/2'] and FLAC(path)['totaldiscs']==['2'] for i,path in enumerate(original,1))
        report['checks']['same_name_synchronized_lyrics_preserved']=all(Path(path).with_suffix('.lrc').read_text('utf-8').startswith('[00:00.00]') for path in original)
        transactions=client.call('get_status',{'compact':True})['transactions'];report['checks']['native_transaction_committed']=any(t['state']=='COMMITTED' for t in transactions)
        report['checks']['original_backups_verified']=all(hashlib.sha256(Path(b['backup']).read_bytes()).hexdigest()==original[b['path']] for t in transactions for b in t['backup_manifest'] if b['path'] in original)
        process.wait(195);report['checks']['gui_exit_zero']=process.returncode==0;report['checks']['gui_screenshot_created']=screenshot.is_file()
        report['checks']['no_demo_cli_entry']=subprocess.run([str(bundle/'MusicOps.exe'),'--demo','--base',str(base)],capture_output=True,timeout=10).returncode==2
        report['checks']['brand_assets_shipped']=(bundle/'_internal/musicops/assets/musicops.ico').is_file()
        report['passed']=all(report['checks'].values())
    except Exception as exc:report.update(passed=False,error_class=type(exc).__name__,error=str(exc))
    finally:
        try:client.call('shutdown_worker')
        except Exception:pass
        if process.poll() is None:process.terminate();process.wait(10)
        Path(args.report).write_text(json.dumps(report,ensure_ascii=False,indent=2),'utf-8')
    print(json.dumps(report,ensure_ascii=True))
    if not report['passed']:raise SystemExit(1)

if __name__=='__main__':main()
