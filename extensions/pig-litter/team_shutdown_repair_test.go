package piglitter

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestShutdownRequestWakesAnIdleTeammate(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "worker", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	_, previous, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: AgentDefinition{Role: "worker", Tools: []string{"read"}}, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	previous.owner.admitted = true
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		tree.teamFinished = o.finishTeamMember
		tree.finish(previous.owner, previous, Outcome{State: Completed, Text: "Ready."})
		tree.teamWG.Wait()
		started, release := make(chan *run, 1), make(chan struct{})
		defer func() { close(release); tree.close() }()
		tree.execute = func(_ *record, selected *run) Outcome {
			started <- selected
			<-release
			return Outcome{State: Completed}
		}
		value, err := o.applyShutdown(controlAuthority{human: func() error { return nil }}, shutdownCommand{Action: "request", Member: member.Ref, SubmissionID: "idle-request", Reason: "Please stop", TimeoutMs: 60000})
		if err != nil || value["state"] != "pending" {
			exerciseErr = fmt.Errorf("idle request: %#v %v", value, err)
			return
		}
		select {
		case selected := <-started:
			if selected == previous || selected.ref.ID != previous.ref.ID || selected.owner.team.Ref != member.Ref || selected.peerWake == nil || selected.peerWake.Control == nil || selected.peerWake.Control.Request.Member != member.Ref {
				exerciseErr = fmt.Errorf("unexpected idle continuation: %#v", selected)
			}
		case <-time.After(time.Second):
			lifecycle, _ := store.readLifecycle(member.Ref)
			exerciseErr = fmt.Errorf("idle request did not start a recipient turn: %#v", lifecycle)
		}
	}, shutdownRepairHost)
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}

func TestFailedApprovedShutdownReportsTheFailure(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "worker", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: AgentDefinition{Role: "worker", Tools: []string{"read"}}, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	selected.owner.admitted = true
	request, _, _, err := store.requestShutdown(member.Ref, "failed-batch", "Please stop", 60000)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.respondShutdown(request.ID, selected.noticeID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetState(member.Ref, "closing"); err != nil {
		t.Fatal(err)
	}
	selected.shutdown = &shutdownFence{Request: request.ID, Drained: true}
	selected.owner.controlPhase = "closing"
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		tree.teamFinished, tree.teamSettled = o.finishTeamMember, o.settleShutdown
		tree.finish(selected.owner, selected, Outcome{State: Partial, Text: "The work failed.", Error: "tool bash failed: exit status 37"})
		tree.teamWG.Wait()
		o.settleShutdown(selected.owner, selected)
		tree.teamWG.Wait()
		lifecycle, err := store.readLifecycle(member.Ref)
		if err != nil || lifecycle.Phase != "recovery_pending" || len(lifecycle.Turns) != 1 || lifecycle.Turns[0].Kind != "failed" || lifecycle.Turns[0].Error != "tool bash failed: exit status 37" {
			exerciseErr = fmt.Errorf("failed shutdown did not retain a failure notice: %#v %v", lifecycle, err)
		}
	}, shutdownRepairHost)
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}

func TestShutdownRequestLeavesAnExplicitlyPausedTeammatePaused(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "worker", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	_, previous, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: AgentDefinition{Role: "worker", Tools: []string{"read"}}, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	previous.owner.admitted = true
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		tree.teamFinished, tree.teamPaused = o.finishTeamMember, o.pauseTeamMember
		tree.finish(previous.owner, previous, Outcome{State: Completed, Text: "Ready."})
		tree.teamWG.Wait()
		if _, _, err := tree.stop(nil, previous); err != nil {
			exerciseErr = err
			return
		}
		value, err := o.applyShutdown(controlAuthority{human: func() error { return nil }}, shutdownCommand{Action: "request", Member: member.Ref, SubmissionID: "paused-request", Reason: "Please stop", TimeoutMs: 60000})
		if err != nil || value["state"] != "pending" {
			exerciseErr = fmt.Errorf("paused request: %#v %v", value, err)
			return
		}
		tree.peerWakeWG.Wait()
		tree.teamWG.Wait()
		_, members, err := store.Snapshot()
		if err != nil || len(members) != 1 || members[0].State != "paused" || previous.owner.run != previous || !previous.owner.paused {
			exerciseErr = fmt.Errorf("shutdown changed the paused teammate: %#v %v", members, err)
		}
	}, shutdownRepairHost)
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}

