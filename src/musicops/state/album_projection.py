"""One indexed current head per album chain, including hidden successors.

Hidden successors must suppress old completed prerequisites. Rank first, then
apply visibility in reads; changing that order resurrects deleted jobs.
"""
FIELDS=('job_id','root_id','task_type','created_at','hidden','stage','job_kind','paused','label','review_kind','scene')

def initialize(db,*,rebuild=False):
    exists=db.execute("SELECT 1 FROM sqlite_master WHERE name='album_workflow_heads'").fetchone()
    db.execute('CREATE TABLE IF NOT EXISTS album_workflow_heads(root_key TEXT PRIMARY KEY,workflow_id TEXT NOT NULL,'+
               ','.join(key+' '+('INTEGER' if key in ('hidden','paused') else 'TEXT') for key in FIELDS)+')')
    columns='root_key,workflow_id,'+','.join(FIELDS)
    values="coalesce(root_id,''),id,"+','.join(FIELDS)
    def refresh(root):
        return ("DELETE FROM album_workflow_heads WHERE root_key=coalesce("+root+",''); "+'INSERT INTO album_workflow_heads ('+columns+') SELECT '+values+
                " FROM workflow_overview WHERE root_id IS "+root+" AND job_kind!='activity' ORDER BY task_priority DESC,created_at DESC,id DESC LIMIT 1; ")
    for action in ('insert','update','delete'):
        body=refresh('NEW.root_id') if action=='insert' else refresh('OLD.root_id') if action=='delete' else refresh('OLD.root_id')+refresh('NEW.root_id')
        condition=''
        if action=='update':condition=' WHEN '+ ' OR '.join('OLD.'+key+' IS NOT NEW.'+key for key in FIELDS)
        db.execute('CREATE TRIGGER IF NOT EXISTS album_head_'+action+' AFTER '+action.upper()+' ON workflow_overview'+condition+' BEGIN '+body+' END')
    if not exists or rebuild:
        db.execute('DELETE FROM album_workflow_heads')
        db.execute('INSERT INTO album_workflow_heads ('+columns+') SELECT '+values+
                   " FROM (SELECT id,"+','.join(FIELDS)+",row_number() OVER (PARTITION BY root_id ORDER BY task_priority DESC,created_at DESC,id DESC) AS position FROM workflow_overview WHERE job_kind!='activity') WHERE position=1")
    db.execute('CREATE INDEX IF NOT EXISTS album_head_state ON album_workflow_heads(hidden,stage,paused)')
    db.execute('CREATE INDEX IF NOT EXISTS album_head_recent ON album_workflow_heads(created_at DESC,workflow_id)')
