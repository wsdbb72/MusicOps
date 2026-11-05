"""Download exclusions apply before token or downloader mutations."""
import re

def is_dsd(candidate):
    media=candidate.get('media') or {}
    text=' '.join(str(v) for v in [candidate.get('title',''),*media.values()])
    return bool(re.search(r'(?<![a-z0-9])(?:dsd\d*|dsf|dff|sacd)(?![a-z0-9])',text,re.I))

def allowed(candidate,config):
    return config.get('quality',{}).get('dsd_policy')!='reject' or not is_dsd(candidate)
