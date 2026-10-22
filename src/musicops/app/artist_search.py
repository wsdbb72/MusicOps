"""Paginated, read-only artist discography with conservative album/edition groups."""
import copy
import re
import uuid
from urllib.parse import urlsplit
from musicops.config.schema import digest
from musicops.media.chinese import alias_key
from musicops.domain.editions import album_base,family_base,describe,recording_kinds
from musicops.clients.http import HttpError,PolicyBlocked,SafeHttpTransport
from musicops.app.search_status import summarize,SITE_NAMES

SITES=tuple(SITE_NAMES)

def classify(candidate,artist):
    row=copy.deepcopy(candidate);release=row.get('release') or {};title=release.get('album') or row['title']
    row['artist_match']=alias_key(artist) in {alias_key(a) for a in row.get('artists',[])}
    if row['source']=='open.cd':
        parts=re.split(r'\s+[-–—]\s+',title,maxsplit=1)
        if len(parts)==2 and alias_key(parts[0])==alias_key(artist):
            title=parts[1];row['artist_match']=True;row['artists']=[artist]
            year=re.match(r'^(\d{4})\s*[-–—]\s*(.+)$',title)
            if year:release['year']=int(year[1]);title=year[2]
            title=re.sub(r'\s+[-–—]\s*(?:FLAC|ALAC|MP3|WAV|AAC|DSD)\b.*$','',title,flags=re.I).strip()
            title=re.sub(r'\s*[\[(](?:FLAC|ALAC|MP3|WAV|AAC|DSD|\d+\s*[- ]?bit)[^\])]*[\])]\s*$','',title,flags=re.I).strip()
            year=re.search(r'(?:\s*[\[(]|\s+)((?:19|20)\d{2})[\])]?\s*$',title)
            if year:release['year']=int(year[1]);title=title[:year.start()].strip()
            release['album']=title;row['release']=release
        else:row['grouping_uncertain']=True
    row['album_label']=title
    row['grouping_uncertain']=bool(row.get('grouping_uncertain') or not row['artist_match'] or not title)
    return row


def groups(candidates,artist):
    albums={}
    for row in candidates:
        title=row.get('manual_album_label') or row.get('ai_album_label') or row.get('album_label') or row['title'];release=row.get('release') or {}
        # Grouping is a display hint, never permission to equate recordings.
        key=digest([family_base(title),bool(row.get('grouping_uncertain') and not row.get('manual_grouping'))])
        album=albums.setdefault(key,{'group_id':key,'title':album_base(title) or title,'artist':artist,'year':None,'uncertain':bool(row.get('grouping_uncertain')),'versions':{},'identity_confirmed':False})
        original=describe(row)
        description=describe({**row,'title':title})
        description['group_id']=digest([description['group_id'],original['label']])
        description['label']=original['label']
        version=album['versions'].setdefault(description['group_id'],{'group_id':description['group_id'],'label':description['label'],'year':description['year'],'catalogue':description['catalogue'],'candidates':[],'identity_confirmed':False})
        version['candidates'].append(row)
    result=[]
    for album in albums.values():album['versions']=list(album['versions'].values());result.append(album)
    return sorted(result,key=lambda r:(r['uncertain'],str(r['year'] or ''),alias_key(r['title'])))


def assist_grouping(service,cfg,artist,candidates):
    pending=[r for r in candidates if r.get('grouping_uncertain') and not r.get('ai_grouping_attempted')]
    if not pending or not cfg['ai'].get('enabled'):return
    from musicops.providers.ai import AIProvider
    provider=AIProvider(cfg['ai'],SafeHttpTransport([urlsplit(cfg['ai']['base_url']).hostname],offline=False),service.vault.get)
    schema={'type':'object','properties':{'groups':{'type':'array','items':{'type':'object','properties':{'candidate_id':{'type':'string'},'album':{'type':'string'},'artist_matches':{'type':'boolean'}},'required':['candidate_id','album','artist_matches'],'additionalProperties':False}}},'required':['groups'],'additionalProperties':False}
    for offset in range(0,len(pending),24):
        chunk=pending[offset:offset+24];service._checkpoint('artist_ai_grouping');by_id={r['candidate_id']:r for r in chunk}
        for row in chunk:row['ai_grouping_attempted']=True
        context={'artist':artist,'candidates':[{'candidate_id':r['candidate_id'],'title':r['title'],'artists':r.get('artists',[]),'album':r.get('album_label')} for r in chunk]}
        try:
            result=provider._recognition_request('artist_album_grouping',schema,'根据已有资源标题核对艺人归属并提取专辑名称。只处理已有候选编号；专辑名必须来自原始标题，不编造唱片，不去掉现场、混音、重录、单曲等版本标注。无法确定的保留原文且 artist_matches=false。你仅提供分组线索，不确认录音身份。',context)
            if not isinstance(result,dict) or not isinstance(result.get('groups'),list):continue
            for entry in result['groups']:
                if not isinstance(entry,dict) or not isinstance(entry.get('candidate_id'),str):continue
                row=by_id.pop(entry.get('candidate_id'),None);label=entry.get('album')
                if not row or not isinstance(label,str) or not label.strip() or len(label)>300 or any(ord(ch)<32 for ch in label):continue
                if alias_key(label) not in alias_key(row['title']):continue
                if recording_kinds(label)!=recording_kinds(row['title']):continue
                row.update(ai_album_label=label,ai_artist_match=entry.get('artist_matches') is True,ai_grouping=True)
        except (HttpError,ValueError,TypeError,KeyError):continue


