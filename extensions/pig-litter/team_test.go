package piglitter

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestTeamAdmissionReadsToolResultAfterStringMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	transcript := `{"type":"session","id":"root-uuid"}` + "\n" +
		`{"type":"message","message":{"role":"system","content":"Instructions"}}` + "\n" +
		`{"type":"message","message":{"role":"toolResult","toolCallId":"call-1","toolName":"team_spawn","content":[{"type":"text","text":"accepted receipt"}],"isError":false}}` + "\n"
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	owner := &owner{sessionID: "root-uuid"}
	selected := &run{teamSessionFile: path, teamResult: sha256.Sum256([]byte("accepted receipt"))}
	if !owner.teamResultStored(&callTicket{callID: "call-1"}, selected) {
		t.Fatal("the matching stored team result was not recognized")
	}
	if owner.teamResultStored(&callTicket{callID: "other-call"}, selected) {
		t.Fatal("a different call matched the stored result")
	}
	owner.sessionID = "other-root"
	if owner.teamResultStored(&callTicket{callID: "call-1"}, selected) {
		t.Fatal("a different root matched the stored result")
	}
}

func TestTeamWritersKeepOrdinaryOwnershipAndNameIndex(t *testing.T) {
	tree := newTree(defaultConfig())
	tree.root.id = "018f0000-0000-7000-8000-000000000001"
	team := TeamID{Root: tree.root.id, Epoch: 1}
	worker, _ := agentDefaults("worker")
	scout, _ := agentDefaults("scout")
	first := &teamMember{Ref: MemberRef{Team: team, ID: "018f0000-0000-7000-8000-000000000002", Generation: 1}, Name: "Alice", Workspace: WorkspacePolicy{Kind: "shared_checkout", Cwd: "/work"}}
	second := &teamMember{Ref: MemberRef{Team: team, ID: "018f0000-0000-7000-8000-000000000003", Generation: 2}, Name: "Bob", Workspace: WorkspacePolicy{Kind: "shared_checkout", Cwd: "/work"}}
	_, alice, err := tree.admit(admission{kind: "worker", name: "Alice", cwd: "/work", task: "write", agent: worker, member: first})
	if err != nil {
		t.Fatal(err)
	}
	_, bob, err := tree.admit(admission{kind: "worker", name: "Bob", cwd: "/work", task: "write", agent: worker, member: second})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tree.admit(admission{kind: "worker", name: "ordinary-writer", cwd: "/work", task: "write", agent: worker}); err == nil || err.Error() != "writer conflict in the shared working directory" {
		t.Fatalf("ordinary writer conflict = %v", err)
	}
	ordinary, ordinaryRun, err := tree.admit(admission{kind: "scout", name: "Alice", cwd: "/work", task: "read", agent: scout})
	if err != nil {
		t.Fatal(err)
	}
	children, err := tree.list(nil)
	if err != nil || len(children) != 1 || children[0].ID != ordinary.ID || children[0].Name != "Alice" {
		t.Fatalf("ordinary children = %#v, %v", children, err)
	}
	if len(tree.pending) != 1 || tree.live != 3 {
		t.Fatalf("ordinary reservations = %d, live = %d", len(tree.pending), tree.live)
	}
	tree.cancelPending(alice)
	children, err = tree.list(nil)
	if err != nil || len(children) != 1 || children[0].ID != ordinary.ID {
		t.Fatalf("ordinary child after teammate rollback = %#v, %v", children, err)
	}
	tree.cancelPending(ordinaryRun)
	_, replacement, err := tree.admit(admission{kind: "scout", name: "Alice", cwd: "/work", task: "read", agent: scout})
	if err != nil {
		t.Fatal(err)
	}
	tree.cancelPending(replacement)
	tree.cancelPending(bob)
	if tree.live != 0 || len(tree.pending) != 0 {
		t.Fatalf("resources after rollback: live %d, reservations %d", tree.live, len(tree.pending))
	}
	tree.close()
}

func TestOrdinaryWriterBlocksTeamWriter(t *testing.T) {
	tree := newTree(defaultConfig())
	tree.root.id = "018f0000-0000-7000-8000-000000000001"
	worker, _ := agentDefaults("worker")
	_, ordinary, err := tree.admit(admission{kind: "worker", name: "ordinary", cwd: "/work", task: "write", agent: worker})
	if err != nil {
		t.Fatal(err)
	}
	member := &teamMember{Ref: MemberRef{Team: TeamID{Root: tree.root.id, Epoch: 1}, ID: "018f0000-0000-7000-8000-000000000002", Generation: 1}, Name: "Alice", Workspace: WorkspacePolicy{Kind: "shared_checkout", Cwd: "/work"}}
	if _, _, err := tree.admit(admission{kind: "worker", name: "Alice", cwd: "/work", task: "write", agent: worker, member: member}); err == nil || err.Error() != "writer conflict in the shared working directory" {
		t.Fatalf("teammate writer conflict = %v", err)
	}
	tree.cancelPending(ordinary)
	tree.close()
}

func TestTeamNamesRejectReservedAndControlAddresses(t *testing.T) {
	for _, name := range []string{"team-lead", "USER", "main", "system", "Alice@host", "x\x1b[31m", "a0123456789abcdef"} {
		if err := validateTeamName(name); err == nil {
			t.Fatalf("accepted invalid teammate name %q", name)
		}
	}
	if err := validateTeamName("Alice-2"); err != nil {
		t.Fatalf("valid teammate name: %v", err)
	}
}
