import importlib.util
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('maintenance',Path(__file__).with_name('history-maintenance.py'))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class HistoryMaintenanceTest(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.home=Path(self.tmp.name);(self.home/'sqlite').mkdir();(self.home/'archived_sessions').mkdir();(self.home/'sessions').mkdir()
        (self.home/"config.toml").write_text('sqlite_home = "sqlite"\n')
        self.now=time.time();self.old=self.now-40*86400
        self.db=sqlite3.connect(self.home/'sqlite/state_5.sqlite');self.addCleanup(self.db.close)
        self.db.execute('create table threads(id text primary key,rollout_path text,archived integer,archived_at integer,updated_at integer,recency_at integer)')
        self.policy=m.enable(self.home,self.old)
    def fixture(self,tid='thr-a',archived=1):
        p=self.home/('archived_sessions' if archived else 'sessions')/('rollout-'+tid+'.jsonl')
        data=(json.dumps({'type':'session_meta','payload':{'id':tid}})+'\n'+json.dumps({'type':'event_msg','payload':{'message':'long history '*10000}})+'\n').encode()
        p.write_bytes(data);os.utime(p,(self.old,self.old))
        self.db.execute('insert into threads values(?,?,?,?,?,?)',(tid,str(p),archived,self.old,self.old,self.old));self.db.commit()
        return p,data
    def test_roundtrip_and_no_plain_duplicate(self):
        p,data=self.fixture();report=m.run(self.home,True,self.now)
        self.assertEqual(report['compressed'],1);self.assertFalse(p.exists())
        self.assertEqual(subprocess.check_output(['zstd','-q','-d','-c',str(p)+'.zst']),data)
    def test_dry_run_keeps_source(self):
        p,_=self.fixture();report=m.run(self.home,False,self.now);self.assertEqual(report['eligible'],1);self.assertTrue(p.exists())
    def test_recent_access_unarchived_and_observation_floor(self):
        a,_=self.fixture();b,_=self.fixture('thr-b',0)
        access=self.home/'codex-provider-switcher/history-access';access.mkdir();(access/'thr-a').touch()
        self.assertEqual(m.run(self.home,True,self.now)['compressed'],0)
        (access/'thr-a').unlink();m.write_json(self.policy,{'enabled_at':self.now,'days':30})
        self.assertEqual(m.run(self.home,True,self.now)['compressed'],0)
        self.assertTrue(a.exists());self.assertTrue(b.exists())
    def test_busy_writer_and_new_access_during_compression(self):
        p,_=self.fixture()
        with m.writer_lock(self.home,'thr-a'):
            self.assertEqual(m.run(self.home,True,self.now)['compressed'],0)
        original=m.encode
        def access_during_compression(src,dest):
            original(src,dest);access=self.home/'codex-provider-switcher/history-access';access.mkdir(exist_ok=True);(access/'thr-a').touch()
        with patch.object(m,'encode',access_during_compression):
            self.assertEqual(m.run(self.home,True,self.now)['compressed'],0)
        self.assertTrue(p.exists());self.assertFalse(Path(str(p)+'.zst').exists())
    def test_corrupt_output_never_replaces_history(self):
        p,data=self.fixture()
        with patch.object(m,'encode',lambda src,dest:dest.write_bytes(b'broken')):
            result=m.run(self.home,True,self.now);self.assertEqual(result['failed'],1)
        self.assertEqual(p.read_bytes(),data)
    def test_symlink_is_never_compressed(self):
        p,_=self.fixture();real=p.with_suffix('.real');p.rename(real);p.symlink_to(real)
        self.assertEqual(m.run(self.home,True,self.now)['compressed'],0);self.assertTrue(real.exists())

    def test_invalid_header_is_counted_and_other_archives_continue(self):
        p,_=self.fixture();p.write_bytes(b'null\n'+b' '*70000);os.utime(p,(self.old,self.old))
        self.fixture('thr-b');report=m.run(self.home,True,self.now)
        self.assertEqual(report['failed'],1);self.assertEqual(report['compressed'],1);self.assertTrue(p.exists())
    def test_read_only_file_access_and_new_archive_are_not_cold(self):
        p,_=self.fixture();p.read_bytes()
        self.assertEqual(m.run(self.home,True,self.now)['compressed'],0)
        os.utime(p,(self.old,self.old));self.db.execute('update threads set archived_at=?',(self.now,));self.db.commit()
        self.assertEqual(m.run(self.home,True,self.now)['compressed'],0)
    def test_archive_state_change_during_compression_preserves_original(self):
        p,_=self.fixture();original=m.encode
        def unarchive(src,dest):
            original(src,dest);self.db.execute('update threads set archived=0');self.db.commit()
        with patch.object(m,'encode',unarchive):self.assertEqual(m.run(self.home,True,self.now)['compressed'],0)
        self.assertTrue(p.exists());self.assertFalse(Path(str(p)+'.zst').exists())
    def test_stalled_decoder_timeout_covers_stream_read(self):
        import sys
        p,_=self.fixture();original=subprocess.Popen
        def stalled(*args,**kwargs):return original([sys.executable,'-c','import time; time.sleep(10)'],**kwargs)
        start=time.monotonic()
        with patch.object(m,'DECODE_TIMEOUT',.05),patch.object(m.subprocess,'Popen',stalled):
            with self.assertRaises(ValueError):m.verify(p,p)
        self.assertLess(time.monotonic()-start,2)
    def test_default_database_directory_is_codex_home(self):
        (self.home/'config.toml').unlink();p,_=self.fixture();self.db.close()
        (self.home/'sqlite/state_5.sqlite').rename(self.home/'state_5.sqlite')
        self.assertEqual(m.run(self.home,True,self.now)['compressed'],1)

if __name__=='__main__':unittest.main()
