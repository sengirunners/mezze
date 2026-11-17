package main

import "github.com/sengirunners/potluck/kit/secrets/inventory"

// The key inventory zm checks (zm doctor, "secret keys"): Potluck's own rows
// plus the keys zm itself keeps in a home. The classes, owners and verdicts
// follow the key inventory decision ("Inventory: zm and Potluck keys"); the
// work item that owns a verdict is tracked there, not in code. A new file zm
// writes under $ZM_HOME that holds a secret needs a row here, and
// keys_inventory_test.go fails until it has one.
//
// Not listed on purpose, because they hold no secret: host_keys.json (the
// slug-to-project map a host froze at migration) and ssh/ (ssh's shared
// connection sockets). Neither sits under a root, so neither is judged.
//
// Known gap: the references in backup.json are not read (the inventory reads
// config.json only), so a literal key there is not found; a config-file list
// in inventory.Table closes it.

// zmKeyRows are the keys zm writes into a home itself.
var zmKeyRows = []inventory.Row{
	{ID: "receipt-key", Name: "receipt signing key (ed25519)", Class: inventory.ClassO,
		Locations: []inventory.Location{inventory.FileAt("receipt.key")}},
	{ID: "backup-key", Name: "home backup key (AES-256)", Class: inventory.ClassO,
		Locations: []inventory.Location{inventory.FileAt("secrets/backup.key")}},
	{ID: "tracker-password", Name: "tracker password", Class: inventory.ClassO,
		Locations: []inventory.Location{inventory.FileAt("secrets/*/*.password")}},
	{ID: "ghlink-token", Name: "GitHub linker's member token", Class: inventory.ClassO,
		Locations: []inventory.Location{inventory.FileAt("secrets/ghlink/*.token")}},
	{ID: "web-push-vapid", Name: "Web Push (VAPID) key", Class: inventory.ClassO,
		Locations: []inventory.Location{inventory.FileAt("secrets/web-push-vapid")}},
	{ID: "receipt-github-token", Name: "receipt-posting GitHub token (per project)", Class: inventory.ClassO,
		Locations: []inventory.Location{inventory.FileAt("approvals/github/*.token")}},
}

// zmKeyRoots are directories zm owns where every file is a key.
var zmKeyRoots = []string{"approvals"}

// zmKeyIgnore are files in a root that are not keys: the marker that the
// first approver was bootstrapped.
var zmKeyIgnore = []string{"approvals/bootstrapped"}

// zmKeyExceptions maps a row id to the bead that moves a key held below its
// class (class P or S rows only). Potluck's own exception (the runner App
// key) comes with inventory.Potluck; zm has none of its own.
var zmKeyExceptions = map[string]string{}

// keyInventory is the table zm doctor checks a home against.
func keyInventory() inventory.Table {
	return inventory.Potluck().With(zmKeyRows, zmKeyRoots, zmKeyIgnore).WithExceptions(zmKeyExceptions)
}
