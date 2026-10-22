"""Manual four-site searches: literal input or a user-confirmed catalogue album."""
import re
from musicops.clients.http import PolicyBlocked
from musicops.domain.identity import strict_text
from musicops.app.search_status import search_site,search_album,edition_conflict,summarize,detail

SITES=('open.cd','dicmusic','orpheus','redacted')


def exact_album(candidate,album,artists):
    if edition_conflict(candidate.get('title',''),' '.join(artists)+' '+album):return False
    title=strict_text(candidate.get('title'));expected=strict_text(album)
    credits={strict_text(a) for a in candidate.get('artists',[])};wanted={strict_text(a) for a in artists}
    if title==expected and credits==wanted:return True
    if candidate.get('source')=='open.cd' and len(artists)==1:
        prefix=strict_text(artists[0])+' - '+expected
        if title.startswith(prefix):
            tail=title[len(prefix):]
            return not tail.strip() or bool(re.match(r'^\s*(?:\(?\d{4}\)?|\[?(?:flac|alac|mp3|wav|24\s*bit|16\s*bit)\b)',tail))
    return False


def manual_album_search(service,args,cfg):
    if cfg['offline']:raise PolicyBlocked('请完成连接设置后检索站点')
    mode=args.get('mode','literal');query=args.get('query','')
    if mode=='artist':
        from musicops.app.artist_search import search
        return search(service,args,cfg)
    if mode not in ('literal','precise') or not isinstance(query,str) or not query.strip() or len(query)>300:raise ValueError('请输入不超过 300 字符的专辑搜索内容')
    album=None;artists=[];identity=None
    if mode=='precise':
        candidate_id=args.get('candidate_id')
        if not candidate_id:
            album=query.strip();artist=args.get('artist','')
            if not isinstance(artist,str) or len(artist)>500:raise ValueError('艺人名称格式无效')
            if not artist and ' - ' in album:artist,album=album.split(' - ',1)
            artists=[a.strip() for a in artist.split(' / ') if a.strip()]
            found=service._catalog_command('identify_metadata',{'album':album,'artists':artists},cfg)
            albums=[c for c in found.get('candidates',[]) if c.get('entity_kind') in ('release','master') and (c.get('album') or c.get('title')) and c.get('artists')]
            return {'status':'NEEDS_CHOICE' if albums else 'NO_ALBUM_MATCH','albums':albums[:40],'lookup_links':found.get('lookup_links',[]),'sources':found.get('sources',[]),'message':'请选择要精准检索的专辑或发行。' if albums else '没有可确认的专辑资料；可以补充艺人名，或使用原样搜索。','read_only':True,'music_writes':0}
        evidence=service.store.get('evidence','catalog:'+str(candidate_id))
        if not evidence or evidence['candidate'].get('entity_kind') not in ('release','master'):raise PolicyBlocked('请先识别并选定专辑候选')
        resolved=service._catalog_command('resolve_metadata',{'candidate_id':candidate_id},cfg)
        identity=resolved.get('candidate')
        if not isinstance(identity,dict):raise PolicyBlocked('专辑详细资料未能确认，请重试识别或使用原样搜索')
        album=identity.get('album') or identity.get('title');artists=identity.get('artists',[])
        if not album or not artists:raise PolicyBlocked('专辑详细资料缺少规范专辑名或艺人，无法精准检索')
        query=' '.join(artists)+' '+album
        if len(query)>300:raise PolicyBlocked('识别后的完整艺人与专辑名称超过站点 300 字符检索上限，请使用原样搜索精简关键词')
    from musicops.app.workflow_actions import validate_sites
    selected_sites=validate_sites(args['sites'],cfg) if 'sites' in args else SITES
    outcomes={};candidates=[];excluded=0
    for site in SITES:
        if not cfg['providers'][site]['enabled'] or site not in selected_sites:outcomes[site]={'status':'DISABLED','candidate_count':0};continue
        service._checkpoint('manual_album_search_'+site)
        result=search_album(lambda:service._real_pt(site,cfg),query,album=album if mode=='precise' else '',checkpoint=lambda:service._checkpoint('manual_album_search_'+site))
        outcomes[site]={key:result.get(key) for key in ('status','search_complete','page','pages','attempts')}
        outcomes[site]['candidate_count']=len(result.get('candidates',[]))
        for raw in result.get('candidates',[]):
            candidate=dict(raw);matched=exact_album(candidate,album,artists) if mode=='precise' else None
            if mode=='precise' and not matched:excluded+=1;continue
            candidate['precise_match']=matched
            service.store.put('candidates',candidate['candidate_id'],candidate);candidates.append(candidate)
    candidates=list({(c['source'],c['candidate_id']):c for c in candidates}.values())
    configured=any(value['status']!='DISABLED' for value in outcomes.values())
    summary=summarize(outcomes)
    message=f'找到 {len(candidates)} 条资源。'+(f' 已排除 {excluded} 条专辑或艺人不一致的候选。' if excluded else '')
    if not configured:message='尚未启用资源站，请在连接与设置中导入 Cookie 并开启参与检索。'
    elif not summary['successful_sites']:message='已启用站点当前均不可用，请查看各站状态后重试。'
    elif summary['unavailable_sites']:message+=' 可访问站点的检索已完成，其他站点可稍后单独重试。'
    if summary['limited_sites']:message+=' 部分站点仅返回首页，当前结果不能用于断言全站无匹配。'
    return {'status':'NOT_CONFIGURED' if not configured else 'SEARCH_UNAVAILABLE' if not summary['successful_sites'] else 'OK' if candidates else 'NO_MATCH','mode':mode,'query':query,'album':identity,'candidates':candidates,'source_outcomes':outcomes,'excluded_count':excluded,'read_only':True,'music_writes':0,'message':message,**summary}
