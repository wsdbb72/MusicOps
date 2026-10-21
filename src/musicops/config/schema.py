from __future__ import annotations
import copy,hashlib,json,os,shutil,sys,math,re
from pathlib import Path
from urllib.parse import urlparse
from musicops.state.store import utcnow

def digest(value): return hashlib.sha256(json.dumps(value,ensure_ascii=False,sort_keys=True,separators=(',',':')).encode()).hexdigest()
def default_config(base,offline=True):
    base=Path(base).resolve()
    paths={key:str((base/'demo' if offline else base)/key) for key in ('library','source','work','backup','quarantine','reports')}
    if os.environ.get('MUSICOPS_CONTAINER')=='1' and not offline:
        paths.update(library='/media/music',**{key:'/media/MusicOps/'+('downloads' if key=='source' else key) for key in ('source','work','backup','quarantine','reports')})
    tool_roots=[Path(getattr(sys,'_MEIPASS',Path(__file__).resolve().parents[3]))/'tools',Path(sys.executable).parent/'tools']
    if sys.platform=='darwin':tool_roots.extend([Path(sys.executable).parent.parent/'Resources/tools',Path.home()/'.local/bin',Path('/opt/homebrew/bin'),Path('/usr/local/bin')])
    suffix='.exe' if os.name=='nt' else ''
    paths['tools']={key:next((str(p/(key+suffix)) for p in tool_roots if (p/(key+suffix)).is_file()),shutil.which(key) or '') for key in ('ffmpeg','ffprobe')}
    return dict(schema_version=1,offline=offline,paths=paths,playlist_ids=[],
      providers={name:dict(enabled=bool(offline and name!='redacted' or name=='netease'),base_url=url,credential_ref='',search_language='auto',concurrency=1,timeout=15,rules_verified=False) for name,url in [('netease','https://music.163.com'),('dicmusic','https://dicmusic.com'),('open.cd','https://www.open.cd'),('orpheus','https://orpheus.network'),('redacted','https://redacted.sh')]},
      token_budget={'dicmusic':3,'orpheus':3,'redacted':3,'safety_margin':1,'balance_ttl':300},
      qbt=dict(base_url='http://127.0.0.1:8085',credential_ref='vault://qbt/local/credentials',category='MusicOps',tag_prefix='MusicOps',poll_seconds=5,auto_tmm=False),
      lyrics=dict(primary=dict(base_url='https://lyrics.example.invalid',lyrics_path='/lyrics',candidates_path='/jsonapi',cover_path='/cover',auth_mode='raw',auth_header='Authorization',auth_secret_ref='vault://lyrics/default/authorization'),direct_backup=dict(base_url='http://backup.example.invalid',enabled=False,auth_secret_ref='vault://lyrics/default/authorization'),public_fallback=dict(enabled=False,lyrics_url='https://api.lrc.cx/lyrics',cover_url='https://api.lrc.cx/cover',auth_mode='none'),fallback_on_auth_error=False,write_mode='sidecar',overwrite_existing='review',connect_timeout=3,read_timeout=12,max_attempts=2,total_budget=60,concurrency=1,min_cover_side=300,max_response_bytes=10_000_000,cache_ttl=86400,negative_ttl=86400),
      ai=dict(enabled=False,protocol='responses',base_url='https://ai.example.invalid/v1',model='',auth_secret_ref='vault://ai/default/key',timeout=60,max_output_tokens=2048,temperature=0.2,structured_outputs=True,allow_plan_only=True),
      catalog=dict(enabled=True,web_enabled=True,timeout=15,candidate_limit=20,cache_ttl=86400,cache_entries=128,musicbrainz=dict(enabled=True),discogs=dict(enabled=True,credential_ref='vault://catalog/discogs/token')),
      quality=dict(lossless_first=True,format_order=['flac','alac','wav','aiff','mp3','aac'],prefer_24bit=False,dsd_policy='preserve',multichannel='separate',upgrade_suggestions=True),
      metadata=dict(prefer_simplified=True,preserve_multivalue=True,directory_template='{albumartist}/{album} [{edition}]/CD{disc}/{track} {title}',track_padding=2,preserve_unknown=True,overwrite='review',locked_fields=[]),
      repair=dict(mode='report',fields=['album','albumartist','artist','title','date','tracknumber','discnumber'],backup_retention_days=90),
      dedup=dict(mode='report',same_release_required=True,permanent_delete=False),
      scheduler=dict(enabled=False,mode='report',timezone='Asia/Shanghai',playlist_time='06:30',health_weekday=6,health_time='07:00',missed='report_once',notify='meaningful_changes'),
      worker=dict(concurrency=1,keep_background=True,decode_timeout=60),
      netease=dict(batch_size=150,timeout=15,max_attempts=2,credential_ref='',strip_layers=['VipSongsDownload']),
      updates=dict(enabled=True),
      streaming=dict(managed=True,enabled=False,base_url='http://127.0.0.1:8080/music',credential_ref='',order=['netease','qqmusic','kuwo','kugou','migu','qishui','bilibili','applemusic'],timeout=30),
      browser_solver=dict(managed=True,enabled=False,base_url='http://127.0.0.1:8191',timeout=60,executable='',auto_start=False),
      workflow=dict(edition_policy='quality',setup_done=False,source_order=['open.cd','dicmusic','orpheus','redacted'],free_only=True,fl_per_batch=3,fl_unlimited=False,max_albums=50,netease_fallback=True,netease_api_url='',netease_downloader='',netease_download_args=[],netease_import_dir='',backup_days=7,prefer_24bit=True),
      music_servers=dict(roon=dict(enabled=False,library='',watched_folders=[],credential_ref='',client='Roon',host='127.0.0.1',port=9330,timeout=8),navidrome=dict(enabled=False,url='',username='',password_ref='',library='')),
      playlist_sources={key:dict(credential_ref='',timeout=15) for key in ('qqmusic','qishui','kugou','kuwo','migu','bilibili','applemusic','spotify','youtube')})

