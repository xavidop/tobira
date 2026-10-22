package oracle

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestCheckReadOnlyAllows(t *testing.T) {
	for _, q := range []string{
		"select * from tobira_people",
		"SELECT 1 FROM dual",
		"select 1 from dual;",
		"select 1 from dual ; -- trailing comment",
		"  \n select 1",
		"(select 1 from dual) union all (select 2 from dual)",
		"with x as (select 1 n from dual) select n from x",
		"with t (n) as (select 1 from dual union all select n + 1 from t where n < 3) search depth first by n set ord select * from t",
		"with t (n) as (select 1 from dual union all select n + 1 from t where n < 3) cycle n set is_cycle to 'Y' default 'N' select * from t",
		"select /*+ full(p) */ * from tobira_people p",
		"select 'delete from t; drop table t' from dual",
		"select q'[it's ; delete]' from dual",
		"select nq'{x}' from dual",
		`select "DELETE", "INTO", "@" from t`,
		"select * from t where id = :1",
		"select * from t where name = :name",
		"select count(*) from all_objects a, all_objects b",
		"select * from t fetch first 10 rows only",
		"select * from t offset 5 rows fetch next 10 rows only",
		"select * from t as of timestamp systimestamp - interval '1' minute",
		"select 1e3, 1.5, 2.5e+10, 1.5f, 2d, .5 from dual",
		"select x from t for share", // not an Oracle clause; the server refuses it
		"select v$x, a#b from t",
		"select * from t where b = true",
		"select json_value(j, '$.a') from t",
		"select * from tobira_people where name like 'A%' escape '!'",
		"select sys_context('USERENV', 'CURRENT_SCHEMA') from dual",
		"select * from t\nwhere x = 1 -- comment\n",
		"select 'café ñ' from dual",
		`select "Café" from t`,
		"select q'[ü\u00a0]' from dual -- ünïcode comment",
		"select 1 /* é */ from dual",
	} {
		if err := checkReadOnly(q); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", q, err)
		}
	}
}

func TestCheckReadOnlyRefuses(t *testing.T) {
	for _, q := range []string{
		"",
		"   ",
		"-- only a comment",
		"delete from tobira_people where id = 1",
		"insert into t values (1)",
		"update t set a = 1",
		"merge into t using s on (t.id = s.id) when matched then update set t.a = s.a",
		"truncate table t",
		"drop table t",
		"create table t2 as select * from t",
		"alter session set current_schema = x",
		"alter table t add (c number)",
		"grant select on t to public",
		"revoke select on t from public",
		"rename t to t2",
		"comment on table t is 'x'",
		"analyze table t compute statistics",
		"audit select on t",
		"purge recyclebin",
		"flashback table t to before drop",
		"lock table t in exclusive mode",
		"commit",
		"rollback",
		"savepoint a",
		"set transaction read write",
		"set role all",
		"call p()",
		"exec p",
		"execute immediate 'drop table t'",
		"begin null; end;",
		"begin delete from t; end;",
		"declare x number; begin null; end;",
		"<<lbl>> begin null; end;",
		"explain plan for select 1 from dual",
		"select * from t for update",
		"select * from t for update nowait",
		"select * from t for update of a skip locked",
		"select 1 into x from dual",
		"select 1 from dual; delete from t",
		"select 1 from dual; select 2 from dual",
		"select 1 from dual;;",
		"with function f return number is begin return 1; end; select f() from dual",
		"with procedure p is begin null; end; select 1 from dual",
		"with function f return number is pragma autonomous_transaction; begin delete from t; commit; return 1; end; select f from dual",
		"select * from t@remote",
		"select * from t @ remote",
		"select dbms_pipe.send_message('x') from dual",
		"select sys.dbms_lock.sleep(1) from dual",
		`select "DBMS_PIPE"."SEND_MESSAGE"('x') from dual`,
		`select "SYS"."DBMS_LOCK".sleep(1) from dual`,
		"select utl_http.request('http://x') from dual",
		"select utl_inaddr.get_host_address('x') from dual",
		`select "UTL_HTTP".request('http://x') from dual`,
		"select httpuritype('http://x').getclob() from dual",
		"select dburitype('/x').getclob() from dual",
		"select xdburitype('/x').getclob() from dual",
		"select urifactory.geturi('http://x') from dual",
		"select owa_util.get_cgi_env('x') from dual",
		"select htp.p('x') from dual",
		"select apex_web_service.make_rest_request('http://x', 'GET') from dual",
		"select seq.nextval from dual",
		`select seq."NEXTVAL" from dual`,
		"select 1delete from dual",
		"select 1into from dual",
		"select 1e from dual",
		"select 1.5x from dual",
		"select 'unterminated from dual",
		`select "unterminated from dual`,
		"select /* unterminated from dual",
		"select q'[unterminated from dual",
		"select q' x ' from dual",
		"select 1 from dual -- comment\rdelete from t",
		"select 1\x00 from dual",
		"select 'a\xff' from dual",
		"select q'\xc3\xa9x\xc3\xa9' from dual",
		"select \xff from dual",
		"select\u00a0* from t",
		"select 1\u2003from dual",
		"select café from t",
		"select 1 from dual\u3000",
		"select a\uff0cb from t",
		"values (1)",
		"table t",
		"show tables",
		"describe t",
		"upsert into t values (1)",
		"administer key management set keystore open identified by x",
		"shutdown immediate",
		"select * from t where c in (select x from s for update)",
		"select f(x) from t where p => 1 and delete = 1",
	} {
		err := checkReadOnly(q)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", q, err)
			continue
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("checkReadOnly(%q): want a *driver.Error from %q, got %#v", q, Name, err)
		}
	}
}

