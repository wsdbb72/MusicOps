"""Frozen IPC choice contract, synthetic only; never resolve or download audio."""
import copy


def check(client,store):
    client.call('pause_pipeline',{})
    created=[]
    try:
        for mode,choice in (('single','keep_single'),('full','playlist_album:scope-fixture-album')):
            ident='native-playlist-scope-'+mode;created.append(ident)
            song={'raw_title':'Synthetic Song','artists':['Synthetic Artist'],'album':'Synthetic Single','duration':180,'target_id':'scope-song'}
            candidate={'candidate_id':'scope-fixture-album','source':'musicbrainz','source_id':'release:synthetic-only',
                'title':'Synthetic Album','album':'Synthetic Album','artists':['Synthetic Artist'],'release_type':'Album',
                'tracklist_complete':True,'tracklist':[{'title':'Synthetic Song','artists':['Synthetic Artist'],'duration':180}]}
            store.put('evidence','workflow:'+ident,{'job_id':ident,'kind':'workflow','scene':'playlist','task_type':'identification',
                'stage':'needs_review','review_kind':'album_scope','prefer_full_album':True,'targets':[song],'old_files':[],
                'playlist_original_targets':[copy.deepcopy(song)],'catalog_choices':[candidate]})
            snapshot=client.call('get_workflow',{'job_id':ident})['job']
            if not snapshot['prefer_full_album'] or snapshot['review_kind']!='album_scope':return False
            client.call('review_workflow',{'job_id':ident,'choice':choice,'expected_revision':snapshot['review_revision']})
            current=client.call('get_workflow',{'job_id':ident})['job']
            if current['stage']!='identifying' or current['album_scope_decision']!=('single' if mode=='single' else 'full_pending'):return False
        return True
    finally:
        if created:client.call('bulk_workflows',{'action':'cancel','job_ids':created})
        client.call('resume_pipeline',{})