def search(service,args,cfg):
    artist=args.get('query','').strip() if isinstance(args.get('query'),str) else ''
    if not artist or len(artist)>200 or any(ord(c)<32 for c in artist):raise ValueError('请输入 1–200 字符的歌手名称')
    if cfg['offline']:raise PolicyBlocked('请完成连接设置后按歌手检索')
    result_id=args.get('artist_result_id');state=None
    if result_id:
        if not re.fullmatch(r'[0-9a-f]{32}',str(result_id)):raise ValueError('歌手检索记录无效')
        state=service.store.get('evidence','artist-search:'+result_id)
        if not state or state['artist']!=artist:raise ValueError('歌手检索已改变，请重新搜索')
    else:result_id=uuid.uuid4().hex
    state=copy.deepcopy(state) if state else {'artist':artist,'candidates':[],'next_pages':{s:1 for s in SITES},'source_outcomes':{},'seen_pages':{s:[] for s in SITES},'excluded_count':0}
    by_id={(r['source'],r['candidate_id']):r for r in state['candidates']};outcomes=state['source_outcomes'];failed=[]
    for site in SITES:
        if args.get('force_ai') or args.get('group_override'):continue
        if not cfg['providers'][site]['enabled']:outcomes[site]={'status':'DISABLED'};continue
        page=state['next_pages'].get(site)
        if not page or (outcomes.get(site,{}).get('status') not in (None,'OK','NO_RESOURCE','DISABLED') and not args.get('retry_sites')):continue
        service._checkpoint('artist_search_'+site)
        try:result=service._real_pt(site,cfg).search_artist(artist,page=page)
        except (HttpError,PolicyBlocked,OSError,TimeoutError) as exc:result={'status':getattr(exc,'error_class','NETWORK_ERROR'),'candidates':[]}
        previous=outcomes.get(site,{})
        if result.get('status') not in ('OK','NO_RESOURCE'):outcomes[site]={**previous,'status':result.get('status','PARSE_ERROR'),'search_complete':False};failed.append(site);continue
        rows=result.get('candidates',[]);signature=digest(sorted(str(r.get('candidate_id')) for r in rows))
        if signature in state['seen_pages'][site] and rows:
            outcomes[site]={'status':'PARSE_ERROR','search_complete':False,'message':'站点重复返回同一页，已停止自动翻页'};continue
        state['seen_pages'][site].append(signature)
        for raw in rows:
            row=classify(raw,artist)
            by_id.setdefault((site,row['candidate_id']),row)
        outcomes[site]={k:result.get(k) for k in ('status','page','pages','search_complete')}
        pages=result.get('pages')
        state['next_pages'][site]=page+1 if type(pages) is int and page<pages and page<1000 else None
        if len(by_id)>10000:
            state['next_pages']={s:None for s in SITES};state['result_limit']=True;break
    state['candidates']=list(by_id.values())[:10000]
    if args.get('force_ai'):
        for row in state['candidates']:
            if not row.get('manual_grouping'):row.update(grouping_uncertain=True,ai_grouping_attempted=False)
    override=args.get('group_override')
    if override:
        if not isinstance(override,dict) or not isinstance(override.get('album'),str) or not override['album'].strip() or len(override['album'])>300 or any(ord(c)<32 for c in override['album']):raise ValueError('请输入 1–300 字符的专辑分组名')
        row=next((r for r in state['candidates'] if r['candidate_id']==override.get('candidate_id')),None)
        if not row:raise ValueError('请先选择本次结果中的音源')
        row.update(manual_album_label=override['album'].strip(),manual_grouping=True,grouping_uncertain=False,artist_match=True)
    assist_grouping(service,cfg,artist,state['candidates'])
    for row in state['candidates']:service.store.put('candidates',row['candidate_id'],row)
    service.store.put('evidence','artist-search:'+result_id,state)
    summary=summarize(outcomes);albums=groups(state['candidates'],artist)
    more=any(page and outcomes.get(site,{}).get('status')=='OK' for site,page in state['next_pages'].items())
    complete=not state.get('result_limit') and all(o.get('search_complete') is True for o in outcomes.values() if o.get('status')!='DISABLED') and any(o.get('status')!='DISABLED' for o in outcomes.values())
    message=f'已发现 {len(albums)} 个专辑分组、{len(state["candidates"])} 个音源；按专辑展开查看版本及站点。'
    message+=' 仅展示已启用站点可见的资源；分页未知、限流和站点缺失资源均可能造成遗漏，不保证覆盖歌手全部发行。'
    if state.get('result_limit'):message+=' 已达单次展示 10000 条音源上限，请缩小检索范围。'
    return {'status':'OK','mode':'artist','query':artist,'artist_result_id':result_id,'albums':albums,'candidates':state['candidates'],'source_outcomes':outcomes,'has_more':more,'complete':complete,'excluded_count':state['excluded_count'],'message':message,'read_only':True,'music_writes':0,**summary}
