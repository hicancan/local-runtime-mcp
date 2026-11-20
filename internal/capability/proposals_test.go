package capability

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProposalsWaitForTheOwnerAndApprovalReloadsTheCatalog(t *testing.T) {
	r, _ := registryFixture(t)
	changed := 0
	r.Watch(func() { changed++ })

	if _, err := r.Propose("course_outline", []byte(definitionJSON), "function () { return {}; }"); err == nil {
		t.Fatal("a name without a namespace was accepted")
	}
	if _, err := r.Propose("example.bad", []byte(`{"description":"x"}`), "function () {}"); err == nil {
		t.Fatal("a definition that does not validate was stored")
	}
	if _, err := r.Propose("example.run", []byte(`{"description":"Run a program.","command":{"program":"cmd"}}`), ""); err == nil {
		t.Fatal("a local app binding was proposed")
	}
	proposal, err := r.Propose("example.read", []byte(definitionJSON), "async function(input) { return {text:input.text}; }")
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Replaces || proposal.Definition.Name != "example.read" {
		t.Fatalf("unexpected proposal: %+v", proposal)
	}
	// A draft is not a tool until the owner approves it, even across reloads.
	if err := r.Reload(); err != nil {
		t.Fatal(err)
	}
	if len(r.List()) != 1 || len(r.Proposals()) != 1 {
		t.Fatalf("draft leaked into the catalog: %d tools, %d drafts", len(r.List()), len(r.Proposals()))
	}
	changed = 0
	if err := r.Approve("example.read"); err != nil {
		t.Fatal(err)
	}
	if len(r.List()) != 2 || len(r.Proposals()) != 0 || changed != 1 {
		t.Fatalf("approval did not take effect: %d tools, %d drafts, %d reloads", len(r.List()), len(r.Proposals()), changed)
	}
	if _, err := os.Stat(filepath.Join(r.root, "example", "read.js")); err != nil {
		t.Fatal("approved script not in the catalog")
	}
	// Proposing an approved name again is a replacement, and rejecting drops it.
	again, err := r.Propose("example.read", []byte(definitionJSON), "async function(input) { return {text:'v2'}; }")
	if err != nil || !again.Replaces {
		t.Fatalf("replacement not recognised: %+v %v", again, err)
	}
	if err := r.Reject("example.read"); err != nil {
		t.Fatal(err)
	}
	if len(r.Proposals()) != 0 || r.Approve("example.read") == nil {
		t.Fatal("a rejected draft could still be approved")
	}
}
