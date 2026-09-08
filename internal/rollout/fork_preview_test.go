package rollout

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const previewChild = "11111111-1111-4111-8111-111111111111"

const previewFixture = `
import sqlite3,pathlib,json,sys
h=pathlib.Path(sys.argv[1]);(h/'sessions').mkdir();(h/'db').mkdir()
(h/'config.toml').write_text('sqlite_home = "db"\n')
c=sqlite3.connect(h/'db/state_5.sqlite')
c.execute("create table threads(id text primary key,preview text,archived integer,history_mode text,thread_source text,rollout_path text,source text default 'vscode')")
for i,base,preview in [('11111111-1111-4111-8111-111111111111','22222222-2222-4222-8222-222222222222',''),('22222222-2222-4222-8222-222222222222','33333333-3333-4333-8333-333333333333','真实继承的用户请求'),('33333333-3333-4333-8333-333333333333',None,'真实继承的用户请求')]:
 p=h/'sessions'/('rollout-'+i+'.jsonl')
 meta={'id':i,'history_mode':'paginated'}
 if base:meta.update(forked_from_id=base,history_base={'thread_id':base,'end_ordinal_exclusive':10,'end_byte_offset':100})
 p.write_text(json.dumps({'type':'session_meta','payload':meta})+'\n')
 c.execute('insert into threads(id,preview,archived,history_mode,thread_source,rollout_path) values(?,?,0,?,?,?)',(i,preview,'paginated','user',str(p)))
c.commit()
c=sqlite3.connect(h/'db/thread_history_1.sqlite')
c.execute('create table thread_items(thread_id text,rollout_ordinal integer,item_type text)')
c.execute('insert into thread_items values(?,5,?)',('33333333-3333-4333-8333-333333333333','userMessage'));c.commit()
`

func newPreviewFixture(t *testing.T) string {
	t.Helper()
	if err := exec.Command("python3", "-I", "-c", "import tomllib,sqlite3").Run(); err != nil {
		t.Skip("Python 3.11+ required for SQLite compatibility tests")
	}
	home := t.TempDir()
	previewPython(t, home, previewFixture)
	return home
}

func previewPython(t *testing.T, home, script string) []byte {
	t.Helper()
	out, err := exec.Command("python3", "-I", "-c", script, home).CombinedOutput()
	if err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	return out
}

