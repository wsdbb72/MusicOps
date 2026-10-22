"""Share a platform's cookie permission with its own readers and download engine.

Web cookies are never substituted for an OAuth/developer token. Credential host
bindings are explicit; credentials cannot escape to a provider selected by search.
"""
import copy
import json
from musicops.providers.playlist_access import credential_hosts,validate_credential

ENGINE={'netease':'netease','qqmusic':'qq','qishui':'soda','applemusic':'apple',
        'kuwo':'kuwo','kugou':'kugou','migu':'migu','bilibili':'bilibili'}


def share(service,platform,text,from_engine=False):
    if platform not in ENGINE or not text:return False
    current=service.config.current();values=copy.deepcopy(current['values'])
    source=ENGINE[platform]
    if from_engine:
        if platform in ('applemusic',):return False
        host='music.163.com' if platform=='netease' else credential_hosts(platform)[0]
        rows=[]
        for part in text.split(';'):
            name,sep,value=part.strip().partition('=')
            if sep and name:rows.append({'name':name,'value':value,'domain':host,'path':'/','secure':True})
        if not rows:return False
        cookie_json=json.dumps(rows)
    else:
        # Apple Music API developer JWTs are not media-user-token cookies.
        if platform in ('applemusic',):return False
        cookie_json=text
        if platform!='netease':cookie_json=validate_credential(platform,text)
        rows=json.loads(cookie_json)
    if platform=='netease':
        ref='vault://netease/user/cookies';hosts=['music.163.com']
        values['netease']['credential_ref']=ref;values['providers']['netease']['credential_ref']=ref
    else:
        ref='vault://playlist/'+platform;hosts=credential_hosts(platform)
        values['playlist_sources'][platform]['credential_ref']=ref
    service.vault.set(ref,cookie_json,hosts)
    service.vault.set('vault://engine/'+source,'; '.join(row['name']+'='+row['value'] for row in rows),'127.0.0.1')
    if hasattr(service,'engines'):service.engines.credentials_changed()
    if values!=current['values']:service.config.save(values,current['version'])
    return True


def engine_login_shared(service,source):
    source='qq' if source=='qq_wx' else source
    platform=next((p for p,s in ENGINE.items() if s==source),None)
    if platform:
        try:value=service.vault.get('vault://engine/'+source,'127.0.0.1')
        except KeyError:return
        with service.mutex:share(service,platform,value,from_engine=True)
