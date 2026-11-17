package dbrole

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// legacy is the part of the crm reference schema (legacy-schema.sql)
// zm-connect touches, owned by acme_agent as on the box.
const legacy = `
create schema crm authorization acme_agent;
create schema enrich authorization acme_agent;
set role acme_agent;
create table crm.outreach (
  email text not null, channel text not null, campaign text not null default '', owner text,
  name text, first_name text, program_code text, program text, specialty text, state text,
  status text, skip_reason text, verify text, chief_status text, subject text, body text, hook text,
  thread_id text, sent_at timestamptz, last_out_at timestamptz, last_in_at timestamptz,
  reply_label text, reply_summary text, next_step text, notes text,
  blocklisted boolean not null default false, updated_at timestamptz not null default now(),
  primary key (email, channel, campaign));
create table crm.messages (id text primary key, email text not null, channel text not null, thread_id text,
  at timestamptz not null, direction text not null, sender text, subject text, body text, label text, summary text);
create table crm.load_log (id serial primary key, note text);
create view crm.contacts as select email, bool_or(status in ('sent','replied','bounced')) as contacted
  from crm.outreach group by email;
create table enrich.chief_enrichment (email text);
reset role;
create table public.people (email text);
`

type pg struct {
	t      *testing.T
	bin    string
	dir    string
	sock   string
	port   int
	stopFn func()
}

func startPG(t *testing.T) *pg {
	t.Helper()
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		t.Skip("no initdb on PATH (Postgres tests run where Postgres is installed)")
	}
	bin := filepath.Dir(initdb)
	dir, err := os.MkdirTemp("", "zmpg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	data := filepath.Join(dir, "d")
	if out, err := exec.Command(initdb, "-D", data, "-U", "postgres", "--auth-local=trust", "--auth-host=scram-sha-256", "-E", "UTF8").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	p := &pg{t: t, bin: bin, dir: data, sock: dir, port: port}
	cmd := exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-w", "-l", filepath.Join(dir, "log"),
		"-o", fmt.Sprintf("-p %d -k %s -c listen_addresses=127.0.0.1 -c fsync=off", port, dir), "start")
	if out, err := cmd.CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(dir, "log"))
		t.Fatalf("pg_ctl start: %v\n%s\n%s", err, out, log)
	}
	t.Cleanup(func() {
		exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-m", "immediate", "stop").Run()
	})
	return p
}

