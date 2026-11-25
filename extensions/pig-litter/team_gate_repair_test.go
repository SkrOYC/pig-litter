package piglitter

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
)

type failingPlanEntropy struct{}

func (failingPlanEntropy) Read([]byte) (int, error) {
	return 0, errors.New("injected entropy failure")
}

func TestPlanRequestEntropyFailureKeepsGateClosed(t *testing.T) {
	if os.Getenv("TEAM_PLAN_ENTROPY_CHILD") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestPlanRequestEntropyFailureKeepsGateClosed$")
		command.Env = append(os.Environ(), "TEAM_PLAN_ENTROPY_CHILD=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("entropy failure child: %s: %v", output, err)
		}
		return
	}
	store, _, member := planTestMember(t)
	if granted, err := store.planGranted(member.Ref); err != nil || granted {
		t.Fatalf("initial gate = %v, %v", granted, err)
	}
	previous := rand.Reader
	rand.Reader = failingPlanEntropy{}
	defer func() { rand.Reader = previous }()
	if _, err := store.submitPlan(member.Ref, "run-1", "entropy", "Plan.", 60000); err == nil {
		t.Fatal("plan request succeeded without an identity")
	}
	requests, err := store.readPlans(member.Ref)
	if err != nil || len(requests) != 0 {
		t.Fatalf("failed entropy created a request: %#v, %v", requests, err)
	}
	if granted, err := store.planGranted(member.Ref); err != nil || granted {
		t.Fatalf("failed entropy opened the gate: %v, %v", granted, err)
	}
}

