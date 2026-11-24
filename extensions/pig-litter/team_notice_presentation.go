package piglitter

import (
	"encoding/json"
	"fmt"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func teamNoticeName(state *extensionState, ref MemberRef) string {
	if !validControlRef(ref) {
		return "unknown teammate"
	}
	state.mu.Lock()
	o := state.owner
	active := o != nil && state.epoch == o.epoch
	state.mu.Unlock()
	if active && o.tree != nil {
		o.tree.mu.Lock()
		member := o.tree.records[ref.ID]
		if member != nil && member.team != nil && member.team.Ref == ref {
			name := member.name
			o.tree.mu.Unlock()
			return name
		}
		o.tree.mu.Unlock()
	}
	id := string(ref.ID)
	return "#" + id[len(id)-8:]
}

func teamTurnNoticeRenderer(state *extensionState) sdk.RendererFunc {
	return func(_ sdk.Context, message map[string]any, _ sdk.MessageRenderOptions, width int) ([]string, error) {
		content, ok := message["content"].(string)
		var notice turnNotice
		if !ok || len(content) > paintByteLimit || json.Unmarshal([]byte(content), &notice) != nil || !validControlRef(notice.Member) {
			return toolPaintRows([]string{"Teammate update unavailable"}, "", width), nil
		}
		switch notice.Kind {
		case "available", "interrupted", "failed":
		default:
			return toolPaintRows([]string{"Teammate update unavailable"}, "", width), nil
		}
		metadata := []string{fmt.Sprintf("Teammate %s | %s", teamNoticeName(state, notice.Member), notice.Kind)}
		if notice.Error != "" {
			metadata = append(metadata, "Error "+notice.Error)
		}
		if notice.Report != "" {
			metadata = append(metadata, "Report")
		}
		return toolPaintRows(metadata, notice.Report, width), nil
	}
}

func teamControlNoticeRenderer(state *extensionState) sdk.RendererFunc {
	return func(_ sdk.Context, message map[string]any, _ sdk.MessageRenderOptions, width int) ([]string, error) {
		content, ok := message["content"].(string)
		var notice struct {
			Kind      string            `json:"kind"`
			RequestID ShutdownRequestID `json:"requestId"`
			Message   string            `json:"message"`
		}
		if !ok || len(content) > paintByteLimit || json.Unmarshal([]byte(content), &notice) != nil || !validControlRef(notice.RequestID.Member) {
			return toolPaintRows([]string{"Team control update unavailable"}, "", width), nil
		}
		label := "team control update"
		switch notice.Kind {
		case "shutdown_request":
			label = "shutdown requested"
		case "shutdown_rejected":
			label = "shutdown declined"
		case "shutdown_approved":
			label = "shutdown accepted"
		case "shutdown_cancelled":
			label = "shutdown request cancelled"
		case "shutdown_terminated":
			label = "shutdown complete"
		}
		metadata := []string{fmt.Sprintf("Teammate %s | %s", teamNoticeName(state, notice.RequestID.Member), label)}
		return toolPaintRows(metadata, notice.Message, width), nil
	}
}
