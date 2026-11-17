package door

import "github.com/sengirunners/potluck/kit/store"

// Migrations are this module's tables; its New adds them (core.Deps.AddMigrations).
var Migrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "door/0001_init", Script: `
CREATE TABLE door_machines (
	project TEXT NOT NULL,
	machine TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 0,
	updated INTEGER NOT NULL,
	PRIMARY KEY (project, machine)
);
CREATE TABLE door_tasks (
	project TEXT NOT NULL,
	machine TEXT NOT NULL,   -- the machine whose Z3 owns the task
	task    TEXT NOT NULL,   -- Z3's task id
	token   TEXT NOT NULL,   -- the URL's credential; '' in a backup copy (rotate re-mints)
	target  TEXT NOT NULL,   -- the task's hook URL on its machine (loopback)
	label   TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL,
	rotated INTEGER NOT NULL,
	PRIMARY KEY (project, machine, task)
);
CREATE UNIQUE INDEX door_tasks_token ON door_tasks (token) WHERE token != '';
CREATE TABLE door_deliveries (
	id       TEXT PRIMARY KEY, -- ULID: the order deliveries go out in
	project  TEXT NOT NULL,
	machine  TEXT NOT NULL,
	task     TEXT NOT NULL,
	delivery TEXT NOT NULL DEFAULT '', -- the sender's delivery id
	event    TEXT NOT NULL DEFAULT '',
	headers  TEXT NOT NULL DEFAULT '', -- JSON, blanked once it ends
	body     BLOB NOT NULL DEFAULT x'', -- blanked once it ends
	bytes    INTEGER NOT NULL,
	state    TEXT NOT NULL,            -- queued | delivered | refused | dropped
	attempts INTEGER NOT NULL DEFAULT 0,
	status   INTEGER NOT NULL DEFAULT 0,
	error    TEXT NOT NULL DEFAULT '',
	received INTEGER NOT NULL,
	next_at  INTEGER NOT NULL,
	ended    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX door_deliveries_queue ON door_deliveries (state, project, machine, id);
CREATE INDEX door_deliveries_task ON door_deliveries (project, machine, task, id);
CREATE UNIQUE INDEX door_deliveries_dedupe ON door_deliveries (project, machine, task, delivery) WHERE delivery != '';`},
}
