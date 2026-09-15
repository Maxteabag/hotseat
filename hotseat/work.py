"""Read stopped work and explicitly resume the same native conversation."""
from __future__ import annotations
import hashlib
import json
import os
from pathlib import Path
import signal
import sqlite3
import time
from . import actions,codexsessions,inspect as inspection,resume

class WorkError(RuntimeError): pass


def process_identity(pid,provider):
    """Exact native CLI identity plus kernel start time; no substring matching."""
    try:
        root=Path('/proc')/str(pid)
        if root.stat().st_uid != os.getuid():return None
        argv=[a.decode(errors='replace') for a in (root/'cmdline').read_bytes().split(b'\0') if a]
        if not argv:return None
        names=[Path(argv[0]).name]
        if names[0] in ('node','nodejs') and len(argv)>1:names.append(Path(argv[1]).name)
        if provider not in names and not (provider=='claude' and any(n in ('claude.js','cli.js') for n in names) and any('@anthropic-ai/claude-code' in a for a in argv[:2])):return None
        fields=(root/'stat').read_text().rsplit(')',1)[1].split()
        return {'pid':pid,'start':fields[19],'argv':argv,'shared':'app-server' in argv or '--input-format' in argv and 'stream-json' in argv}
    except (OSError,ValueError,IndexError):return None


def process_running(owner):
    try:
        fields=Path(f"/proc/{owner['pid']}/stat").read_text().rsplit(')',1)[1].split()
        return fields[19]==owner['start'] and fields[0] not in ('Z','X')
    except FileNotFoundError:return False
    except (OSError,IndexError) as exc:raise WorkError("Cannot verify process exit") from exc


def claude_processes():
    found=[]
    for p in Path('/proc').iterdir() if Path('/proc').exists() else []:
        if not p.name.isdigit():continue
        item=process_identity(int(p.name),'claude')
        if not item:continue
        found.append(item)
    return found


def claude_holders(identifier,processes=None):
    return [item for item in (claude_processes() if processes is None else processes)
            if any(arg in ('--resume','-r','--session-id') and i+1<len(item['argv']) and item['argv'][i+1]==identifier for i,arg in enumerate(item['argv']))]


def codex_metadata(identifiers):
    if not identifiers:return {}
    paths=sorted(codexsessions.CODEX_HOME.glob('state_*.sqlite'),key=lambda p:p.stat().st_mtime,reverse=True)
    for path in paths:
        try:
            with sqlite3.connect(f'file:{path}?mode=ro',uri=True,timeout=2) as db:
                rows=db.execute('SELECT id,cwd,title FROM threads WHERE id IN ('+','.join('?' for _ in identifiers)+')',identifiers).fetchall()
                if rows:return {row[0]:{'cwd':row[1],'title':row[2]} for row in rows}
        except sqlite3.Error:continue
    return {}


def revision(item,holders):
    material=[item['id'],item['provider'],item['state'],item.get('updated'),item.get('cwd'),sorted((p['pid'],p['start']) for p in holders)]
    return hashlib.sha256(json.dumps(material).encode()).hexdigest()


def listing(limit=100):
    items=[];errors=[]
    try:
        entries=codexsessions.recent(limit=limit)
    except codexsessions.SessionError as exc:
        entries=[];errors.append(str(exc))
    directories=codex_metadata([e['thread_id'] for e in entries])
    for entry in entries:
        holders=[p for pid in entry['holders'] if (p:=process_identity(pid,'codex'))]
        shared=any(p['shared'] for p in holders)
        unknown=len(holders)!=len(entry['holders'])
        item={'id':entry['thread_id'],'provider':'codex','title':directories.get(entry['thread_id'],{}).get('title') or entry['topic'],'state':entry['state'],
              'updated':entry['last_at'],'cwd':directories.get(entry['thread_id'],{}).get('cwd',''),'live':entry['live'],
              'can_resume':not entry['live'],'can_reboot':entry['live'] and not shared and not unknown,
              'reason':'Managed by a shared app-server; use its host integration' if shared else ('Cannot identify the exact owning process' if unknown else '')}
        item['revision']=revision(item,holders);items.append(item)
    # Native Claude transcripts are local and include the original working directory.
    try:
        candidates=sorted(inspection.PROJECTS_DIR.glob('*/*.jsonl'),key=lambda p:p.stat().st_mtime,reverse=True)[:limit]
    except OSError:
        candidates=[]
    native_processes=claude_processes()
    for path in candidates:
        try:
            tail=[e for e in resume._last_entries(path) if isinstance(e,dict)]
            last=next((e for e in reversed(tail) if e.get('type') in ('assistant','user')),None)
            if not last:continue
            holders=claude_holders(path.stem,native_processes)
            failed=bool(last.get('isApiErrorMessage'))
            live=bool(holders)
            state='stuck' if failed and live else 'failed' if failed else 'running' if live else 'recorded'
            title=next((resume.entry_text(e) for e in reversed(tail) if e.get('type')=='user' and not resume.entry_text(e).startswith(('<','# AGENTS'))),path.stem)
            cwd=last.get('cwd') or next((e['cwd'] for e in reversed(tail) if e.get('cwd')), '')
            item={'id':path.stem,'provider':'claude','title':title[:160],'state':state,'updated':path.stat().st_mtime,
                  'cwd':cwd,'live':live,'can_resume':not live,'can_reboot':live and not any(p['shared'] for p in holders),
                  'reason':'Managed streaming process; use its host integration' if any(p['shared'] for p in holders) else '' if live else 'Reboot requires an exact --resume/--session-id process match'}
            item['revision']=revision(item,holders);items.append(item)
        except OSError:continue
    return {'items':sorted(items,key=lambda i:(i['state'] not in ('failed','stuck'),-i['updated'])),'errors':errors}