func TestCancelledShutdownDoesNotWakeAfterTheTurnFinishes(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "worker", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	_, previous, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: AgentDefinition{Role: "worker", Tools: []string{"read"}}, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	previous.owner.admitted = true
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		tree.teamFinished = o.finishTeamMember
		human := controlAuthority{human: func() error { return nil }}
		value, err := o.applyShutdown(human, shutdownCommand{Action: "request", Member: member.Ref, SubmissionID: "cancel-before-idle", Reason: "Please stop", TimeoutMs: 60000})
		if err != nil {
			exerciseErr = err
			return
		}
		request := value["request"].(shutdownRequest)
		if _, err := o.applyShutdown(human, shutdownCommand{Action: "cancel", RequestID: request.ID}); err != nil {
			exerciseErr = err
			return
		}
		tree.finish(previous.owner, previous, Outcome{State: Completed, Text: "Finished."})
		tree.teamWG.Wait()
		tree.peerWakeWG.Wait()
		lifecycle, err := store.readLifecycle(member.Ref)
		if err != nil || lifecycle.Requests[0].State != "cancelled" || previous.owner.run != previous {
			exerciseErr = fmt.Errorf("cancelled request woke the teammate: %#v %v", lifecycle, err)
		}
	}, shutdownRepairHost)
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}

func TestShutdownRequestWakesWhenCapacityReturns(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "worker", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	config := defaultConfig()
	config.Concurrency = 1
	tree := testTree(t, config)
	tree.root.id = root
	_, previous, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: AgentDefinition{Role: "worker", Tools: []string{"read"}}, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	previous.owner.admitted = true
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		tree.teamFinished = o.finishTeamMember
		tree.finish(previous.owner, previous, Outcome{State: Completed, Text: "Ready."})
		tree.teamWG.Wait()
		_, other, err := tree.admit(admission{kind: "scout", name: "other", cwd: workspace.Cwd, task: "inspect", agent: AgentDefinition{Role: "scout", Tools: []string{"read"}}})
		if err != nil {
			exerciseErr = err
			return
		}
		started, release := make(chan *run, 1), make(chan struct{})
		defer func() { close(release); tree.close() }()
		tree.execute = func(_ *record, selected *run) Outcome {
			started <- selected
			<-release
			return Outcome{State: Completed}
		}
		human := controlAuthority{human: func() error { return nil }}
		command := shutdownCommand{Action: "request", Member: member.Ref, SubmissionID: "capacity-retry", Reason: "Please stop", TimeoutMs: 60000}
		first, err := o.applyShutdown(human, command)
		if err != nil {
			exerciseErr = err
			return
		}
		retry, err := o.applyShutdown(human, command)
		if err != nil || retry["request"].(shutdownRequest).ID != first["request"].(shutdownRequest).ID {
			exerciseErr = fmt.Errorf("shutdown retry: %#v %v", retry, err)
			return
		}
		select {
		case <-started:
			exerciseErr = fmt.Errorf("teammate woke while capacity was full")
			return
		case <-time.After(100 * time.Millisecond):
		}
		tree.finish(other.owner, other, Outcome{State: Completed})
		select {
		case selected := <-started:
			if selected.ref.ID != previous.ref.ID {
				exerciseErr = fmt.Errorf("retry woke the wrong teammate")
			}
		case <-time.After(time.Second):
			exerciseErr = fmt.Errorf("shutdown did not wake after capacity returned")
		}
	}, shutdownRepairHost)
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}

