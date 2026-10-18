"""Stable task categories shared by pagination and whole-queue operations."""
STATES={'completed':('completed','restored','cancelled','skipped'),
        'review':('needs_review',),'failed':('failed',)}
TERMINAL=tuple(s for states in STATES.values() for s in states)

def predicate(state):
    if state in (None,'','all'):return '',()
    if state=='active':return ' AND json_extract(payload,\'$.stage\') NOT IN ('+','.join('?' for _ in TERMINAL)+')',TERMINAL
    if state not in STATES:raise ValueError('任务分类无效')
    stages=STATES[state]
    return ' AND json_extract(payload,\'$.stage\') IN ('+','.join('?' for _ in stages)+')',stages