// psql runs sql as the superuser over the socket; it returns the output.
func (p *pg) psql(db, sql string) (string, error) {
	cmd := exec.Command(filepath.Join(p.bin, "psql"), "-X", "-q", "-v", "ON_ERROR_STOP=1", "-h", p.sock,
		"-p", fmt.Sprint(p.port), "-U", "postgres", "-d", db, "-f", "-")
	cmd.Stdin = strings.NewReader(sql)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (p *pg) must(db, sql string) string {
	p.t.Helper()
	out, err := p.psql(db, sql)
	if err != nil {
		p.t.Fatalf("psql: %v\n%s", err, out)
	}
	return out
}

func TestRoleSQLAndStoreOnRealPostgres(t *testing.T) {
	p := startPG(t)
	p.must("postgres", "create role acme_agent login password 'agentpw'; create database acme_leads;")
	p.must("acme_leads", legacy)
	p.must("acme_leads", `insert into crm.outreach (email, channel, campaign, owner, name, program, status)
		values ('A@hosp.org', 'gmail', 'chiefs', 'ivo', 'Dr. A', 'Hosp IM', 'drafted'),
		       ('b@clinic.org', 'instantly', 'ACGME East', null, 'Dr. B', 'Clinic FM', 'active'),
		       ('c@x.org', 'gmail', 'pd-new', 'ivo', 'Dr. C', 'X', 'sent'),
		       ('d@two.org', 'gmail', 'chiefs', 'ivo', 'Dr. D', 'Two', 'drafted')`)

	pw := NewPassword()
	ver, err := NewSCRAMVerifier(pw)
	if err != nil {
		t.Fatal(err)
	}
	sql, err := RoleSQL("acme_leads", "acme_agent", ver, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, pw) {
		t.Fatal("the role SQL carries the password")
	}
	p.must("acme_leads", sql)
	out := p.must("acme_leads", sql) // idempotent
	if strings.Contains(out, "can still write") {
		t.Fatalf("warning on a clean setup: %s", out)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("host=127.0.0.1 port=%d dbname=acme_leads user=zm_connect sslmode=disable password=wrong", p.port))
	if err == nil || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("a wrong password: %v", err)
	}
	conn, err = pgx.Connect(ctx, fmt.Sprintf("host=127.0.0.1 port=%d dbname=acme_leads user=zm_connect sslmode=disable password=%s", p.port, pw))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	w := worker{t: t, conn: conn}

	rows, err := conn.Query(ctx, w.sql("leads"), []string{"a@hosp.org", "b@clinic.org", "nobody@x.org"})
	if err != nil {
		t.Fatal(err)
	}
	leads := map[string][3]string{}
	for rows.Next() {
		var k, e, n, pr string
		rows.Scan(&k, &e, &n, &pr)
		leads[k] = [3]string{e, n, pr}
	}
	if len(leads) != 2 || leads["a@hosp.org"][0] != "A@hosp.org" || leads["b@clinic.org"][2] != "Clinic FM" {
		t.Fatalf("leads %v", leads)
	}
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, m := range []msg{
		{id: "m1", email: "A@hosp.org", thread: "t1", at: at, dir: "out", subject: "Hi", body: "hello"},
		{id: "m2", email: "A@hosp.org", thread: "t1", at: at.Add(time.Hour), dir: "in", sender: "a@hosp.org"},
		{id: "m3", email: "b@clinic.org", thread: "t3", at: at, dir: "out", subject: "By hand"},
		{id: "m4", email: "c@x.org", at: at, dir: "bounce"},
		{id: "m5", email: "c@x.org", at: at.Add(2 * time.Hour), dir: "in", label: "auto-reply"},
		{id: "m6", email: "A@hosp.org", at: at, dir: "out", others: []string{"d@two.org"}},
	} {
		if !w.log(ctx, m) {
			t.Fatalf("log %s: not inserted", m.id)
		}
	}
	if w.log(ctx, msg{id: "m1", email: "A@hosp.org", at: at, dir: "out"}) {
		t.Fatal("relogged m1")
	}
	var known []string
	rs, _ := conn.Query(ctx, w.sql("known"), []string{"m1", "m9"})
	for rs.Next() {
		var id string
		rs.Scan(&id)
		known = append(known, id)
	}
	if strings.Join(known, ",") != "m1" {
		t.Fatalf("known %v", known)
	}
	// zm_connect cannot delete or change the schema; acme_agent reads but no longer writes.
	if _, err := conn.Exec(ctx, "delete from crm.messages"); err == nil {
		t.Error("zm_connect deleted")
	}
	if _, err := conn.Exec(ctx, "create table crm.x (a int)"); err == nil {
		t.Error("zm_connect created a table")
	}
	if _, err := conn.Exec(ctx, "insert into public.people values ('x')"); err == nil {
		t.Error("zm_connect wrote public")
	}
	if _, err := conn.Exec(ctx, "select count(*) from enrich.chief_enrichment"); err != nil {
		t.Errorf("zm_connect cannot read enrich: %v", err)
	}
	asAgent := "set role acme_agent; "
	if out, err := p.psql("acme_leads", asAgent+"insert into crm.messages (id,email,channel,at,direction) values ('x','x','x',now(),'in');"); err == nil {
		t.Errorf("acme_agent still writes crm: %s", out)
	}
	if out, err := p.psql("acme_leads", asAgent+"grant insert on crm.messages to acme_agent; "+
		"insert into crm.messages (id,email,channel,at,direction) values ('y','x','x',now(),'in');"); err == nil {
		t.Errorf("acme_agent granted itself writes back: %s", out)
	}
	if out, err := p.psql("acme_leads", asAgent+"select count(*) from crm.contacts; select count(*) from crm.messages;"); err != nil {
		t.Errorf("acme_agent cannot read crm: %v %s", err, out)
	}
	// leads_loader loads enrich and public and only reads crm.
	p.must("acme_leads", "alter role leads_loader password 'loaderpw';")
	lc, err := pgx.Connect(ctx, fmt.Sprintf("host=127.0.0.1 port=%d dbname=acme_leads user=leads_loader sslmode=disable password=loaderpw", p.port))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lc.Exec(ctx, "insert into public.people values ('new@x.org')"); err != nil {
		t.Errorf("leads_loader cannot load public: %v", err)
	}
	if _, err := lc.Exec(ctx, "insert into enrich.chief_enrichment values ('new@x.org')"); err != nil {
		t.Errorf("leads_loader cannot load enrich: %v", err)
	}
	if _, err := lc.Exec(ctx, "insert into crm.messages (id,email,channel,at,direction) values ('z','x','x',now(),'in')"); err == nil {
		t.Error("leads_loader wrote crm")
	}
	if _, err := lc.Exec(ctx, "select count(*) from crm.outreach"); err != nil {
		t.Errorf("leads_loader cannot read crm: %v", err)
	}
	lc.Close(ctx)

	// A new crm table made by crm_owner gets the grants too.
	p.must("acme_leads", "set role crm_owner; create table crm.later (a int);")
	if _, err := conn.Exec(ctx, "insert into crm.later values (1)"); err != nil {
		t.Errorf("default privileges: %v", err)
	}

	// A trust rule for loopback is refused too unless the database is in a
	// bridged container (where loopback is the container's own).
	hbaPath := filepath.Join(p.dir, "pg_hba.conf")
	orig, _ := os.ReadFile(hbaPath)
	os.WriteFile(hbaPath, append([]byte("host all all 127.0.0.1/32 trust\n"), orig...), 0o600)
	p.must("postgres", "select pg_reload_conf();")
	if out, err := p.psql("acme_leads", sql); err == nil {
		t.Fatalf("loopback trust not refused:\n%s", out)
	}
	isolated, _ := RoleSQL("acme_leads", "acme_agent", ver, true)
	p.must("acme_leads", isolated)
	os.WriteFile(hbaPath, orig, 0o600)
	p.must("postgres", "select pg_reload_conf();")

	// An md5 rule for any user, too.
	os.WriteFile(hbaPath, append([]byte("host all leads_loader 10.0.0.0/8 md5\n"), orig...), 0o600)
	p.must("postgres", "select pg_reload_conf();")
	if out, err := p.psql("acme_leads", sql); err == nil {
		t.Fatalf("md5 rule not refused:\n%s", out)
	}
	os.WriteFile(hbaPath, orig, 0o600)
	p.must("postgres", "select pg_reload_conf();")

	// A trust rule for the network makes the setup refuse before changing anything.
	hba := filepath.Join(p.dir, "pg_hba.conf")
	b, _ := os.ReadFile(hba)
	os.WriteFile(hba, append([]byte("host all all 0.0.0.0/0 trust\n"), b...), 0o600)
	p.must("postgres", "select pg_reload_conf();")
	if out, err := p.psql("acme_leads", sql); err == nil || !strings.Contains(out, "TCP rule without scram-sha-256") {
		t.Fatalf("trust rule not refused: %v\n%s", err, out)
	}
}