func shutdownRepairHost(method string, raw json.RawMessage) json.RawMessage {
	if method == "sessionRead" {
		var query struct{ Method string }
		_ = json.Unmarshal(raw, &query)
		if query.Method == "getEntries" {
			return json.RawMessage(`[]`)
		}
		return json.RawMessage(`"fixture"`)
	}
	return json.RawMessage(`{}`)
}

func TestShutdownRequestWakesWhenEitherCapacitySlotReturns(t *testing.T) {
	for _, release := range []string{"finish", "cancel"} {
		for _, first := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/blocker-%d", release, first+1), func(t *testing.T) {
				store, _, root, workspace := storeTestOpen(t)
				member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "worker", workspace, false)
				if err != nil {
					t.Fatal(err)
				}
				member, err = store.Commit(member.AdmissionID)
				if err != nil {
					t.Fatal(err)
				}
				config := defaultConfig()
				config.Concurrency = 2
				tree := testTree(t, config)
				tree.root.id = root
				_, previous, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: AgentDefinition{Role: "worker", Tools: []string{"read"}}, member: &member})
				if err != nil {
					t.Fatal(err)
				}
				previous.owner.admitted = true
				var exerciseErr error
				withCompletionHost(t, tree, func(o *owner) {
					o.teamStore = store
					tree.teamFinished = o.finishTeamMember
					tree.finish(previous.owner, previous, Outcome{State: Completed, Text: "Ready."})
					tree.teamWG.Wait()
					select {
					case <-previous.teamFinishDone:
					default:
						exerciseErr = fmt.Errorf("target roster settlement did not finish")
						return
					}
					blockers := make([]*run, 2)
					for i := range blockers {
						_, blockers[i], err = tree.admit(admission{kind: "scout", name: fmt.Sprintf("blocker-%d", i+1), cwd: workspace.Cwd, task: "inspect", agent: AgentDefinition{Role: "scout", Tools: []string{"read"}}})
						if err != nil {
							exerciseErr = err
							return
						}
					}
					started, allowFinish := make(chan *run, 1), make(chan struct{})
					defer func() { close(allowFinish); tree.close() }()
					tree.execute = func(_ *record, selected *run) Outcome {
						started <- selected
						<-allowFinish
						return Outcome{State: Completed}
					}
					value, err := o.applyShutdown(controlAuthority{human: func() error { return nil }}, shutdownCommand{Action: "request", Member: member.Ref, SubmissionID: "two-slot-retry", Reason: "Please stop", TimeoutMs: 60000})
					if err != nil {
						exerciseErr = err
						return
					}
					request := value["request"].(shutdownRequest)
					waitForPeerWakeWaiter(t, o)
					if release == "finish" {
						tree.finish(blockers[first].owner, blockers[first], Outcome{State: Completed})
					} else {
						tree.cancelPending(blockers[first])
					}
					select {
					case selected := <-started:
						if selected.ref.ID != member.Ref.ID || selected.peerWake == nil || selected.peerWake.Control == nil || selected.peerWake.Control.Request != request.ID {
							exerciseErr = fmt.Errorf("wrong wake identity: %#v", selected.peerWake)
						}
						select {
						case <-blockers[1-first].done:
							exerciseErr = fmt.Errorf("the other blocker settled before the teammate woke")
						default:
						}
					case <-time.After(time.Second):
						exerciseErr = fmt.Errorf("shutdown did not wake after %s of blocker %d while the other blocker remained live", release, first+1)
					}
				}, shutdownRepairHost)
				if exerciseErr != nil {
					t.Fatal(exerciseErr)
				}
			})
		}
	}
}

