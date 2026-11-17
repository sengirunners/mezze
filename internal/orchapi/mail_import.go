package orchapi

import (
	"time"
)

// LegacyMailImportRequest is a finite, explicitly mapped recovery artifact.
// It carries full bodies, never a feed preview. Only an authenticated own-person
// importer may submit it, locally or through the existing joined authority.
type LegacyMailImportRequest struct {
	SourceInstance string              `json:"source_instance"`
	MappingVersion string              `json:"mapping_version"`
	SourceIDs      []string            `json:"source_ids"`
	Mappings       []LegacyMailMapping `json:"mappings"`
	Records        []LegacyMailRecord  `json:"records"`
	DryRun         bool                `json:"dry_run"`
}

type LegacyMailMapping struct {
	SourceProject   string `json:"source_project"`
	SourceRecipient string `json:"source_recipient"`
	Recipient       string `json:"recipient"`
}

type LegacyMailRecord struct {
	SourceKind   string         `json:"source_kind,omitempty"`
	SourceRef    string         `json:"source_ref,omitempty"`
	Kind         string         `json:"record_kind"`
	ID           string         `json:"source_id"`
	Project      string         `json:"source_project"`
	ThreadID     string         `json:"source_thread_id,omitempty"`
	ParentID     string         `json:"source_parent_id,omitempty"`
	Sender       string         `json:"source_sender"`
	Recipient    string         `json:"source_recipient"`
	SentAt       time.Time      `json:"source_sent_at"`
	Body         string         `json:"body"`
	BodyComplete bool           `json:"body_complete"`
	Delivery     LegacyDelivery `json:"legacy_delivery"`
}

type LegacyMailImportResult struct {
	DryRun  bool                           `json:"dry_run"`
	Partial bool                           `json:"partial"`
	Records []LegacyMailImportRecordResult `json:"records"`
}

type LegacyMailImportRecordResult struct {
	SourceID string `json:"source_id"`
	NativeID string `json:"native_id,omitempty"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
}
