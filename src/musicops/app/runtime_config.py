"""Refresh identification policy without relocating an existing job's files."""
import copy
from musicops.config.schema import digest


def refresh(job,current,baseline_revision=None):
    values=current['values'];changed=[]
    if job.get('config_revision')==current['version']:return changed
    baseline=job.get('config_revision') is None and current['version']==baseline_revision
    hashes=job.get('config_policy_hashes',{})
    next_hashes={}
    for key in ('catalog','ai'):
        next_hashes[key]=digest(values[key])
        # Preserve deliberate legacy per-batch opt-outs on the first upgrade.
        # A newly disabled service applies immediately; unchanged global flags
        # must never silently re-enable thousands of costly AI/web requests.
        reference_changed=hashes.get(key)!=next_hashes[key]
        if reference_changed and not baseline:
            permitted=True
        else:permitted=baseline and values[key].get('enabled') is False
        if permitted and job['config'].get(key)!=values.get(key):
            job['config'][key]=copy.deepcopy(values[key]);changed.append(key)
        if key=='catalog' and baseline and values[key].get('web_enabled') is False:
            job['config'][key]['web_enabled']=False
    if job['config']['workflow'].get('artist_folders')!=values['workflow'].get('artist_folders'):
        job['config']['workflow']['artist_folders']=values['workflow'].get('artist_folders',True)
        changed.append('workflow.artist_folders')
    # Never change the policy behind already downloaded/prepared audio.
    if job.get('resume_stage',job.get('stage')) in ('queued','snapshotting','identifying','searching') and not job.get('publication_plan') and not job.get('prepared'):
        if job['config'].get('quality')!=values.get('quality'):
            job['config']['quality']=copy.deepcopy(values['quality']);changed.append('quality')
    job['config_revision']=current['version']
    job['config_policy_hashes']=next_hashes
    return changed
