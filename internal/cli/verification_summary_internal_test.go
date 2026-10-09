package cli

import (
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func TestVerificationSummaryShowsConfiguredStatusRevisionAndEvidence(t *testing.T) {
	revision := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got := verificationSummary(protocol.Verification{Status: "unknown", CriteriaRevision: &revision, EvidenceIDs: []string{"evd_one", "evd_two"}})
	want := "unknown criteria_revision=" + revision + " evidence_ids=evd_one,evd_two"
	if got != want {
		t.Fatalf("verification summary = %q, want %q", got, want)
	}
	if got := verificationSummary(protocol.Verification{Status: "not_run", CriteriaRevision: protocol.Str(revision), EvidenceIDs: []string{}}); got != "not_run criteria_revision="+revision+" evidence_ids=none" {
		t.Fatalf("configured but unexecuted plan summary = %q", got)
	}
	if got := verificationSummary(protocol.Verification{Status: "not_run", EvidenceIDs: []string{}}); got != "not_run" {
		t.Fatalf("an absent plan changed the human summary: %q", got)
	}
}