func TestBlockedShutdownRechecksLifecycleBeforeWaking(t *testing.T) {
	for _, action := range []string{"cancel", "pause", "expire", "close"} {
		t.Run(action, func(t *testing.T) {
			store, _, root, workspace := storeTestOpen(t)
			member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "worker", workspace, false)
			if err != nil {
				t.Fatal(err)
			}
			member, err = store.Commit(member.AdmissionID)
			if err != nil {
				t.Fatal(err)
			}
			config := defaultConfig()
			config.Concurrency = 2
			tree := testTree(t, config)
			tree.root.id = root
			_, previous, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: AgentDefinition{Role: "worker", Tools: []string{"read"}}, member: &member})
			if err != nil {
				t.Fatal(err)
			}
			previous.owner.admitted = true
			var exerciseErr error
			withCompletionHost(t, tree, func(o *owner) {
				o.teamStore = store
				tree.teamFinished, tree.teamPaused = o.finishTeamMember, o.pauseTeamMember
				tree.finish(previous.owner, previous, Outcome{State: Completed, Text: "Ready."})
				tree.teamWG.Wait()
				blockers := make([]*run, 2)
				for i := range blockers {
					_, blockers[i], err = tree.admit(admission{kind: "scout", name: fmt.Sprintf("blocker-%d", i+1), cwd: workspace.Cwd, task: "inspect", agent: AgentDefinition{Role: "scout", Tools: []string{"read"}}})
					if err != nil {
						exerciseErr = err
						return
					}
				}
				started := make(chan *run, 1)
				tree.execute = func(_ *record, selected *run) Outcome {
					started <- selected
					return Outcome{State: Completed}
				}
				human := controlAuthority{human: func() error { return nil }}
				timeoutMs := 60000
				if action == "expire" {
					timeoutMs = 1000
				}
				value, err := o.applyShutdown(human, shutdownCommand{Action: "request", Member: member.Ref, SubmissionID: "blocked-lifecycle", Reason: "Please stop", TimeoutMs: timeoutMs})
				if err != nil {
					exerciseErr = err
					return
				}
				request := value["request"].(shutdownRequest)
				waitForPeerWakeWaiter(t, o)
				switch action {
				case "cancel":
					_, err = o.applyShutdown(human, shutdownCommand{Action: "cancel", RequestID: request.ID})
				case "pause":
					_, _, err = tree.stop(nil, previous)
				case "expire":
					deadline := time.Now().Add(3 * time.Second)
					for time.Now().Before(deadline) {
						var lifecycle memberLifecycle
						lifecycle, err = store.readLifecycle(member.Ref)
						if err != nil || lifecycle.Requests[0].State == "expired" {
							break
						}
						time.Sleep(10 * time.Millisecond)
					}
				case "close":
					tree.close()
				}
				if err != nil {
					exerciseErr = err
					return
				}
				if action != "close" {
					tree.finish(blockers[1].owner, blockers[1], Outcome{State: Completed})
				}
				finished := make(chan struct{})
				go func() { tree.peerWakeWG.Wait(); close(finished) }()
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					exerciseErr = fmt.Errorf("wake driver did not stop after %s", action)
					return
				}
				select {
				case selected := <-started:
					exerciseErr = fmt.Errorf("%s request woke member %s", action, selected.ref.ID)
				default:
				}
				lifecycle, err := store.readLifecycle(member.Ref)
				if err != nil {
					exerciseErr = err
					return
				}
				want := "pending"
				if action == "cancel" {
					want = "cancelled"
				} else if action == "expire" {
					want = "expired"
				}
				if lifecycle.Requests[0].ID != request.ID || lifecycle.Requests[0].State != want {
					exerciseErr = fmt.Errorf("request identity or state changed after %s: %#v", action, lifecycle.Requests)
				}
				if action == "pause" {
					_, members, err := store.Snapshot()
					if err != nil || len(members) != 1 || members[0].State != "paused" {
						exerciseErr = fmt.Errorf("teammate did not remain paused: %#v %v", members, err)
					}
				}
			}, shutdownRepairHost)
			if exerciseErr != nil {
				t.Fatal(exerciseErr)
			}
		})
	}
}