// worker runs the Worker's own SQL files (deploy/cloudflare/zm-connect/sql) with the same parameters and
// the same order as src/db.ts, so the SQL that ships is the SQL tested here.
type worker struct {
	t    *testing.T
	conn *pgx.Conn
}

type msg struct {
	id, email, thread, dir, sender, subject, body, label string
	at                                                   time.Time
	others                                               []string
}

func (w worker) sql(name string) string {
	// A public copy leaves deploy/ out altogether; in the private tree a missing SQL file fails.
	if _, err := os.Stat(filepath.Join("..", "..", "..", "deploy")); errors.Is(err, fs.ErrNotExist) {
		w.t.Skip("no deploy/: a public copy leaves the Worker and its SQL out")
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "cloudflare", "zm-connect", "sql", name+".sql"))
	if err != nil {
		w.t.Fatal(err)
	}
	return string(b)
}

func (w worker) log(ctx context.Context, m msg) bool {
	tx, err := w.conn.Begin(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, w.sql("insert_message"), m.id, m.email, "gmail", m.thread, m.at, m.dir, m.sender, m.subject, m.body, m.label)
	if err != nil {
		w.t.Fatal(err)
	}
	if tag.RowsAffected() == 0 {
		tx.Commit(ctx)
		return false
	}
	if m.label != "auto-reply" {
		for _, e := range append([]string{m.email}, m.others...) {
			e = strings.ToLower(e)
			switch m.dir {
			case "out":
				t, err := tx.Exec(ctx, w.sql("touch_out"), e, m.at, m.thread)
				if err == nil && t.RowsAffected() == 0 {
					_, err = tx.Exec(ctx, w.sql("touch_out_new"), e, "ivo", m.subject, m.thread, m.at)
				}
				if err != nil {
					w.t.Fatal(err)
				}
			case "in":
				if _, err := tx.Exec(ctx, w.sql("touch_in"), e, m.at); err != nil {
					w.t.Fatal(err)
				}
			case "bounce":
				if _, err := tx.Exec(ctx, w.sql("touch_bounce"), e); err != nil {
					w.t.Fatal(err)
				}
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		w.t.Fatal(err)
	}
	return true
}
