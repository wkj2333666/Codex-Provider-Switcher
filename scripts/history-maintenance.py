#!/usr/bin/env python3
"""Lossless, archive-only Codex history maintenance (Python 3.11+, zstd)."""
import argparse
from contextlib import contextmanager
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import stat
import subprocess
import tempfile
import time
import threading
import tomllib

DAY=86400
DECODE_TIMEOUT=180


def write_json(path,value):
    path.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
    fd,name=tempfile.mkstemp(prefix='.maintenance-',dir=path.parent)
    try:
        with os.fdopen(fd,'w') as f:
            json.dump(value,f,indent=2);f.flush();os.fsync(f.fileno())
        os.replace(name,path)
    finally:
        if os.path.exists(name):os.unlink(name)


def enable(home,now=None):
    path=home/'codex-provider-switcher/history-maintenance.json'
    if not path.exists():write_json(path,{'enabled_at':time.time() if now is None else now,'days':30})
    return path


@contextmanager
def lock_file(path,blocking=False):
    path.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
    fd=os.open(path,os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW|os.O_NONBLOCK,0o600)
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):raise ValueError('invalid lock file')
        fcntl.flock(fd,fcntl.LOCK_EX|(0 if blocking else fcntl.LOCK_NB));yield fd
    finally:os.close(fd)


@contextmanager
def writer_lock(home,tid):
    directory=home/'thread-writer-locks';path=directory/(tid+'.lock')
    # Match native Codex's unlink-on-unlock protocol. Never replace a live inode.
    with lock_file(directory/'.coordination.lock'):
        guard=lock_file(path);fd=guard.__enter__()
    try:yield fd
    finally:
        with lock_file(directory/'.coordination.lock',True):
            guard.__exit__(None,None,None);path.unlink(missing_ok=True)


def encode(source,destination):
    with destination.open('wb') as out:
        subprocess.run(['zstd','-q','-T1','-3','--long=27','-c','--',str(source)],stdout=out,stderr=subprocess.DEVNULL,check=True,timeout=180)
        out.flush();os.fsync(out.fileno())


def digest(stream):
    result=hashlib.sha256()
    while block:=stream.read(1024*1024):result.update(block)
    return result.digest()


