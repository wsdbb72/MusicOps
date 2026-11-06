"""Compact task polling/selection data, updated atomically with task writes."""
FIELDS=('task_id','command','state','created_at','started_at','finished_at','progress','step',
        'safe_phase','error','error_class','cancel_requested','cancel_result','id','schema_version')

def initialize(db):
    exists=db.execute("SELECT 1 FROM sqlite_master WHERE name='task_queue_overview'").fetchone()
    db.execute('CREATE TABLE IF NOT EXISTS task_queue_overview(id TEXT PRIMARY KEY,state TEXT,command TEXT,priority INTEGER,updated_at TEXT,summary TEXT NOT NULL)')
    selected=','.join("'"+field+"'" for field in FIELDS)
    summary="(SELECT json_group_object(key,CASE WHEN type IN ('object','array') THEN json(value) ELSE value END) FROM json_each(NEW.payload) WHERE key IN ("+selected+'))'
    priority="CASE json_extract(NEW.payload,'$.command') WHEN 'refresh_playlist' THEN 0 WHEN 'manual_album_search' THEN 1 WHEN 'identify_metadata' THEN 1 WHEN 'resolve_metadata' THEN 1 WHEN 'accept_metadata' THEN 1 WHEN 'assist_workflows' THEN 3 WHEN 'advance_workflows' THEN 4 ELSE 2 END"
    values="NEW.id,json_extract(NEW.payload,'$.state'),json_extract(NEW.payload,'$.command'),"+priority+',NEW.updated_at,'+summary
    for action in ('insert','update'):
        db.execute('DROP TRIGGER IF EXISTS task_queue_projection_'+action)
        db.execute('CREATE TRIGGER IF NOT EXISTS task_queue_projection_'+action+' AFTER '+action.upper()+
                   ' ON tasks BEGIN INSERT INTO task_queue_overview VALUES('+values+') ON CONFLICT(id) DO UPDATE SET '+
                   'state=excluded.state,command=excluded.command,priority=excluded.priority,updated_at=excluded.updated_at,summary=excluded.summary; END')
    db.execute('CREATE TRIGGER IF NOT EXISTS task_queue_projection_delete AFTER DELETE ON tasks BEGIN DELETE FROM task_queue_overview WHERE id=OLD.id; END')
    if not exists:db.execute('INSERT OR REPLACE INTO task_queue_overview SELECT '+values.replace('NEW.','')+' FROM tasks')
    db.execute('CREATE INDEX IF NOT EXISTS task_queue_ready ON task_queue_overview(state,priority,updated_at,id)')
    db.execute('CREATE INDEX IF NOT EXISTS task_queue_recent ON task_queue_overview(updated_at DESC,id)')
