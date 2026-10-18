"""Only an explicit review action can authorize a token-free exception."""
import uuid
from musicops.clients.http import PolicyBlocked
from musicops.state.store import utcnow

def approve(workflows,job,scope):
    candidate=job.get('selected') or {}
    if job.get('stage')!='needs_review' or job.get('review_kind')!='token' or candidate.get('source') not in ('dicmusic','orpheus'):
        raise PolicyBlocked('只有等待令牌确认的任务可在此授权普通下载')
    consent_id=str(uuid.uuid4())
    consent={'approved_by':'user','scope':scope,'batch_id':job['batch_id'],'job_id':job['job_id'],
             'candidate_id':candidate['candidate_id'],'created_at':utcnow()}
    workflows.store.put('evidence','nonfree-consent:'+consent_id,consent)
    if scope=='batch':workflows.store.put('evidence','batch-nonfree:'+job['batch_id'],{'consent_id':consent_id})
    workflows.put(job,nonfree_consent_id=consent_id,stage='requesting',review_kind=None,message='已收到普通下载授权，继续核对所选资源')
    if scope=='batch':
        for other in workflows.rows():
            if other.get('batch_id')==job['batch_id'] and other.get('stage')=='needs_review' and other.get('review_kind')=='token':
                workflows.put(other,nonfree_consent_id=consent_id,stage='requesting',review_kind=None,message='已收到本批次普通下载授权，等待核对资源')
    return {'status':'OK'}

def consent_for(store,job):
    batch=store.get('evidence','batch-nonfree:'+job['batch_id']) or {}
    return job.get('nonfree_consent_id') or batch.get('consent_id')