def upgrade_config(values,base):
    """Add new settings without overwriting an existing user's endpoint or secret ref."""
    fresh=copy.deepcopy(values)
    def merge(obj,defaults):
        if not isinstance(obj,dict):return
        for key,value in defaults.items():
            if key not in obj:obj[key]=copy.deepcopy(value)
            elif isinstance(value,dict):merge(obj[key],value)
    defaults=default_config(base,offline=fresh.get('offline',True))
    for section in ('browser_solver','streaming'):
        previous=fresh.get(section,{})
        if previous and 'managed' not in previous:
            previous['managed']=previous.get('base_url')==defaults[section]['base_url'] and not previous.get('executable')
    merge(fresh,defaults)
    fresh['qbt']['tag_prefix']='MusicOps'
    return fresh

def validate_config(values):
    from musicops.clients.http import is_secret_key
    def reject_plain_secrets(obj):
        if isinstance(obj,dict):
            for k,v in obj.items():
                if not isinstance(k,str):raise ValueError('配置字段名称须为文本')
                if not k.endswith('_ref') and is_secret_key(k): raise ValueError('普通配置不允许明文秘密，使用vault引用')
                reject_plain_secrets(v)
        elif isinstance(obj,list):
            for v in obj: reject_plain_secrets(v)
    reject_plain_secrets(values)
    def check_types(obj,template,label='配置'):
        if isinstance(template,dict):
            if not isinstance(obj,dict):raise ValueError(label+'须为对象')
            if set(obj)!=set(template):raise ValueError(label+'字段不完整或包含未知项')
            for key,default in template.items():check_types(obj[key],default,label+'.'+key)
        elif isinstance(template,list):
            if not isinstance(obj,list) or any(not isinstance(v,str) for v in obj):raise ValueError(label+'须为文本数组')
        elif isinstance(template,bool):
            if type(obj) is not bool:raise ValueError(label+'须为布尔值')
        elif type(template) in (int,float):
            if type(obj) not in (int,float) or not math.isfinite(obj) or (type(template) is int and type(obj) is not int):raise ValueError(label+'数值无效')
        elif not isinstance(obj,str):raise ValueError(label+'须为文本')
    check_types(values,default_config(Path.cwd()))
    from musicops.clients.browser_solver import validate_solver
    validate_solver(values['browser_solver'])
    from musicops.providers.streaming import validate as validate_streaming
    validate_streaming(values['streaming'])
    if values['workflow']['edition_policy'] not in ('quality','complete','special'):raise ValueError('发行筛选偏好无效')
    def bounded(obj,key,low,high,label):
        if not low<=obj[key]<=high:raise ValueError(label+'超出允许范围')
    def clean_url(value,label,https=True,origin=False):
        u=urlparse(value)
        if u.scheme not in (('https',) if https else ('http','https')) or not u.hostname or u.username or u.password or u.query or u.fragment: raise ValueError(label+'需干净URL，禁止嵌入身份、查询凭据和片段')
        if origin and u.path not in ('','/'): raise ValueError(label+'需服务origin，不含端点路径')
        try:u.port
        except ValueError:raise ValueError(label+'端口无效')
        return u
    if values.get('schema_version')!=1: raise ValueError('不支持的配置版本')
    for pid in values['playlist_ids']:
        if not re.fullmatch(r'[1-9]\d{0,18}',pid):raise ValueError('歌单ID须为有效正整数文本')
    roots=[]
    for key in ('library','source','work','backup','quarantine','reports'):
        p=Path(values['paths'][key])
        if not p.is_absolute(): raise ValueError(f'{key}必须是绝对路径')
        for parent in [p,*p.parents]:
            if parent.exists() and (parent.is_symlink() or getattr(parent,'is_junction',lambda:False)()): raise ValueError(f'{key}含重解析点')
        roots.append((key,os.path.normcase(str(p.resolve()))))
    for i,(a,p) in enumerate(roots):
        for b,q in roots[i+1:]:
            if Path(p).drive.lower()!=Path(q).drive.lower(): continue
            if os.path.commonpath([p,q]) in (p,q): raise ValueError(f'路径重叠：{a}/{b}')
    for name,provider in values['providers'].items():
        clean_url(provider['base_url'],name+'站点',origin=True)
        if type(provider.get('timeout')) not in (int,float) or not 1<=provider['timeout']<=120:raise ValueError('来源超时需为1–120秒')
    ai=values.get('ai')
    if ai:
        clean_url(ai['base_url'],'AI接口')
        if ai['protocol']!='responses':raise ValueError('AI协议目前仅支持Responses')
        if ai['enabled'] and not ai['model'].strip():raise ValueError('AI模型不能为空')
        if type(ai['timeout']) not in (int,float) or not 1<=ai['timeout']<=300:raise ValueError('AI超时需为1–300秒')
        if type(ai['max_output_tokens']) is not int or not 16<=ai['max_output_tokens']<=32768:raise ValueError('AI输出预算需为16–32768 tokens')
        bounded(ai,'temperature',0,2,'AI温度')
    l=values['lyrics']
    for key,low,high in [('timeout',1,120),('candidate_limit',1,50),('cache_ttl',0,604800),('cache_entries',1,512)]:bounded(values['catalog'],key,low,high,'在线识别 '+key)
    clean_url(l['primary']['base_url'],'歌词主服务',origin=True)
    clean_url(l['direct_backup']['base_url'],'歌词直连备用',https=False,origin=True)
    clean_url(values['qbt']['base_url'],'qBittorrent',https=False,origin=True)
    for key in ('lyrics_url','cover_url'):
        clean_url(l['public_fallback'][key],'公开回退')
    for key in ('lyrics_path','candidates_path','cover_path'):
        endpoint=l['primary'][key]
        if not endpoint.startswith('/') or endpoint.startswith('//') or any(x in endpoint for x in ('?','#','://','..')): raise ValueError('资源端点必须为无凭据的相对路径')
    if l['primary']['auth_mode']!='raw' or l['public_fallback']['auth_mode']!='none': raise ValueError('鉴权模式违反私有raw/公开none契约')
    if l['write_mode'] not in ('sidecar','embedded'): raise ValueError('资源写入模式仅支持sidecar或embedded')
    if not re.fullmatch(r"[!#$%&'*+.^_`|~0-9A-Za-z-]+",l['primary']['auth_header']):raise ValueError('歌词鉴权请求头名称无效')
    for key,low,high in [('connect_timeout',1,120),('read_timeout',1,120),('max_attempts',1,2),('total_budget',1,300),('concurrency',1,1),('min_cover_side',1,4096),('max_response_bytes',1024,20_000_000),('cache_ttl',0,604800),('negative_ttl',0,604800)]:bounded(l,key,low,high,'歌词 '+key)
    for key,low,high in [('batch_size',1,500),('timeout',1,120),('max_attempts',1,2)]:bounded(values['netease'],key,low,high,'网易云 '+key)
    for provider in values['providers'].values():bounded(provider,'concurrency',1,1,'来源并发')
    for source in values['playlist_sources'].values():bounded(source,'timeout',1,120,'歌单请求超时')
    bounded(values['qbt'],'poll_seconds',1,300,'qBittorrent轮询间隔')
    bounded(values['worker'],'concurrency',1,1,'后台并发')
    bounded(values['worker'],'decode_timeout',1,3600,'解码超时')
    flow=values['workflow']
    for key,low,high in [('fl_per_batch',0,100),('max_albums',1,500),('backup_days',1,365)]:bounded(flow,key,low,high,'工作流 '+key)
    if set(flow['source_order'])!={'open.cd','dicmusic','orpheus','redacted'} or len(flow['source_order'])!=4:raise ValueError('资源站顺序必须包含四个不同站点')
    if flow['netease_api_url']:
        u=clean_url(flow['netease_api_url'],'网易云 API',https=False)
        if u.scheme=='http' and u.hostname not in ('localhost','127.0.0.1','::1'):raise ValueError('远程网易云 API 请使用 HTTPS')
    for key in ('netease_import_dir','netease_downloader'):
        if flow[key] and not Path(flow[key]).is_absolute():raise ValueError('网易云下载目录和程序需要完整路径')
    if not flow['free_only']:raise ValueError('仅支持明确免费的站点下载')
    for server in values['music_servers'].values():
        if server.get('library') and not Path(server['library']).is_absolute():raise ValueError('音乐服务器目录须为绝对路径')
        if server.get('enabled') and not server.get('library'):raise ValueError('已启用音乐服务器须填写音乐目录')
    nav=values['music_servers']['navidrome']
    roon=values['music_servers']['roon']
    if roon['host'] not in ('localhost','127.0.0.1','::1'):raise ValueError('当前Roon只读验收仅连接本机Core')
    bounded(roon,'port',1,65535,'Roon端口');bounded(roon,'timeout',1,30,'Roon超时')
    if nav['url']:clean_url(nav['url'],'Navidrome',https=False,origin=True)
    if any(not Path(p).is_absolute() for p in values['music_servers']['roon']['watched_folders']):raise ValueError('Roon监听目录须为绝对路径')
    if values['dedup']['permanent_delete']: raise ValueError('首版不提供永久删除')
    if values['scheduler']['timezone']!='Asia/Shanghai': raise ValueError('当前业务时区为Asia/Shanghai')
    if values['scheduler']['mode']!='report': raise ValueError('首次调度只允许报告模式')
    bounded(values['scheduler'],'health_weekday',0,6,'调度星期')
    for key in ('playlist_time','health_time'):
        if not re.fullmatch(r'(?:[01]\d|2[0-3]):[0-5]\d',values['scheduler'][key]):raise ValueError('调度时间须为24小时制HH:MM')
    for val in values['token_budget'].values():
        if not isinstance(val,int) or val<0: raise ValueError('预算必须为非负整数')
    return values

