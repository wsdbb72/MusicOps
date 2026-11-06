"""Small, transactionally maintained task summaries for large private profiles."""
PUBLIC_FIELDS = ('job_id','job_kind','task_type','task_group','scene','stage','label','progress','message','detail',
    'review_kind','batch_id','scanned_count','cached_count','current_directory','album_count','selected_target_count',
    'target_count','matched_count','identification_job_id','search_job_id','processing_job_id','progress_phase',
    'retry_job_id','retry_count','ai_review_state','paused','start_task_id','resume_stage','error_class','stage_started_at')

def summary_expression():
    # json_each parses the large payload once. Repeating json_extract for
    # every UI field multiplied migration/write cost for multi-MB manifests.
    cases=["WHEN field.key='message' THEN substr(field.value,1,4000)",
           "WHEN field.key='detail' THEN substr(field.value,1,8000)"]
    for field,keys in (('ai_assistance',('status','kind','message')),('ai_fallback',('status','calls','reason'))):
        value='json_object('+','.join("'"+key+"',json_extract(field.value,'$."+key+"')" for key in keys)+')'
        cases.append(f"WHEN field.key='{field}' THEN CASE WHEN field.type='object' THEN {value} ELSE NULL END")
    cases.append("WHEN field.type IN ('array','object') THEN json(field.value)")
    selected=','.join("'"+field+"'" for field in (*PUBLIC_FIELDS,'ai_assistance','ai_fallback'))
    return '(SELECT json_group_object(field.key,CASE '+ ' '.join(cases)+' ELSE field.value END) FROM json_each(NEW.payload) AS field WHERE field.key IN ('+selected+'))'

FIELDS = {
    'job_id': "json_extract(NEW.payload,'$.job_id')",
    'root_id': "coalesce(nullif(json_extract(NEW.payload,'$.identification_job_id'),''),json_extract(NEW.payload,'$.job_id'))",
    **{key: f"json_extract(NEW.payload,'$.{key}')" for key in
       ('stage','scene','task_type','review_kind','identification_job_id','search_job_id','batch_id','error_class')},
    'label': "coalesce(json_extract(NEW.payload,'$.label'),'')",
    'job_kind': "coalesce(json_extract(NEW.payload,'$.job_kind'),'album')",
    'task_priority': "CASE json_extract(NEW.payload,'$.task_type') WHEN 'album' THEN 2 WHEN 'search' THEN 1 ELSE 0 END",
    'hidden': "coalesce(json_extract(NEW.payload,'$.hidden'),0)",
    'paused': "coalesce(json_extract(NEW.payload,'$.paused'),0)",
    'next_poll': "coalesce(json_extract(NEW.payload,'$.next_poll'),0)",
    'updated_at': 'NEW.updated_at',
    'created_at': "coalesce(json_extract(NEW.payload,'$.created_at'),NEW.updated_at)",
    'summary': summary_expression(),
}


def initialize(db):
    exists = db.execute("SELECT 1 FROM sqlite_master WHERE name='workflow_overview'").fetchone()
    columns_before={row[1] for row in db.execute('PRAGMA table_info(workflow_overview)')} if exists else set()
    upgrade=bool(exists and not {'id',*FIELDS}.issubset(columns_before))
    if upgrade:
        for name in ('insert','update','delete'):db.execute('DROP TRIGGER IF EXISTS workflow_projection_'+name)
        for key in FIELDS:
            if key not in columns_before:db.execute('ALTER TABLE workflow_overview ADD COLUMN '+key+' '+('INTEGER' if key in ('hidden','paused','next_poll','task_priority') else 'TEXT'))
    db.execute('CREATE TABLE IF NOT EXISTS workflow_overview (id TEXT PRIMARY KEY,'+
               ','.join(key+' '+('INTEGER' if key in ('hidden','paused','next_poll','task_priority') else 'TEXT') for key in FIELDS)+')')
    columns = 'id,'+','.join(FIELDS)
    values = 'NEW.id,'+','.join(FIELDS.values())
    for action in ('INSERT','UPDATE'):
        db.execute(f"CREATE TRIGGER IF NOT EXISTS workflow_projection_{action.lower()} AFTER {action} ON evidence "
                   "WHEN NEW.id LIKE 'workflow:%' BEGIN INSERT INTO workflow_overview ("+
                   columns+') VALUES ('+values+') ON CONFLICT(id) DO UPDATE SET '+
                   ','.join(key+'=excluded.'+key for key in FIELDS)+'; END')
    db.execute("CREATE TRIGGER IF NOT EXISTS workflow_projection_delete AFTER DELETE ON evidence "
               "BEGIN DELETE FROM workflow_overview WHERE id=OLD.id; END")
    if not exists or upgrade:
        db.execute('INSERT OR REPLACE INTO workflow_overview ('+columns+') SELECT '+
                   values.replace('NEW.','')+" FROM evidence WHERE id LIKE 'workflow:%'")
    db.execute('CREATE INDEX IF NOT EXISTS overview_state ON workflow_overview(hidden,stage,job_kind,paused)')
    db.execute('CREATE INDEX IF NOT EXISTS overview_recent ON workflow_overview(created_at DESC,id)')
    db.execute('CREATE INDEX IF NOT EXISTS overview_batch ON workflow_overview(batch_id,job_kind,stage)')
    db.execute('CREATE INDEX IF NOT EXISTS overview_error ON workflow_overview(stage,error_class)')
    db.execute('CREATE INDEX IF NOT EXISTS overview_job ON workflow_overview(job_id)')
    db.execute('CREATE INDEX IF NOT EXISTS overview_root ON workflow_overview(root_id,task_type,created_at DESC,id)')
    db.execute('CREATE INDEX IF NOT EXISTS overview_root_priority ON workflow_overview(root_id,task_priority DESC,created_at DESC,id)')
    return not exists or upgrade