func TestCheckReadOnlyReasons(t *testing.T) {
	for _, tc := range []struct{ q, want string }{
		{"delete from t", "DELETE"},
		{"select 1 from dual; select 2 from dual", "one statement"},
		{"select * from t@x", "database link"},
		{"select dbms_lock.sleep(1) from dual", "DBMS_LOCK"},
		{"select 'x", "unterminated"},
		{"select 1delete from dual", "malformed number"},
		{"select 1 -- a\rb", "carriage return"},
		{"select 'a\xff' from dual", "UTF-8"},
		{"select\u00a0* from t", "non-ASCII"},
		{"select café from t", "non-ASCII"},
		{"select * from t for update", "UPDATE"},
		{"with function f return number is begin return 1; end; select f from dual", "FUNCTION"},
	} {
		err := checkReadOnly(tc.q)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("checkReadOnly(%q) = %v, want a reason containing %q", tc.q, err, tc.want)
		}
	}
}

func TestExplainTarget(t *testing.T) {
	for _, q := range []string{
		"select * from t",
		"with x as (select 1 from dual) select * from x",
		"delete from tobira_people where id = 1",
		"insert into t (a) values (seq.nextval)",
		"update t set a = 1 where id = :1",
		"merge into t using s on (t.id = s.id) when matched then update set t.a = s.a",
		"select * from t for update",
		"select 1 from dual;",
	} {
		if err := explainTarget(q); err != nil {
			t.Errorf("explainTarget(%q) = %v, want nil", q, err)
		}
	}
	for _, q := range []string{
		"",
		"drop table t",
		"create table t (a number)",
		"begin null; end;",
		"call p()",
		"commit",
		"explain plan for select 1 from dual",
		"select 1 from dual; delete from t",
		"select * from t@remote",
		"select dbms_lock.sleep(1) from dual",
		"with function f return number is begin return 1; end; select f from dual",
		"alter session set sql_trace = true",
		"select 'x",
		"lock table t in exclusive mode",
		"truncate table t",
		"grant select on t to x",
		"select\u00a0* from t",
	} {
		if err := explainTarget(q); err == nil {
			t.Errorf("explainTarget(%q) = nil, want a refusal", q)
		}
	}
}

func TestCheckTxStatement(t *testing.T) {
	for _, q := range []string{
		"select * from t",
		"update t set a = 1",
		"insert into t values (1)",
		"delete from t where id = 1",
		"merge into t using s on (t.id = s.id) when matched then update set t.a = s.a",
		"lock table t in exclusive mode",
		"savepoint a",
		"rollback to savepoint a",
		"rollback to a",
		"select * from t for update",
		"select * from t@remote",
		"update t set a = 1;",
	} {
		if err := checkTxStatement(q); err != nil {
			t.Errorf("checkTxStatement(%q) = %v, want nil", q, err)
		}
	}
	for _, q := range []string{
		"",
		"commit",
		"commit work",
		"rollback",
		"rollback work",
		"set transaction read only",
		"create table t (a number)",
		"alter table t add (b number)",
		"drop table t",
		"truncate table t",
		"grant select on t to x",
		"begin commit; end;",
		"declare begin null; end;",
		"call p()",
		"exec p",
		"execute immediate 'commit'",
		"comment on table t is 'x'",
		"analyze table t compute statistics",
		"update t set a = 1; commit",
		"with function f return number is begin commit; return 1; end; select f from dual",
		"select 'x",
		"alter session set isolation_level = serializable",
		"purge recyclebin",
		"flashback table t to before drop",
		"rename t to u",
	} {
		err := checkTxStatement(q)
		if err == nil {
			t.Errorf("checkTxStatement(%q) = nil, want a refusal", q)
		}
	}
}

func TestCheckRawFilter(t *testing.T) {
	for _, raw := range []string{
		"age > 30",
		"name like 'A%' and (age is null or age < 50)",
		"x = ';'",
		"x = q'[)]'",
	} {
		if err := checkRawFilter(raw); err != nil {
			t.Errorf("checkRawFilter(%q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{
		"1 = 1) or (1 = 1",
		"(a = 1",
		"a = 1; delete from t",
		"a = 'x",
		"a = 1 -- comment\rx",
		"a = 1 /* x",
	} {
		err := checkRawFilter(raw)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkRawFilter(%q) = %v, want ErrSyntax", raw, err)
		}
	}
}

func TestIsQueryText(t *testing.T) {
	for q, want := range map[string]bool{
		"select 1 from dual":        true,
		"  with x as (select 1) s":  true,
		"(select 1 from dual)":      true,
		"/* c */ select 1":          true,
		"insert into t values (1)":  false,
		"begin null; end;":          false,
		"update t set a = 1":        false,
		"":                          false,
		"create table t (a number)": false,
	} {
		if got := isQueryText(q); got != want {
			t.Errorf("isQueryText(%q) = %v, want %v", q, got, want)
		}
	}
}