def resolve(provider,identifier):
    found=[i for i in listing()['items'] if i['provider']==provider and i['id']==identifier]
    if len(found)!=1:raise WorkError('Session is missing or ambiguous; refresh the work list')
    return found[0]


def detail(provider,identifier):
    item=resolve(provider,identifier)
    if provider=='claude':
        result=inspection._native_detail(identifier)
        if result:item.update(last_user=result.get('last_user',''),last_assistant=result.get('last_assistant',''),model=result.get('model') or '')
    else:
        try:
            with sqlite3.connect(f'file:{codexsessions.HISTORY_DB}?mode=ro',uri=True,timeout=5) as db:
                rows=db.execute('SELECT item_json FROM thread_items WHERE thread_id=? ORDER BY rollout_ordinal DESC LIMIT 40',(identifier,)).fetchall()
            messages=[]
            for row in reversed(rows):
                text=codexsessions._first_text(row[0])
                if text:messages.append(text)
            item['last_user']=item['title'];item['last_assistant']='\n\n'.join(messages[-6:])[-6000:]
        except sqlite3.Error as exc:raise WorkError(str(exc)) from exc
    return item


def act(provider,identifier,operation,expected_revision,acknowledged=False):
    if operation not in ('resume','reboot'):raise WorkError('Unsupported work action')
    if not acknowledged:raise WorkError('Confirmation is required')
    item=resolve(provider,identifier)
    if item['revision']!=expected_revision:raise WorkError('Session changed; refresh and confirm again')
    if not item['can_'+operation]:raise WorkError(item['reason'] or 'This action is not available for the current session state')
    cwd=item.get('cwd')
    if not cwd or not Path(cwd).is_dir():raise WorkError('Original working directory is missing; no session was changed')
    if operation=='reboot':
        holders=claude_holders(identifier) if provider=='claude' else [process_identity(pid,'codex') for pid in codexsessions.lock_holders(identifier)]
        if not holders or any(p is None or p['shared'] for p in holders):raise WorkError('Owner changed; refusing to stop a shared or unknown process')
        if revision(item,holders)!=expected_revision:raise WorkError('Owning process changed; confirm again')
        ancestors=set();pid=os.getpid()
        while pid>1 and pid not in ancestors:
            ancestors.add(pid)
            try:pid=int(next(line.split()[1] for line in Path(f'/proc/{pid}/status').read_text().splitlines() if line.startswith('PPid:')))
            except (OSError,StopIteration):break
        if any(p['pid'] in ancestors for p in holders):raise WorkError('Refusing to stop the controller process')
        for p in holders:
            current=process_identity(p['pid'],provider)
            if not current or current['start']!=p['start']:raise WorkError('Process identity changed before shutdown')
            os.kill(p['pid'],signal.SIGTERM)
        deadline=time.monotonic()+10
        while time.monotonic()<deadline:
            alive=any(process_running(p) for p in holders)
            locked=provider=='codex' and bool(codexsessions.lock_holders(identifier))
            if not alive and not locked:break
            time.sleep(.1)
        else:raise WorkError('Session did not exit; no replacement was launched')
        if provider=='codex' and codexsessions.lock_holders(identifier):raise WorkError('Conversation lock is still held; no replacement was launched')
    if operation=='resume':
        current_holders=codexsessions.lock_holders(identifier) if provider=='codex' else claude_holders(identifier)
        if current_holders:raise WorkError('Session acquired an owner; refresh before resuming')
    command=['codex','resume',identifier] if provider=='codex' else ['claude','--resume',identifier]
    result=actions.launch_command(command,cwd=cwd)
    return {'id':identifier,'provider':provider,'operation':operation,'launched':bool(result.get('started'))}
