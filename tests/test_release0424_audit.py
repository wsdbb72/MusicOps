from musicops.clients.http import redact


def test_error_text_masks_common_credential_query_parameters():
    for url in ('https://lrc.example.invalid/api?api_key=abc','https://pt.example.invalid/x?id=1&sid=abc',
                'https://pt.example.invalid/x?client_secret=abc','https://pt.example.invalid/rss?passkey=abc'):
        assert redact('failed: '+url+' end')=='failed: [SENSITIVE_URL] end'
    assert redact('https://music.example.invalid/album?id=1&side=a')=='https://music.example.invalid/album?id=1&side=a'


def test_playlist_presence_requires_same_version_release_and_unchanged_file(tmp_path):
    import copy,os
    from pathlib import Path
    from musicops.library.catalog import AlbumCatalog
    from musicops.state.store import StateStore
    from test_local_album_catalog import record,target
    root=tmp_path/'music';store=StateStore(tmp_path/'state.sqlite');catalog=AlbumCatalog(store)
    row=record(root,store,'Song.flac')
    catalog.sync(root,[row])
    wanted=target(row)
    assert catalog.matched_targets(root,[wanted])=={'selected'}
    # A generic version tag is not a different recording.
    for tag in ('','Original','Album Version'):
        catalog.sync(root,[{**row,'recording_version':tag}]);store.put('library_files',row['file_id'],{**row,'recording_version':tag})
        assert catalog.matched_targets(root,[wanted])=={'selected'},tag
    def check(changes,expected,**target_changes):
        changed={**copy.deepcopy(row),**changes};store.put('library_files',row['file_id'],changed);catalog.sync(root,[changed])
        assert catalog.matched_targets(root,[{**wanted,**target_changes}])==expected,(changes,target_changes)
    check({'recording_version':'Live'},set())
    check({'recording_version':'Live'},{'selected'},recording_version='(Live)',version='(live)')
    check({},set(),recording_version='(Remix)')
    check({'edition':''},set(),edition='Deluxe')
    check({'edition':'Deluxe'},{'selected'},edition='deluxe')
    check({'source_song_id':'111'},set(),source='netease',source_song_id='222')
    check({'source_song_id':'111'},{'selected'},source='qqmusic',source_song_id='222')
    check({'source_album_id':'5'},set(),source_album_id='6')
    check({'track':'3/12'},set(),track='4')
    check({'track':'03/12'},{'selected'},track='3')
    check({'filename_tag_conflict':True},set())
    check({'artists':['Unknown Artist']},set(),artists=['Unknown Artist'])
    check({'size':None},set())
    check({},{'selected'})
    # Same size but rewritten in place: the recorded mtime no longer matches.
    stat=Path(row['path']).stat();os.utime(row['path'],ns=(stat.st_atime_ns,stat.st_mtime_ns+10_000_000_000))
    assert catalog.matched_targets(root,[wanted])==set()


def test_docker_and_lan_services_may_use_http_but_public_hosts_need_https():
    import pytest
    from musicops.clients.http import local_service_host,PolicyBlocked
    from musicops.clients.qbt import readonly_preflight
    from musicops.clients.qbt_live import QbtSession
    from musicops.clients import navidrome,plex
    for host in ('127.0.0.1','localhost','::1','qbittorrent','qbittorrent-vpn','host.docker.internal','172.18.0.5',
                 '10.0.0.2','192.168.1.20','fd00::5','::ffff:192.168.1.2','nas.local','qbt.lan','169.254.1.1'):
        assert local_service_host(host),host
    for host in ('example.com','qbt.example.com','8.8.8.8','2001:4860::8888','0.0.0.0','224.0.0.1','','-bad'):
        assert not local_service_host(host),host
    missing=lambda *a:None
    assert readonly_preflight({'base_url':'http://qbittorrent:8080','credential_ref':'vault://q'},missing)['status']=='AUTH_REQUIRED'
    blocked=readonly_preflight({'base_url':'http://qbt.example.com:8080','credential_ref':'vault://q'},missing)
    assert blocked['status']=='POLICY_BLOCKED' and 'HTTPS' in blocked['reason']
    with pytest.raises(Exception) as caught:QbtSession({'base_url':'http://172.18.0.5:8081','credential_ref':'vault://q'},missing)
    assert not isinstance(caught.value,PolicyBlocked)
    with pytest.raises(PolicyBlocked):QbtSession({'base_url':'http://93.184.216.34:8080','credential_ref':'vault://q'},missing)
    with pytest.raises(Exception) as caught:navidrome.scan({'enabled':True,'url':'http://navidrome:4533','password_ref':'vault://n','username':'u'},missing)
    assert not isinstance(caught.value,PolicyBlocked)
    plex.validate({'enabled':True,'url':'http://plex:32400'})
    with pytest.raises(ValueError):plex.validate({'enabled':True,'url':'http://plex.example.com:32400'})