func TestPlanRequestIDCannotBeReusedAfterLedgerLoss(t *testing.T) {
	for _, test := range []struct {
		name, mode string
		legacy     bool
	}{{"rollback", "rollback", false}, {"delete", "delete", false}, {"rollback_legacy", "rollback", true}, {"delete_legacy", "delete", true}} {
		t.Run(test.name, func(t *testing.T) {
			store, dir, member := planTestMember(t)
			old, err := store.submitPlan(member.Ref, "old-run", "old-plan", "Old plan.", 60000)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			lockPath, recordPath := planPaths(store.dir, member.Ref)
			if test.legacy {
				data, err := os.ReadFile(recordPath)
				if err != nil {
					t.Fatal(err)
				}
				var legacy planLedger
				if err := json.Unmarshal(data, &legacy); err != nil {
					t.Fatal(err)
				}
				old.ID.Nonce = ""
				legacy.Requests[0].ID = old.ID
				data, err = json.Marshal(legacy)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(recordPath, data, 0o600); err != nil {
					t.Fatal(err)
				}
				legacyOwner, err := openTeamStore(dir, member.Ref.Team.Root)
				if err != nil {
					t.Fatal(err)
				}
				if err := legacyOwner.recoverPlans(member.Ref); err != nil {
					t.Fatal(err)
				}
				history, err := legacyOwner.readPlans(member.Ref)
				if err != nil || len(history) != 1 || history[0].ID != old.ID || history[0].State != "cancelled" {
					t.Fatalf("legacy recovered request = %#v, %v", history, err)
				}
				if err := legacyOwner.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if test.mode == "rollback" {
				empty, err := json.Marshal(planLedger{Version: 1, Member: member.Ref, Requests: []planRequest{}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(recordPath, empty, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(recordPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(lockPath); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := openTeamStore(dir, member.Ref.Team.Root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.recoverPlans(member.Ref); err != nil {
				t.Fatal(err)
			}
			replacement, err := reopened.submitPlan(member.Ref, "new-run", "new-plan", "New plan.", 60000)
			if err != nil {
				t.Fatal(err)
			}
			if err := reopened.expirePlan(old.ID); err != nil {
				t.Fatal(err)
			}
			history, err := reopened.readPlans(member.Ref)
			if err != nil || len(history) != 1 || history[0].ID != replacement.ID || history[0].State != "pending" {
				t.Fatalf("stale expiry changed the replacement: %#v, %v", history, err)
			}
			if _, err := reopened.decidePlan(old.ID, "new-run", "approve", ""); err == nil {
				t.Fatalf("old request %#v approved replacement %#v after %s", old.ID, replacement.ID, test.name)
			}
			if old.ID == replacement.ID {
				t.Fatalf("request identity was reused after %s: %#v", test.name, old.ID)
			}
			if granted, err := reopened.planGranted(member.Ref); err != nil || granted {
				t.Fatalf("stale decision opened the gate: %v, %v", granted, err)
			}
		})
	}
}

func TestLegacyPlanRequestRemainsVisibleAfterReopen(t *testing.T) {
	store, dir, member := planTestMember(t)
	request, err := store.submitPlan(member.Ref, "legacy-run", "legacy", "Prior plan.", 60000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.decidePlan(request.ID, "legacy-run", "approve", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, recordPath := planPaths(store.dir, member.Ref)
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	var legacy planLedger
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.Requests[0].ID.Nonce = ""
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTeamStore(dir, member.Ref.Team.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.recoverPlans(member.Ref); err != nil {
		t.Fatal(err)
	}
	history, err := reopened.readPlans(member.Ref)
	if err != nil || len(history) != 1 || history[0].State != "approved" || history[0].ID.Nonce != "" || history[0].ID.Sequence != 1 {
		t.Fatalf("legacy history = %#v, %v", history, err)
	}
	raw, err := json.Marshal(map[string]any{"action": "get", "requestId": history[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	command, err := parsePlanCommand(raw)
	if err != nil || command.RequestID != history[0].ID {
		t.Fatalf("legacy request lookup = %#v, %v", command, err)
	}
	if granted, err := reopened.planGranted(member.Ref); err != nil || granted {
		t.Fatalf("legacy history restored a grant: %v, %v", granted, err)
	}
	next, err := reopened.submitPlan(member.Ref, "new-run", "new", "New plan.", 60000)
	if err != nil || next.ID.Sequence != 2 || !validPlanNonce(next.ID.Nonce) {
		t.Fatalf("new request after legacy history = %#v, %v", next, err)
	}
}

func TestPlanRequestNonceRequiresCanonicalEncoding(t *testing.T) {
	id := PlanRequestID{Member: MemberRef{Team: TeamID{Root: storeTestUUID(1), Epoch: 1}, ID: storeTestUUID(2), Generation: 1}, Sequence: 1}
	schema, err := json.Marshal(schemas["team_plan"])
	if err != nil {
		t.Fatal(err)
	}
	for _, nonce := range []string{"", "0123456789abcdef0123456789abcdef"} {
		id.Nonce = nonce
		raw, err := json.Marshal(map[string]any{"action": "get", "requestId": id})
		if err != nil {
			t.Fatal(err)
		}
		normalized, err := ai.ValidateToolArgumentsJSON("team_plan", schema, raw)
		if err != nil {
			t.Fatalf("host rejected request ID %s: %v", nonce, err)
		}
		command, err := parsePlanCommand(normalized)
		if err != nil || command.RequestID != id {
			t.Fatalf("valid request ID %s = %#v, %v", nonce, command, err)
		}
	}
	for _, nonce := range []string{"0123456789ABCDEF0123456789ABCDEF", "invalid", "0000000000000000000000000000000g"} {
		id.Nonce = nonce
		raw, err := json.Marshal(map[string]any{"action": "get", "requestId": id})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parsePlanCommand(raw); err == nil {
			t.Fatalf("accepted invalid nonce %q", nonce)
		}
	}
}

func TestTeamAdmissionModeUsesHostCoercedPlanRequired(t *testing.T) {
	_, _, member := planTestMember(t)
	schema, err := json.Marshal(schemas["team_spawn"])
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		input string
		want  bool
	}{{`true`, true}, {`false`, false}, {`"true"`, true}, {`"false"`, false}} {
		t.Run(test.input, func(t *testing.T) {
			arguments := json.RawMessage(`{"type":"worker","task":"Plan","name":"planner","workspace":{"kind":"shared_checkout"},"planRequired":` + test.input + `}`)
			normalized, err := ai.ValidateToolArgumentsJSON("team_spawn", schema, arguments)
			if err != nil {
				t.Fatalf("host did not accept the planning mode: %v", err)
			}
			var actual struct {
				PlanRequired bool `json:"planRequired"`
			}
			if err := json.Unmarshal(normalized, &actual); err != nil || actual.PlanRequired != test.want {
				t.Fatalf("host normalized planning mode = %s, %v", normalized, err)
			}
			call, err := json.Marshal(map[string]any{"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "id": "spawn", "name": "team_spawn", "arguments": arguments}}}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := json.Marshal(map[string]any{"operation": "team_spawn", "state": "starting", "admission": member.AdmissionID, "member": member.Ref, "planRequired": test.want})
			if err != nil {
				t.Fatal(err)
			}
			entry, err := json.Marshal(map[string]any{"message": map[string]any{"role": "toolResult", "toolCallId": "spawn", "toolName": "team_spawn", "content": []any{map[string]any{"type": "text", "text": string(result)}}}})
			if err != nil {
				t.Fatal(err)
			}
			mode, known, err := teamAdmissionMode([]json.RawMessage{call, entry}, member)
			if err != nil || !known || mode != test.want {
				t.Fatalf("recovered host-coerced planning mode = %v, %v, %v", mode, known, err)
			}
		})
	}
}

func TestTeamAdmissionModeDoesNotReusePriorCallIdentity(t *testing.T) {
	_, _, member := planTestMember(t)
	valid := json.RawMessage(`{"message":{"role":"assistant","content":[{"type":"toolCall","id":"spawn","name":"team_spawn","arguments":{"type":"worker","task":"Plan","name":"planner","workspace":{"kind":"shared_checkout"},"planRequired":true}}]}}`)
	invalid := json.RawMessage(`{"message":{"role":"assistant","content":[{"type":"toolCall","id":"spawn","name":"team_spawn","arguments":{"type":"worker","task":"Plan","name":"planner","workspace":{"kind":"shared_checkout"},"planRequired":"invalid"}}]}}`)
	result, err := json.Marshal(map[string]any{"operation": "team_spawn", "state": "starting", "admission": member.AdmissionID, "member": member.Ref, "planRequired": true})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := json.Marshal(map[string]any{"message": map[string]any{"role": "toolResult", "toolCallId": "spawn", "toolName": "team_spawn", "content": []any{map[string]any{"type": "text", "text": string(result)}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := teamAdmissionMode([]json.RawMessage{valid, invalid, entry}, member); err == nil {
		t.Fatal("a later invalid call reused an earlier admission identity")
	}
}

func TestPlanSubmitRetryReportsCurrentGrant(t *testing.T) {
	store, _, member := planTestMember(t)
	tree := testTree(t, defaultConfig())
	tree.root.id = member.Ref.Team.Root
	worker := AgentDefinition{Role: "worker", Tools: []string{"read", "write"}, CanDelegate: true}
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: member.Workspace.Cwd, task: "plan", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	var gotErr error
	var expected PlanRequestID
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		submit := json.RawMessage(`{"action":"submit","submissionId":"retry","text":"Plan the work.","timeoutMs":60000}`)
		value, err := o.planOperation(submit, controlAuthority{caller: selected})
		if err != nil || value["state"] != "pending" || value["gate"] != "closed" {
			gotErr = fmt.Errorf("initial submission = %#v, %v", value, err)
			return
		}
		request := value["request"].(planRequest)
		expected = request.ID
		decision, _ := json.Marshal(map[string]any{"action": "decide", "requestId": request.ID, "decision": "approve"})
		if _, err := o.planOperation(decision, controlAuthority{}); err != nil {
			gotErr = err
			return
		}
		got, gotErr = o.planOperation(submit, controlAuthority{caller: selected})
	}, func(method string, _ json.RawMessage) json.RawMessage {
		if method == "sessionRead" {
			return json.RawMessage(`"fixture"`)
		}
		t.Fatalf("unexpected host method %s", method)
		return nil
	})
	if gotErr != nil || got["state"] != "approved" || got["gate"] != "granted" || got["request"].(planRequest).ID != expected {
		t.Fatalf("approved submission retry = %#v, %v", got, gotErr)
	}
}