class ConfigManager:
    def __init__(self,store,base,initial_offline=True):
        self.store=store; self.base=Path(base)
        if not store.list('config_versions'): self.save(default_config(base,offline=initial_offline),0)
        else:
            current=self.current();upgraded=upgrade_config(current['values'],base)
            if not upgraded['offline'] and upgraded['workflow'].get('setup_done'):
                from musicops.library.storage import storage_paths
                upgraded['paths'].update(storage_paths(upgraded['paths']['library']))
            if upgraded!=current['values']:self.save(upgraded,current['version'])
    def current(self):
        with self.store.connection() as c:row=c.execute('SELECT payload FROM config_versions ORDER BY CAST(id AS INTEGER) DESC LIMIT 1').fetchone()
        if row is None:raise RuntimeError('CONFIG_NOT_FOUND')
        return json.loads(row[0])
    def save(self,values,expected_version):
        values=upgrade_config(values,self.base)
        validate_config(values)
        with self.store.connection() as c:
            c.execute('BEGIN IMMEDIATE')
            row=c.execute('SELECT payload FROM config_versions ORDER BY CAST(id AS INTEGER) DESC LIMIT 1').fetchone()
            old=json.loads(row[0]) if row else None
            current=old['version'] if old else 0
            if expected_version!=current: raise RuntimeError('CONFIG_CONFLICT：请重新载入')
            fresh=copy.deepcopy(values)
            if old:
                for name,provider in fresh['providers'].items():
                    previous=old['values']['providers'].get(name)
                    if previous and provider['base_url']!=previous['base_url']: provider['credential_ref']=''; provider['rules_verified']=False
                if fresh['lyrics']['primary']['base_url']!=old['values']['lyrics']['primary']['base_url']: fresh['lyrics']['primary']['auth_secret_ref']=''
                if fresh['lyrics']['direct_backup']['base_url']!=old['values']['lyrics']['direct_backup']['base_url']: fresh['lyrics']['direct_backup']['auth_secret_ref']=''
                if fresh['qbt']['base_url']!=old['values']['qbt']['base_url']:fresh['qbt']['credential_ref']=''
                if old['values'].get('streaming') and fresh['streaming']['base_url']!=old['values']['streaming']['base_url']:fresh['streaming']['credential_ref']=''
                if fresh.get('ai') and old['values'].get('ai') and fresh['ai']['base_url']!=old['values']['ai']['base_url']:fresh['ai']['auth_secret_ref']=''
                if fresh['music_servers']['navidrome']['url']!=old['values']['music_servers']['navidrome']['url']:fresh['music_servers']['navidrome']['password_ref']=''
            validate_config(fresh)
            refs=[]
            def walk(x):
                if isinstance(x,dict):
                    for k,v in x.items():
                        if k.endswith('_ref') and v: refs.append(v)
                        else: walk(v)
                elif isinstance(x,list):
                    for v in x: walk(v)
            walk(fresh)
            value=dict(schema_version=1,config_id='default',version=current+1,created_at=utcnow(),values=fresh,hash=digest(fresh),secret_refs=sorted(set(refs)))
            c.execute('INSERT INTO config_versions VALUES(?,?,?)',(str(current+1),json.dumps(value,ensure_ascii=False),utcnow()))
        self.store.event('config_saved',{'version':value['version'],'hash':value['hash']})
        return value
    def export(self): return copy.deepcopy(self.current())
    def import_config(self,data,expected_version): return self.save(data.get('values',data),expected_version)
