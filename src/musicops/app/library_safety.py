"""Read configured downloader ownership before moving watched-library originals."""
from pathlib import Path
import threading,time
from musicops.domain.errors import PolicyBlocked

_lock=threading.RLock()


def seeding_roots(service,cfg):
    # Serialize probes and invalidate failures immediately after credential edits.
    with _lock:
        ref=cfg.get('qbt',{}).get('credential_ref')
        refs={r['ref']:r.get('revision',0) for r in service.vault.list_refs()}
        key=(cfg.get('offline'),cfg.get('qbt',{}).get('base_url'),ref,refs.get(ref))
        cached=getattr(service,'_seeding_probe',None)
        if cached and cached['key']==key and cached['until']>time.monotonic():
            if cached.get('error'):raise PolicyBlocked(cached['error'])
            return list(cached['roots'])
        try:roots=_seeding_roots(service,cfg)
        except PolicyBlocked as exc:
            service._seeding_probe={'key':key,'until':time.monotonic()+300,'error':str(exc)}
            raise
        # Ownership can change immediately after a download is added. Cache only
        # failures; successful ownership reads must remain current before moves.
        service._seeding_probe=None
        return roots


def _seeding_roots(service,cfg):
    if cfg.get('offline') or not cfg.get('qbt',{}).get('credential_ref'):return []
    # The default configuration contains a placeholder vault reference even
    # before the user has ever connected a downloader.
    if cfg['qbt']['credential_ref'] not in {r['ref'] for r in service.vault.list_refs()}:return []
    from musicops.clients.qbt_live import QbtSession
    client=None
    try:
        client=QbtSession(cfg['qbt'],service.vault.get)
        response=client.request('GET',client.origin+'/api/v2/torrents/info')
        if response.status_code!=200:raise ValueError()
        rows=response.json()
        if not isinstance(rows,list) or any(not isinstance(r,dict) or not r.get('content_path') for r in rows):raise ValueError()
        return [Path(r['content_path']).absolute() for r in rows]
    except Exception:raise PolicyBlocked('无法确认 qBittorrent 做种目录，请先恢复下载器连接；原文件未改动') from None
    finally:
        if client:client.close()


def protect_seeding(service,cfg,paths):
    roots=seeding_roots(service,cfg)
    if any(path==root or path.is_relative_to(root) for path in map(lambda p:Path(p).absolute(),paths) for root in roots):
        raise PolicyBlocked('选中的原文件仍用于做种，请先在下载器中移至独立做种目录，再整理或移出音乐库副本')
