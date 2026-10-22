"""Small, transactionally maintained task summaries for large private profiles."""
FIELDS = {
    'job_id': "json_extract(NEW.payload,'$.job_id')",
    'root_id': "coalesce(nullif(json_extract(NEW.payload,'$.identification_job_id'),''),json_extract(NEW.payload,'$.job_id'))",
    **{key: f"json_extract(NEW.payload,'$.{key}')" for key in
       ('stage','scene','task_type','review_kind','identification_job_id','search_job_id','batch_id')},
    'label': "coalesce(json_extract(NEW.payload,'$.label'),'')",
    'job_kind': "coalesce(json_extract(NEW.payload,'$.job_kind'),'album')",
    'hidden': "coalesce(json_extract(NEW.payload,'$.hidden'),0)",
    'paused': "coalesce(json_extract(NEW.payload,'$.paused'),0)",
    'next_poll': "coalesce(json_extract(NEW.payload,'$.next_poll'),0)",
    'updated_at': 'NEW.updated_at',
    'created_at': "coalesce(json_extract(NEW.payload,'$.created_at'),NEW.updated_at)",
}


def initialize(db):
    exists = db.execute("SELECT 1 FROM sqlite_master WHERE name='workflow_overview'").fetchone()
    if exists and {row[1] for row in db.execute('PRAGMA table_info(workflow_overview)')}!={'id',*FIELDS}:
        for name in ('insert','update','delete'):db.execute('DROP TRIGGER IF EXISTS workflow_projection_'+name)
        db.execute('DROP TABLE workflow_overview');exists=False
    db.execute('CREATE TABLE IF NOT EXISTS workflow_overview (id TEXT PRIMARY KEY,'+
               ','.join(key+' '+('INTEGER' if key in ('hidden','paused','next_poll') else 'TEXT') for key in FIELDS)+')')
    columns = 'id,'+','.join(FIELDS)
    values = 'NEW.id,'+','.join(FIELDS.values())
    for action in ('INSERT','UPDATE'):
        db.execute(f"CREATE TRIGGER IF NOT EXISTS workflow_projection_{action.lower()} AFTER {action} ON evidence "
                   "WHEN NEW.id LIKE 'workflow:%' BEGIN INSERT INTO workflow_overview ("+
                   columns+') VALUES ('+values+') ON CONFLICT(id) DO UPDATE SET '+
                   ','.join(key+'=excluded.'+key for key in FIELDS)+'; END')
    db.execute("CREATE TRIGGER IF NOT EXISTS workflow_projection_delete AFTER DELETE ON evidence "
               "BEGIN DELETE FROM workflow_overview WHERE id=OLD.id; END")
    if not exists:
        db.execute('INSERT OR REPLACE INTO workflow_overview ('+columns+') SELECT '+
                   values.replace('NEW.','')+" FROM evidence WHERE id LIKE 'workflow:%'")
    db.execute('CREATE INDEX IF NOT EXISTS overview_state ON workflow_overview(hidden,stage,job_kind,paused)')
    db.execute('CREATE INDEX IF NOT EXISTS overview_recent ON workflow_overview(created_at DESC,id)')
    db.execute('CREATE INDEX IF NOT EXISTS overview_batch ON workflow_overview(batch_id,job_kind,stage)')
