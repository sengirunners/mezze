package dbrole

import (
	"fmt"
	"regexp"
	"strings"
)

var ident = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// RoleSQL is the database setup a superuser runs once (idempotent, one
// transaction), holding no secret: zm_connect's password is a SCRAM
// verifier. It
//   - refuses, before changing anything, any TCP pg_hba rule (any user)
//     that is not scram-sha-256 or cert, and any unreadable line;
//   - makes leads_loader (the scraper: loads enrich and public, reads crm);
//   - makes NOLOGIN crm_owner the owner of schema crm and everything in it,
//     so neither acme_agent nor zm_connect can grant itself rights back;
//   - makes zm_connect (login, nothing else) with SELECT, INSERT and UPDATE
//     on crm (no DELETE, TRUNCATE or DDL) and SELECT on enrich and public;
//   - takes crm writes away from acme_agent and PUBLIC (D3), keeps SELECT,
//     and warns if a role acme_agent belongs to still lets it write;
//   - keeps jacks_owner's (the owner login) rights when that role exists;
//   - sets default privileges so new crm tables get the same grants.
//
// New crm tables must then be created as crm_owner (SET ROLE crm_owner).
//
// loopbackIsolated is true only when the database runs in a container on
// a bridge network, where 127.0.0.1 is the container's own and no box user
// can reach it; elsewhere a loopback trust rule is refused like any other.
func RoleSQL(database, agentRole, verifier string, loopbackIsolated bool) (string, error) {
	if !ident.MatchString(database) || !ident.MatchString(agentRole) {
		return "", fmt.Errorf("dbrole: bad database or role name")
	}
	if !strings.HasPrefix(verifier, "SCRAM-SHA-256$") || strings.ContainsAny(verifier, "'\\\n") {
		return "", fmt.Errorf("dbrole: not a SCRAM verifier")
	}
	loop := "''"
	if loopbackIsolated {
		loop = "'127.0.0.1', '::1', 'localhost'"
	}
	r := strings.NewReplacer("{db}", database, "{agent}", agentRole, "{verifier}", verifier, "{loopback}", loop)
	return r.Replace(roleSQL), nil
}

const roleSQL = `-- zm-connect database setup.
-- Holds no secret: the password below is a SCRAM-SHA-256 verifier. Run as a superuser:
--   psql -v ON_ERROR_STOP=1 -d {db} -f db-role.sql
\set ON_ERROR_STOP on
begin;

do $$
begin
  -- Every TCP rule, for every user, must use scram-sha-256 or a client certificate (review: the image's own
  -- rules and initdb's loopback trust apply to every role, not only zm_connect).
  if exists (
    select 1 from pg_hba_file_rules
    where type in ('host', 'hostssl', 'hostnossl')
      and auth_method not in ('scram-sha-256', 'cert')
      and coalesce(address, '') not in ({loopback})
  ) then
    raise exception 'pg_hba.conf has a TCP rule without scram-sha-256 (trust, password or md5); fix pg_hba.conf first';
  end if;
  if exists (select 1 from pg_hba_file_rules where error is not null) then
    raise exception 'pg_hba.conf has a line Postgres cannot read; fix it first';
  end if;
end $$;

do $$
begin
  if not exists (select 1 from pg_roles where rolname = 'crm_owner') then
    create role crm_owner nologin;
  end if;
  if not exists (select 1 from pg_roles where rolname = 'zm_connect') then
    create role zm_connect login;
  end if;
  -- The scraper's role. The owner sets its password (alter role leads_loader password ...).
  if not exists (select 1 from pg_roles where rolname = 'leads_loader') then
    create role leads_loader login;
  end if;
end $$;

alter role zm_connect login nosuperuser nocreatedb nocreaterole noinherit noreplication nobypassrls
  connection limit 4 password '{verifier}';

alter role leads_loader login nosuperuser nocreatedb nocreaterole noinherit noreplication nobypassrls connection limit 8;

alter schema crm owner to crm_owner;

do $$
declare r record;
begin
  for r in
    select c.relname, c.relkind from pg_class c join pg_namespace n on n.oid = c.relnamespace
    where n.nspname = 'crm' and c.relkind in ('r', 'p', 'v', 'm', 'f', 'S')
      and not (c.relkind = 'S' and exists (
        select 1 from pg_depend d where d.objid = c.oid and d.classid = 'pg_class'::regclass and d.deptype in ('a', 'i')))
  loop
    execute format('alter %s crm.%I owner to crm_owner',
      case r.relkind when 'v' then 'view' when 'm' then 'materialized view' when 'f' then 'foreign table'
        when 'S' then 'sequence' else 'table' end, r.relname);
  end loop;
end $$;

revoke create on schema crm from public;
revoke insert, update, delete, truncate on all tables in schema crm from public, {agent};
grant usage on schema crm to {agent}, zm_connect;
grant select on all tables in schema crm to {agent};
grant select, insert, update on all tables in schema crm to zm_connect;
grant usage, select on all sequences in schema crm to zm_connect;
alter default privileges for role crm_owner in schema crm grant select on tables to {agent};
alter default privileges for role crm_owner in schema crm grant select, insert, update on tables to zm_connect;
alter default privileges for role crm_owner in schema crm grant usage, select on sequences to zm_connect;

do $$
begin
  if exists (select 1 from pg_roles where rolname = 'jacks_owner') then
    execute 'grant usage on schema crm to jacks_owner';
    execute 'grant select, insert, update, delete on all tables in schema crm to jacks_owner';
    execute 'alter default privileges for role crm_owner in schema crm grant select, insert, update, delete on tables to jacks_owner';
  end if;
end $$;

grant connect on database {db} to zm_connect, leads_loader;
-- leads_loader: loads enrich.* and public.* (it may rebuild their tables), reads crm, never writes it.
grant usage on schema crm to leads_loader;
grant select on all tables in schema crm to leads_loader;
alter default privileges for role crm_owner in schema crm grant select on tables to leads_loader;
do $$
declare s text;
begin
  foreach s in array array['enrich', 'public'] loop
    if exists (select 1 from pg_namespace where nspname = s) then
      execute format('grant usage on schema %I to zm_connect', s);
      execute format('grant select on all tables in schema %I to zm_connect', s);
      execute format('grant usage, create on schema %I to leads_loader', s);
      execute format('grant select, insert, update, delete, truncate on all tables in schema %I to leads_loader', s);
      execute format('grant usage, select, update on all sequences in schema %I to leads_loader', s);
    end if;
  end loop;
end $$;

do $$
declare t text;
begin
  for t in
    select c.relname from pg_class c join pg_namespace n on n.oid = c.relnamespace
    where n.nspname = 'crm' and c.relkind in ('r', 'p')
      and (has_table_privilege('{agent}', c.oid, 'insert') or has_table_privilege('{agent}', c.oid, 'update')
        or has_table_privilege('{agent}', c.oid, 'delete'))
  loop
    raise warning '{agent} can still write crm.% through a role it belongs to', t;
  end loop;
end $$;

commit;
`
