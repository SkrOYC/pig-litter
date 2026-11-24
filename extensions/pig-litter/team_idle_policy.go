package piglitter

import (
	"slices"
	"strings"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

type idlePolicyCandidate struct {
	Kind      string         `json:"kind"`
	Member    MemberRef      `json:"member"`
	Execution string         `json:"execution"`
	Report    string         `json:"report"`
	Tasks     []teamTaskView `json:"tasks"`
	Revision  uint64         `json:"revision"`
}

func idleReport(event *extension.AgentBeforeSettleEvent) string {
	for index := len(event.Context.ContextMessages) - 1; index >= 0; index-- {
		message := event.Context.ContextMessages[index].Assistant
		if message == nil {
			continue
		}
		var text strings.Builder
		for _, block := range message.Content {
			if value, ok := block.(ai.TextContent); ok {
				part, clipped := bounded(value.Text, 4096-text.Len())
				text.WriteString(part)
				if clipped || text.Len() == 4096 {
					break
				}
			}
		}
		return text.String()
	}
	return ""
}

func (o *owner) idleCandidate(r *record, selected *run, report string) (idlePolicyCandidate, uint64, error) {
	var candidate idlePolicyCandidate
	if err := o.current(); err != nil {
		return candidate, 0, err
	}
	o.tree.mu.Lock()
	if o.tree.closed || o.tree.records[r.id] != r || r.run != selected || r.team == nil || r.paused || selected.ctx.Err() != nil || r.controlPhase == "closing" || r.controlPhase == "terminated" || r.controlPhase == "recovery_pending" {
		o.tree.mu.Unlock()
		return candidate, 0, reject("unavailable", "teammate execution changed before idle")
	}
	candidate = idlePolicyCandidate{Kind: "teammate_idle", Member: r.team.Ref, Execution: selected.noticeID, Report: report}
	wake := r.peerWakeVersion
	o.tree.mu.Unlock()
	store, err := o.teamStoreForRoot()
	if err != nil {
		return candidate, wake, err
	}
	candidate.Tasks, candidate.Revision, err = store.memberTasks(candidate.Member)
	return candidate, wake, err
}

func (o *owner) idlePolicyHooks(r *record, selected *run) extension.Extension {
	return extension.Extension{Name: "pig-litter-idle-policy", Handlers: map[string][]extension.HandlerFn{
		"agent_before_settle": {func(args ...any) (any, error) {
			event := args[0].(*extension.AgentBeforeSettleEvent)
			if event.Outcome != extension.AgentActivityCompleted || event.Continue {
				return nil, nil
			}
			o.tree.lifecycleMu.Lock()
			candidate, wake, err := o.idleCandidate(r, selected, idleReport(event))
			o.tree.lifecycleMu.Unlock()
			if err != nil {
				return o.idlePolicyFeedback(r, selected, event, "The current team work could not be checked."), nil
			}
			permit, feedback := runTeamPolicy(selected.ctx, o.policy.TeammateIdle, candidate)
			o.tree.lifecycleMu.Lock()
			current, currentWake, err := o.idleCandidate(r, selected, candidate.Report)
			if err != nil {
				o.tree.lifecycleMu.Unlock()
				return nil, nil
			}
			if current.Member != candidate.Member || current.Execution != candidate.Execution || current.Revision != candidate.Revision || currentWake != wake {
				permit = false
				feedback = "Team work changed while the idle policy ran. Review the current work before settling."
			}
			if permit {
				o.tree.mu.Lock()
				selected.idleRefusal = ""
				o.tree.mu.Unlock()
				o.tree.lifecycleMu.Unlock()
				return nil, nil
			}
			result := o.idlePolicyFeedback(r, selected, event, feedback)
			o.tree.lifecycleMu.Unlock()
			return result, nil
		}},
	}}
}

func (o *owner) idlePolicyFeedback(r *record, selected *run, event *extension.AgentBeforeSettleEvent, feedback string) any {
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	if o.tree.closed || o.tree.records[r.id] != r || r.run != selected || r.paused || selected.ctx.Err() != nil || r.controlPhase == "closing" || r.controlPhase == "terminated" || r.controlPhase == "recovery_pending" {
		return nil
	}
	selected.idleRefusal = feedback
	if selected.progress.turns >= o.config.MaxTurns {
		return nil
	}
	entries := slices.Clone(event.Entries)
	entries = append(entries, extension.SessionBoundaryDraft{Type: extension.SessionBoundaryDraftCustomMessage, CustomType: "pig_litter_idle_policy", Content: "The teammate idle policy requires further work. " + feedback, Display: true})
	continuation := true
	return extension.BoundaryResult{Continue: &continuation, Entries: &entries}
}
