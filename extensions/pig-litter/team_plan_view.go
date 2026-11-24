package piglitter

import (
	"fmt"
	"slices"
	"time"
)

func teamMemberName(frame *teamViewFrame, ref MemberRef) string {
	if index := slices.IndexFunc(frame.Members, func(member teamMemberView) bool { return member.Member.Ref == ref }); index >= 0 {
		return frame.Members[index].Member.Name
	}
	return string(ref.ID)
}

func teamPlanSummary(frame *teamViewFrame, plan planView) string {
	return fmt.Sprintf("#%d %s | %s | %s", plan.Request.ID.Sequence, plan.Request.State, teamMemberName(frame, plan.Request.ID.Member), plan.Request.Text)
}

func teamPlanDetail(frame *teamViewFrame, plan planView) []string {
	request := plan.Request
	lines := []string{
		fmt.Sprintf("Plan #%d | state %s | gate %s", request.ID.Sequence, request.State, plan.Gate),
		"Member " + teamMemberName(frame, request.ID.Member),
		fmt.Sprintf("Member UUID %s | generation %d", request.ID.Member.ID, request.ID.Member.Generation),
		"Deadline " + request.Deadline.UTC().Format(time.RFC3339),
		"Plan " + request.Text,
	}
	if request.Reason != "" {
		lines = append(lines, "Feedback "+request.Reason)
	}
	return lines
}