func TestRepairForkPreviewPersistsInheritedPreviewWithoutEditingHistory(t *testing.T) {
	home := newPreviewFixture(t)
	path := filepath.Join(home, "sessions", "rollout-"+previewChild+".jsonl")
	before, _ := os.ReadFile(path)
	got, err := RepairForkPreview(context.Background(), home, previewChild)
	if err != nil || got != "真实继承的用户请求" {
		t.Fatalf("repair = %q, %v", got, err)
	}
	out := previewPython(t, home, `import sqlite3,pathlib,sys,json
c=sqlite3.connect(pathlib.Path(sys.argv[1])/'db/state_5.sqlite')
print(json.dumps(c.execute("select id from threads where preview<>''").fetchall()))`)
	var visible [][]string
	if err := json.Unmarshal(out, &visible); err != nil || len(visible) != 3 {
		t.Fatalf("fork remains invisible: %s (%v)", out, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("repair changed rollout bytes")
	}
	if again, err := RepairForkPreview(context.Background(), home, previewChild); err != nil || again != got {
		t.Fatalf("repeat repair = %q, %v", again, err)
	}
}

func TestForkPreviewLeavesUnrelatedOrEmptyThreadsAlone(t *testing.T) {
	for _, condition := range []string{"archived", "subagent", "existing", "no-user", "before-user", "no-base"} {
		t.Run(condition, func(t *testing.T) {
			home := newPreviewFixture(t)
			mutations := map[string]string{
				"archived":    "c.execute('update threads set archived=1 where id=?',(tid,))",
				"subagent":    "c.execute(\"update threads set thread_source='subagent' where id=?\",(tid,))",
				"existing":    "c.execute(\"update threads set preview='already set' where id=?\",(tid,))",
				"no-user":     "hdb=sqlite3.connect(h/'db/thread_history_1.sqlite');hdb.execute('delete from thread_items');hdb.commit()",
				"before-user": "hdb=sqlite3.connect(h/'db/thread_history_1.sqlite');hdb.execute('update thread_items set rollout_ordinal=10');hdb.commit()",
				"no-base":     "p=h/'sessions'/('rollout-'+tid+'.jsonl');m=json.loads(p.read_text());m['payload'].pop('history_base');p.write_text(json.dumps(m)+'\\n')",
			}
			previewPython(t, home, "import pathlib,sqlite3,json,sys\nh=pathlib.Path(sys.argv[1]);tid='"+previewChild+"';c=sqlite3.connect(h/'db/state_5.sqlite')\n"+mutations[condition]+"\nc.commit()")
			got, err := RepairForkPreview(context.Background(), home, previewChild)
			want := ""
			if condition == "existing" {
				want = "already set"
			}
			if err != nil || got != want {
				t.Fatalf("repair = %q, %v; want %q", got, err, want)
			}
		})
	}
}

func TestForkPreviewRejectsMissingOrUnsafeStorage(t *testing.T) {
	for _, condition := range []string{"missing-db", "symlink-rollout", "cycle", "wrong-id", "bad-config"} {
		t.Run(condition, func(t *testing.T) {
			home := newPreviewFixture(t)
			mutations := map[string]string{
				"missing-db":      "(h/'db/state_5.sqlite').unlink()",
				"symlink-rollout": "p=h/'sessions'/('rollout-'+tid+'.jsonl');q=h/'other';p.rename(q);p.symlink_to(q)",
				"cycle":           "p=h/'sessions'/('rollout-'+tid+'.jsonl');m=json.loads(p.read_text());m['payload']['history_base']['thread_id']=tid;p.write_text(json.dumps(m)+'\\n')",
				"wrong-id":        "p=h/'sessions'/('rollout-'+tid+'.jsonl');m=json.loads(p.read_text());m['payload']['id']='wrong';p.write_text(json.dumps(m)+'\\n')",
				"bad-config":      "(h/'config.toml').write_text('broken = [')",
			}
			previewPython(t, home, "import pathlib,json,sys\nh=pathlib.Path(sys.argv[1]);tid='"+previewChild+"'\n"+mutations[condition])
			if _, err := RepairForkPreview(context.Background(), home, previewChild); err == nil {
				t.Fatal("unsafe storage was accepted")
			}
		})
	}
}

func TestForkPreviewUsesNativeDefaultSQLiteHome(t *testing.T) {
	t.Setenv("CODEX_SQLITE_HOME", "")
	home := newPreviewFixture(t)
	previewPython(t, home, `import pathlib,sys
h=pathlib.Path(sys.argv[1]);(h/'config.toml').unlink()
for p in (h/'db').iterdir():p.rename(h/p.name)`)
	if got, err := RepairForkPreview(context.Background(), home, previewChild); err != nil || got == "" {
		t.Fatalf("native default sqlite home: %q, %v", got, err)
	}
}

func TestForkPreviewHonorsCancellation(t *testing.T) {
	home := newPreviewFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RepairForkPreview(ctx, home, previewChild); err == nil {
		t.Fatal("cancelled repair succeeded")
	}
}

func TestForkPreviewAcceptsUnspecifiedUserThreadSource(t *testing.T) {
	home := newPreviewFixture(t)
	previewPython(t, home, `import sqlite3,pathlib,sys
c=sqlite3.connect(pathlib.Path(sys.argv[1])/'db/state_5.sqlite');c.execute('update threads set thread_source=NULL');c.commit()`)
	if got, err := RepairForkPreview(context.Background(), home, previewChild); err != nil || got == "" {
		t.Fatalf("native unspecified source: %q, %v", got, err)
	}
}

func TestForkPreviewSQLiteEnvironmentAndConfigPrecedence(t *testing.T) {
	for _, mode := range []string{"absolute-env", "relative-env", "config-wins"} {
		t.Run(mode, func(t *testing.T) {
			home := newPreviewFixture(t)
			if mode != "config-wins" {
				if err := os.Remove(filepath.Join(home, "config.toml")); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "absolute-env":
				t.Setenv("CODEX_SQLITE_HOME", "  "+filepath.Join(home, "db")+" ")
			case "relative-env":
				t.Chdir(home)
				t.Setenv("CODEX_SQLITE_HOME", "db")
			case "config-wins":
				t.Setenv("CODEX_SQLITE_HOME", filepath.Join(home, "nonexistent"))
			}
			if got, err := RepairForkPreview(context.Background(), home, previewChild); err != nil || got == "" {
				t.Fatalf("sqlite environment: %q, %v", got, err)
			}
		})
	}
}