def verify(source,compressed):
    with source.open('rb') as inp:expected=digest(inp)
    process=subprocess.Popen(['zstd','-q','-d','-c','--',str(compressed)],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
    timer=threading.Timer(DECODE_TIMEOUT,process.kill);timer.daemon=True;timer.start()
    try:
        actual=digest(process.stdout)
        if process.wait(timeout=180)!=0 or actual!=expected:raise ValueError('compression integrity check failed')
    finally:
        timer.cancel()
        process.stdout.close()
        if process.poll() is None:process.kill();process.wait()


def source_state(path):
    s=path.lstat()
    if not stat.S_ISREG(s.st_mode):raise ValueError('not a regular history')
    return (s.st_dev,s.st_ino,s.st_size,s.st_mtime_ns,s.st_ctime_ns)


def access_time(home,tid):
    try:return (home/'codex-provider-switcher/history-access'/tid).lstat().st_mtime
    except FileNotFoundError:return 0


def archive_path(home,row):
    tid,path,archived,archived_at,updated,recency=row
    if not re.fullmatch('[A-Za-z0-9_-]{1,256}',tid) or not archived or not archived_at:return None
    p=Path(path);root=home/'archived_sessions'
    if not p.is_absolute() or p.suffix!='.jsonl' or not p.name.startswith('rollout-'):return None
    try:
        p.relative_to(root)
        if p.resolve()!=p or root.resolve()!=root:return None
        source_state(p)
    except (ValueError,OSError):return None
    return p


def eligible(home,row,cutoff,check_atime=True):
    p=archive_path(home,row)
    if p is None:return None
    s=p.stat()
    last=max(row[3] or 0,row[4] or 0,row[5] or 0,s.st_mtime,access_time(home,row[0]))
    if check_atime:last=max(last,s.st_atime)
    if last>cutoff or s.st_size<64*1024 or Path(str(p)+'.zst').exists():return None
    return p


def sync_dir(path):
    fd=os.open(path,os.O_RDONLY)
    try:os.fsync(fd)
    finally:os.close(fd)


def compress_one(home,db,row,cutoff):
    tid=row[0]
    with writer_lock(home,tid):
        p=eligible(home,row,cutoff)
        if p is None:return 0
        before=source_state(p);used=access_time(home,tid)
        # Check the metadata header without changing any records or byte offsets.
        with p.open('rb') as inp:header=json.loads(inp.readline(1024*1024))
        if not isinstance(header,dict) or header.get('type')!='session_meta' or not isinstance(header.get('payload'),dict) or header['payload'].get('id')!=tid:raise ValueError('history identity mismatch')
        fd,name=tempfile.mkstemp(prefix='.rollout-cold-',suffix='.tmp',dir=p.parent);os.close(fd);temp=Path(name)
        try:
            encode(p,temp);verify(p,temp)
            current=db.execute(QUERY+' where id=?',(tid,)).fetchone()
            if current!=row or eligible(home,current,cutoff,False) is None or access_time(home,tid)!=used or source_state(p)!=before:return 0
            if temp.stat().st_size>=before[2]:return 0
            os.chmod(temp,stat.S_IMODE(p.stat().st_mode));os.utime(temp,ns=(p.stat().st_atime_ns,p.stat().st_mtime_ns))
            with temp.open('rb') as f:os.fsync(f.fileno())
            compressed=Path(str(p)+'.zst');os.link(temp,compressed);sync_dir(p.parent)
            # Recheck after publication as reads may have started while encoding.
            if source_state(p)!=before or access_time(home,tid)!=used or db.execute(QUERY+' where id=?',(tid,)).fetchone()!=row:
                compressed.unlink();sync_dir(p.parent);return 0
            saved=before[2]-temp.stat().st_size;p.unlink();sync_dir(p.parent)
            return saved
        finally:temp.unlink(missing_ok=True)


QUERY='select id,rollout_path,archived,archived_at,updated_at,recency_at from threads'


def run(home,apply=False,now=None):
    home=home.absolute();now=time.time() if now is None else now;started=time.monotonic()
    report={'eligible':0,'compressed':0,'failed':0,'saved_bytes':0,'skipped':0,'apply':apply}
    policy=home/'codex-provider-switcher/history-maintenance.json'
    if not policy.exists():return dict(report,status='disabled')
    config=json.loads(policy.read_text());days=max(30,int(config['days']));cutoff=now-days*DAY
    if float(config['enabled_at'])>cutoff:
        report.update(status='observing',eligible_after=float(config['enabled_at'])+days*DAY)
        if apply:write_json(home/'codex-provider-switcher/history-maintenance-last-run.json',dict(report,finished_at=now))
        return report
    sqlite_home=home
    if (home/'config.toml').exists():
        with (home/'config.toml').open('rb') as f:settings=tomllib.load(f)
        if settings.get('sqlite_home'):sqlite_home=Path(settings['sqlite_home']).expanduser()
    if not sqlite_home.is_absolute():sqlite_home=home/sqlite_home
    try:
        with lock_file(home/'.tmp/rollout-maintenance.lock'):
            db=sqlite3.connect((sqlite_home/'state_5.sqlite').as_uri()+'?mode=ro',uri=True,timeout=1)
            try:
                total=0
                for row in db.execute(QUERY+' where archived=1').fetchall():
                    if time.monotonic()-started>300 or report['compressed']>=5:break
                    p=eligible(home,row,cutoff)
                    if p is None:report['skipped']+=1;continue
                    size=p.stat().st_size
                    if total+size>2*1024**3:report['skipped']+=1;continue
                    report['eligible']+=1;total+=size
                    if not apply:continue
                    try:
                        saved=compress_one(home,db,row,cutoff)
                        if saved:report['compressed']+=1;report['saved_bytes']+=saved
                        else:report['skipped']+=1
                    except BlockingIOError:report['skipped']+=1
                    except (OSError,ValueError,subprocess.SubprocessError):report['failed']+=1
            finally:db.close()
    except BlockingIOError:report['status']='maintenance_busy'
    if apply:write_json(home/'codex-provider-switcher/history-maintenance-last-run.json',dict(report,finished_at=time.time()))
    return report


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--home',type=Path,default=Path(os.environ.get('CODEX_HOME',Path.home()/'.codex')))
    parser.add_argument('--enable',action='store_true',help='Start a conservative 30-day observation period')
    parser.add_argument('--apply',action='store_true',help='Compress eligible archives (default: report only)')
    args=parser.parse_args()
    if args.enable:enable(args.home)
    report=run(args.home,args.apply);print(json.dumps(report))
    return 1 if report['failed'] else 0


if __name__=='__main__':raise SystemExit(main())
