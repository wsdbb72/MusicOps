from __future__ import annotations
import contextlib, datetime as dt, json, math, sqlite3, uuid
from pathlib import Path

TABLES = ('config_versions','evidence','playlist_snapshots','target_tracks','library_files','match_decisions','candidates','download_tasks','media_validations','resource_candidates','change_plans','transactions','transaction_files','field_locks','suppressions','scheduler_runs','tasks','ipc_requests')
def utcnow(): return dt.datetime.now(dt.timezone.utc).isoformat()
def parse_time(value):
    if isinstance(value, (int,float)): return dt.datetime.fromtimestamp(value,dt.timezone.utc)
    return dt.datetime.fromisoformat(str(value).replace('Z','+00:00'))

class StateStore:
    def __init__(self,path):
        self.path=Path(path); self.path.parent.mkdir(parents=True,exist_ok=True)
        self.run_id=str(uuid.uuid4())
        with self.connection() as c:
            c.execute('PRAGMA journal_mode=WAL')
            if c.execute('PRAGMA integrity_check').fetchone()[0]!='ok': raise RuntimeError('STATE_CORRUPT')
            version=c.execute('PRAGMA user_version').fetchone()[0]
            if version>1: raise RuntimeError('SCHEMA_TOO_NEW')
            for table in TABLES:
                c.execute(f'CREATE TABLE IF NOT EXISTS {table} (id TEXT PRIMARY KEY, payload TEXT NOT NULL, updated_at TEXT NOT NULL)')
            c.execute('CREATE TABLE IF NOT EXISTS events (seq INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, phase TEXT NOT NULL, task_id TEXT, tx_id TEXT, payload TEXT NOT NULL)')
            c.execute('CREATE TABLE IF NOT EXISTS token_reservations (id TEXT PRIMARY KEY, site TEXT NOT NULL, resource_id TEXT NOT NULL, cost INTEGER NOT NULL, state TEXT NOT NULL, payload TEXT NOT NULL, updated_at TEXT NOT NULL)')
            # Completed spends remain in the ledger/batch budget, but a later
            # authenticated FL renewal may create another active reservation.
            c.execute("CREATE UNIQUE INDEX IF NOT EXISTS token_active_v2 ON token_reservations(site,resource_id) WHERE state NOT IN ('RELEASED','REJECTED','CONFIRMED','CONSUMED')")
            c.execute('DROP INDEX IF EXISTS token_active')
            c.execute("CREATE INDEX IF NOT EXISTS task_state_queue ON tasks(json_extract(payload,'$.state'),updated_at,id)")
            c.execute("CREATE INDEX IF NOT EXISTS workflow_summary ON evidence(coalesce(json_extract(payload,'$.hidden'),0),json_extract(payload,'$.stage'),coalesce(json_extract(payload,'$.job_kind'),'album'),coalesce(json_extract(payload,'$.paused'),0)) WHERE id LIKE 'workflow:%'")
            c.execute("CREATE INDEX IF NOT EXISTS workflow_page ON evidence(coalesce(json_extract(payload,'$.hidden'),0),coalesce(json_extract(payload,'$.created_at'),updated_at) DESC,id) WHERE id LIKE 'workflow:%'")
            c.execute("CREATE INDEX IF NOT EXISTS ai_review_job ON evidence(json_extract(payload,'$.job_id')) WHERE id LIKE 'ai-review:%'")
            c.execute("CREATE INDEX IF NOT EXISTS ai_review_pending ON evidence(json_extract(payload,'$.state'),updated_at,id) WHERE id LIKE 'ai-review:%'")
            c.execute('CREATE TABLE IF NOT EXISTS library_matches (root_key TEXT NOT NULL,file_id TEXT NOT NULL,title_key TEXT NOT NULL,artist_key TEXT NOT NULL,album_key TEXT NOT NULL,duration REAL NOT NULL,album_id TEXT NOT NULL,PRIMARY KEY(root_key,file_id))')
            c.execute('CREATE INDEX IF NOT EXISTS library_match_identity ON library_matches(root_key,title_key,artist_key,album_key)')
            c.execute('CREATE TABLE IF NOT EXISTS library_albums (root_key TEXT NOT NULL,album_id TEXT NOT NULL,payload TEXT NOT NULL,PRIMARY KEY(root_key,album_id))')
            c.execute('PRAGMA user_version=1')
    @contextlib.contextmanager
    def connection(self):
        c=sqlite3.connect(self.path,timeout=15)
        try:
            c.row_factory=sqlite3.Row
            c.execute('PRAGMA busy_timeout=15000'); c.execute('PRAGMA synchronous=FULL')
            yield c; c.commit()
        except BaseException: c.rollback(); raise
        finally: c.close()
    def put(self,kind,id,data):
        if kind not in TABLES: raise ValueError('INVALID_TABLE')
        value=dict(data); value.setdefault('schema_version',1); value.setdefault('id',id)
        with self.connection() as c:
            c.execute(f'INSERT INTO {kind} VALUES (?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at',(id,json.dumps(value,ensure_ascii=False),utcnow()))
        return value
    def get(self,kind,id):
        if kind not in TABLES and kind!='token_reservations': raise ValueError('INVALID_TABLE')
        with self.connection() as c: row=c.execute(f'SELECT payload FROM {kind} WHERE id=?',(id,)).fetchone()
        return json.loads(row[0]) if row else None
    def list(self,kind):
        if kind not in TABLES and kind!='token_reservations': raise ValueError('INVALID_TABLE')
        with self.connection() as c: rows=c.execute(f'SELECT payload FROM {kind} ORDER BY updated_at,id').fetchall()
        return [json.loads(r[0]) for r in rows]
    def delete_lock(self,id):
        with self.connection() as c: c.execute('DELETE FROM field_locks WHERE id=?',(id,))
        self.event('field_unlocked',{'id':id})
    def event(self,phase,data,task_id=None,tx_id=None):
        from musicops.clients.http import redact
        data=dict(data);data.setdefault('run_id',self.run_id);data.setdefault('requirements',['02§17','用户：每一步可追溯'])
        data.setdefault('action',phase);data.setdefault('artifact','state/musicops.sqlite'+(':'+task_id if task_id else ':'+tx_id if tx_id else ':events'))
        with self.connection() as c:
            cur=c.execute('INSERT INTO events(at,phase,task_id,tx_id,payload) VALUES(?,?,?,?,?)',(utcnow(),phase,task_id,tx_id,json.dumps(redact(data),ensure_ascii=False)))
            return cur.lastrowid
    def events(self,after=0):
        with self.connection() as c: rows=c.execute('SELECT * FROM events WHERE seq>? ORDER BY seq LIMIT 1000',(after,)).fetchall()
        return [{**dict(r),'payload':json.loads(r['payload'])} for r in rows]
    def reserve_token(self,site,resource_id,cost,balance,budget,observed_at,ttl=300,budget_ref='default',renew_confirmed=False):
        now=dt.datetime.now(dt.timezone.utc)
        if any(type(value) is not int for value in (cost,balance,budget)) or cost<=0 or balance<cost or budget<cost:
            raise RuntimeError('POLICY_BLOCKED:余额或预算不足/未知')
        if not isinstance(site,str) or not site.strip() or not str(resource_id or '').strip() or not isinstance(budget_ref,str) or not budget_ref.strip():
            raise RuntimeError('POLICY_BLOCKED:预算预留身份无效')
        try:
            observed=parse_time(observed_at)
            valid_time=observed.tzinfo is not None and type(ttl) in (int,float) and math.isfinite(ttl) and ttl>0 and 0<=(now-observed).total_seconds()<=ttl
        except (TypeError,ValueError,OverflowError,OSError):
            valid_time=False
        if not valid_time: raise RuntimeError('POLICY_BLOCKED:余额时间无效或过期')
        with self.connection() as c:
            c.execute('BEGIN IMMEDIATE')
            if c.execute("SELECT 1 FROM token_reservations WHERE site=? AND state IN ('UNKNOWN','REQUEST_UNKNOWN','REQUESTING')",(site,)).fetchone(): raise RuntimeError('POLICY_BLOCKED:站点请求待对账')
            states="('RELEASED','REJECTED','CONFIRMED')" if renew_confirmed is True else "('RELEASED','REJECTED')"
            if c.execute(f"SELECT 1 FROM token_reservations WHERE site=? AND resource_id=? AND state NOT IN {states}",(site,str(resource_id))).fetchone(): raise RuntimeError('ALREADY_RESERVED')
            rows=c.execute("SELECT cost,state,updated_at,payload FROM token_reservations WHERE site=? AND state NOT IN ('RELEASED','REJECTED')",(site,)).fetchall()
            outstanding=sum(r['cost'] for r in rows if r['state'] not in ('CONSUMED','CONFIRMED') or parse_time(r['updated_at'])>=observed)
            used=sum(r['cost'] for r in rows if json.loads(r['payload']).get('budget_ref','default')==budget_ref)
            if outstanding+cost>balance or used+cost>budget: raise RuntimeError('POLICY_BLOCKED:已预留预算不足')
            rid=str(uuid.uuid4()); value=dict(schema_version=1,id=rid,site=site,resource_id=str(resource_id),cost=cost,state='RESERVED',observed_at=observed.isoformat(),budget_ref=budget_ref,renew_confirmed=renew_confirmed is True)
            c.execute('INSERT INTO token_reservations VALUES(?,?,?,?,?,?,?)',(rid,site,str(resource_id),cost,'RESERVED',json.dumps(value),utcnow()))
        return value
    def token_result(self,id,state,evidence=None):
        if state not in ('RESERVED','REQUESTING','UNKNOWN','REQUEST_UNKNOWN','CONSUMED','CONFIRMED','RELEASED','REJECTED'): raise ValueError('INVALID_TOKEN_STATE')
        with self.connection() as c:
            c.execute('BEGIN IMMEDIATE'); row=c.execute('SELECT payload FROM token_reservations WHERE id=?',(id,)).fetchone()
            if not row: raise KeyError(id)
            value=json.loads(row[0]); previous=value['state']
            transitions={
                'RESERVED': {'REQUESTING','UNKNOWN','REQUEST_UNKNOWN','CONSUMED','CONFIRMED','RELEASED','REJECTED'},
                'REQUESTING': {'UNKNOWN','REQUEST_UNKNOWN','CONSUMED','CONFIRMED','RELEASED','REJECTED'},
                'UNKNOWN': {'REQUEST_UNKNOWN','CONSUMED','CONFIRMED','RELEASED','REJECTED'},
                'REQUEST_UNKNOWN': {'UNKNOWN','CONSUMED','CONFIRMED','RELEASED','REJECTED'},
                'CONSUMED': {'CONFIRMED'}, 'CONFIRMED': set(), 'RELEASED': set(), 'REJECTED': set()}
            if state!=previous and state not in transitions[previous]:
                raise ValueError('INVALID_TOKEN_TRANSITION')
            if state in ('RELEASED','REJECTED') and previous in ('REQUESTING','UNKNOWN','REQUEST_UNKNOWN') and not (isinstance(evidence,dict) and evidence):
                raise ValueError('TOKEN_RECONCILIATION_REQUIRED')
            if state==previous and (evidence is None or evidence==value.get('evidence')):
                return value
            value.update(state=state,evidence=evidence)
            c.execute('UPDATE token_reservations SET state=?,payload=?,updated_at=? WHERE id=?',(state,json.dumps(value),utcnow(),id))
        self.event('token_'+state,{'reservation_id':id,'site':value['site'],'resource_id':value['resource_id'],'requirements':['T09','F12'],'result':state})
        return value
    def backup(self,destination):
        dest=Path(destination); dest.parent.mkdir(parents=True,exist_ok=True)
        with sqlite3.connect(self.path) as src,sqlite3.connect(dest) as out: src.backup(out)
        return str(dest)
