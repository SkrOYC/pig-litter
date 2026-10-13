package piglitter

import (
	"context"
	"regexp"
	"testing"
)

func TestSessionNodesUseNativeUUIDsAndImmediateParentAuthority(t *testing.T) {
	config := defaultConfig()
	config.Depth = 3
	tree := testTree(t, config)
	coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate", Tools: []string{"read"}, CanDelegate: true}
	parent := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "parent", coordinator)
	child := testAdmissionWithAgent(t, tree, parent.Ref, "coordinator", "child", coordinator)
	grandchild := testAdmission(t, tree, child.Ref, "scout", "grandchild")
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for _, snapshot := range []Snapshot{parent, child, grandchild} {
		node := tree.records[snapshot.ID]
		if !uuid.MatchString(string(snapshot.ID)) || string(snapshot.ID) != node.history.GetSessionId() {
			t.Fatalf("child identity is not its native UUID: %#v", snapshot)
		}
	}
	if parent.Parent == nil || *parent.Parent != tree.root.id || tree.root.name != "root" {
		t.Fatalf("root ownership is absent: %#v", parent)
	}
	for _, test := range []struct {
		caller *run
		id     ChildID
	}{
		{nil, child.ID},
		{nil, grandchild.ID},
		{testRun(t, tree, parent.Ref), grandchild.ID},
		{testRun(t, tree, child.Ref), parent.ID},
	} {
		if _, err := tree.selectOwned(test.caller, test.id); err == nil {
			t.Fatalf("UUID knowledge granted nonparent authority for %s", test.id)
		}
	}
	listed, err := tree.list("")
	if err != nil || len(listed) != 1 || listed[0].ID != parent.ID {
		t.Fatalf("root did not list only its immediate active child: %#v %v", listed, err)
	}
	original := testRun(t, tree, parent.Ref)
	tree.finish(tree.records[parent.ID], original, Outcome{State: Completed, Text: "parent finished"})
	tree.acknowledge(original)
	listed, err = tree.list("")
	if err != nil || len(listed) != 0 {
		t.Fatalf("root listed an inactive child: %#v %v", listed, err)
	}
	if err := testRun(t, tree, child.Ref).ctx.Err(); err != nil {
		t.Fatalf("normal parent completion cancelled its child: %v", err)
	}
	resumed, replacement, err := tree.resume(original, "continue")
	if err != nil || resumed.ID != parent.ID {
		t.Fatalf("resume changed identity: %#v %v", resumed, err)
	}
	if _, err := tree.selectOwned(original, child.ID); err == nil {
		t.Fatal("retired parent turn kept control of its child")
	}
	if selected, err := tree.selectOwned(replacement, child.ID); err != nil || selected != testRun(t, tree, child.Ref) {
		t.Fatalf("resumed parent lost its child: %v", err)
	}
}

func TestPauseRetainsCompletedDescendantsAndResumesOnlySelectedNode(t *testing.T) {
	tree := testTree(t, defaultConfig())
	coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate", Tools: []string{"read"}, CanDelegate: true}
	parent := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "parent", coordinator)
	child := testAdmission(t, tree, parent.Ref, "scout", "child")
	parentRun, childRun := testRun(t, tree, parent.Ref), testRun(t, tree, child.Ref)
	tree.finish(tree.records[child.ID], childRun, Outcome{State: Completed, Text: "retained child result"})
	tree.acknowledge(childRun)
	outcome, count, err := tree.stop(parentRun)
	if err != nil || outcome.State != Paused || count != 1 {
		t.Fatalf("pause result %#v %d %v", outcome, count, err)
	}
	inspection, err := tree.inspect(child.ID)
	if err != nil || inspection.Child.State != Paused || inspection.Outcome.State != Completed || inspection.Outcome.Text != "retained child result" {
		t.Fatalf("pause lost the latest result: %#v %v", inspection, err)
	}
	if _, _, err := tree.resume(childRun, "stale resume"); err == nil {
		t.Fatal("a child resumed while its parent was paused")
	}
	tree.acknowledge(parentRun)
	_, resumedParent, err := tree.resume(parentRun, "continue parent")
	if err != nil {
		t.Fatal(err)
	}
	inspection, _ = tree.inspect(child.ID)
	if inspection.Child.State != Paused || tree.records[child.ID].run != childRun {
		t.Fatalf("parent resume restarted its child: %#v", inspection)
	}
	if _, err := tree.selectOwned(resumedParent, child.ID); err != nil {
		t.Fatal(err)
	}
	resumed, resumedChild, err := tree.resume(childRun, "explicitly continue child")
	if err != nil || resumed.ID != child.ID || resumed.State != Starting || resumedChild == childRun {
		t.Fatalf("explicit child resume failed: %#v %v", resumed, err)
	}
	tree.cancelPending(resumedChild)
	inspection, _ = tree.inspect(child.ID)
	if inspection.Child.State != Paused || inspection.Outcome.Text != "retained child result" {
		t.Fatalf("failed resume did not restore the paused session: %#v", inspection)
	}
}

func TestRootPauseIncludesIdleNodesAndBlocksStaleAdmission(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "child")
	selected := testRun(t, tree, child.Ref)
	tree.finish(tree.records[child.ID], selected, Outcome{State: Completed, Text: "done"})
	tree.acknowledge(selected)
	tree.pauseRoot()
	inspection, _ := tree.inspect(child.ID)
	if inspection.Child.State != Paused || inspection.Outcome.Text != "done" {
		t.Fatalf("idle session did not retain paused status: %#v", inspection)
	}
	if _, _, err := tree.admit(admission{agent: tree.config.Agents["scout"]}); err == nil {
		t.Fatal("a paused root admitted a child")
	}
	if _, _, err := tree.message(context.Background(), selected, "resume", "continue"); err == nil {
		t.Fatal("a paused root resumed a child")
	}
	tree.mu.Lock()
	tree.root.paused = false
	tree.mu.Unlock()
	inspection, _ = tree.inspect(child.ID)
	if inspection.Child.State != Paused {
		t.Fatalf("root continuation resumed a child: %#v", inspection)
	}
}
