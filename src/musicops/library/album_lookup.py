"""Names locate existing album candidates; they never prove an edition."""
import json,re
from musicops.media.chinese import alias_key

def key(album,artists):
    def folded(value):return re.sub(r'[\W_]+','',alias_key(value))
    return json.dumps([folded(album),sorted({folded(a) for a in artists if a})],ensure_ascii=False,separators=(',',':'))

def migrate(connection):
    # Compact album summaries only, never music files or the full evidence table.
    rows=connection.execute("SELECT root_key,album_id,payload FROM library_albums WHERE json_extract(payload,'$.lookup_key') IS NULL").fetchall()
    updates=[]
    for root,ident,raw in rows:
        row=json.loads(raw);row['lookup_key']=key(row.get('album',''),row.get('artists',[]))
        updates.append((json.dumps(row,ensure_ascii=False),root,ident))
    connection.executemany('UPDATE library_albums SET payload=? WHERE root_key=? AND album_id=?',updates)

def candidates(store,root,album,artists):
    from musicops.library.catalog import root_key
    with store.connection() as connection:
        rows=connection.execute("SELECT payload FROM library_albums WHERE root_key=? AND json_extract(payload,'$.lookup_key')=?",(root_key(root),key(album,artists))).fetchall()
    return [json.loads(r[0]) for r in rows]
