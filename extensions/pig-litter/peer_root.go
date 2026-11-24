//go:build linux || darwin

package piglitter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

type localRuntime struct {
	peer    *localPeer
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	failure string
}

func localOptIn() (bool, bool, error) {
	mode, supplied := os.LookupEnv("PIG_LITTER_LOCAL_SESSIONS")
	if !supplied {
		path := filepath.Join(configuredAgentDir(), "local-sessions.json")
		data, err := readLocalFile(path, 4096)
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		if err != nil {
			return false, false, fmt.Errorf("local session configuration: %w", err)
		}
		var policy struct {
			Mode string `json:"mode"`
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if checkUniqueJSON(data) != nil || decoder.Decode(&policy) != nil {
			return false, false, errors.New("invalid local session configuration")
		}
		mode = policy.Mode
	}
	switch mode {
	case "", "off":
		return false, false, nil
	case "accept":
		return true, true, nil
	case "deny":
		return true, false, nil
	default:
		return false, false, errors.New("local session mode must be off, accept, or deny")
	}
}

func (o *owner) startLocal(accept bool) error {
	file, err := o.context.SessionManager().GetSessionFile()
	if err != nil || file == nil || *file == "" {
		return errors.Join(errors.New("local sessions require a retained native session file"), err)
	}
	host, err := o.context.HostProcessIdentity()
	if err != nil {
		return fmt.Errorf("local sessions require native host identity: %w", err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	name, err := localName(o.context, o.sessionID)
	if err != nil {
		return err
	}
	identity := localProcessIdentity{PID: host.PID, Birth: host.Birth, Scope: host.Scope}
	peer, err := startLocalPeer(localPeerOptions{
		RegistryDir: filepath.Join(cache, "pig-litter", "local-registry"),
		StateDir:    filepath.Join(cache, "pig-litter", "local-state"),
		Session:     ChildID(o.sessionID), SessionFile: *file, CWD: o.cwd,
		Name: name, AcceptInbound: accept, NativeSessionHost: &identity,
	})
	if err != nil {
		return err
	}
	idle, err := o.context.IsIdle()
	if err != nil {
		return errors.Join(err, peer.close())
	}
	activity := localActivityBusy
	if idle {
		activity = localActivityIdle
	}
	if err := peer.setLocalActivity(activity); err != nil {
		return errors.Join(err, peer.close())
	}
	ctx, cancel := context.WithCancel(o.tree.ctx)
	runtime := &localRuntime{peer: peer, cancel: cancel, done: make(chan struct{})}
	o.localMu.Lock()
	if o.localClosed || o.local != nil {
		o.localMu.Unlock()
		cancel()
		return errors.Join(errors.New("local session owner retired or already started"), peer.close())
	}
	o.local = runtime
	go runtime.run(ctx, o.context, o.sessionID)
	o.localMu.Unlock()
	return nil
}

func localName(ctx sdk.Context, session string) (string, error) {
	if !childUUID.MatchString(session) {
		return "", errors.New("invalid native session ID")
	}
	name, err := ctx.GetSessionName()
	if err != nil {
		return "", err
	}
	if name != nil && validateTeamName(*name) == nil {
		return *name, nil
	}
	if configured := os.Getenv("PIG_LITTER_LOCAL_NAME"); configured != "" {
		if err := validateTeamName(configured); err != nil {
			return "", fmt.Errorf("invalid PIG_LITTER_LOCAL_NAME: %w", err)
		}
		return configured, nil
	}
	return "session-" + strings.ReplaceAll(session, "-", "")[:12], nil
}

func (r *localRuntime) setFailure(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.failure = ""
	} else {
		r.failure = err.Error()
	}
}

func (r *localRuntime) run(ctx context.Context, host sdk.Context, session string) {
	defer close(r.done)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.setFailure(r.step(host, session))
		}
	}
}

func (r *localRuntime) step(host sdk.Context, session string) error {
	peer := r.peer
	name, err := localName(host, session)
	if err != nil {
		return err
	}
	peer.mu.Lock()
	if name != peer.name {
		peer.name = name
		err = peer.writePresence()
	}
	peer.mu.Unlock()
	if err != nil {
		return err
	}
	entries, err := boundedLocalStateEntries(peer.stateDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "inbox-") {
			continue
		}
		receipt, err := readLocalReceipt(filepath.Join(peer.stateDir, entry.Name()))
		if err != nil {
			return err
		}
		if receipt.State == peerAttempted || receipt.State == peerAmbiguous {
			_, _ = peer.observeLocal(receipt)
		}
	}
	receipt, found, err := peer.offerLocal()
	if err != nil || !found {
		return err
	}
	content, err := localMessageContent(receipt)
	if err != nil {
		return err
	}
	trigger := false
	err = host.SendCustomMessage(sdk.CustomMessage{
		CustomType: "pig_litter_local_peer", Content: content, Display: true,
		Details: map[string]any{"envelope": receipt.Envelope},
	}, sdk.SendMessageOptions{TriggerTurn: &trigger})
	if err != nil {
		return fmt.Errorf("local delivery is uncertain: %w", err)
	}
	_, _ = peer.observeLocal(receipt)
	return nil
}

func (o *owner) stopLocal() error {
	o.localMu.Lock()
	o.localClosed = true
	runtime := o.local
	o.localMu.Unlock()
	if runtime == nil {
		return nil
	}
	runtime.cancel()
	<-runtime.done
	return runtime.peer.close()
}

func (o *owner) localRuntime() *localRuntime {
	o.localMu.Lock()
	defer o.localMu.Unlock()
	return o.local
}

func (o *owner) setLocalBusy(busy bool) error {
	runtime := o.localRuntime()
	if runtime == nil {
		return nil
	}
	if busy {
		return runtime.peer.setLocalActivity(localActivityBusy)
	}
	return runtime.peer.setLocalActivity(localActivityIdle)
}

func (o *owner) localDiscovery(cursor string) map[string]any {
	runtime := o.localRuntime()
	if runtime == nil {
		return map[string]any{"state": "disabled", "sessions": []localDiscoveryRow{}}
	}
	page, err := runtime.peer.discover(cursor)
	if err != nil {
		return map[string]any{"state": "unavailable", "report": err.Error()}
	}
	state := "listed"
	runtime.mu.Lock()
	failure := runtime.failure
	runtime.mu.Unlock()
	if page.Truncated || len(page.Issues) != 0 || failure != "" {
		state = "degraded"
	}
	o.tree.mu.Lock()
	rows := make([]map[string]any, 0, len(page.Rows))
	for _, peer := range page.Rows {
		collisions := []string{}
		for _, record := range o.tree.records {
			if record.parent != o.tree.root || !strings.EqualFold(record.name, peer.Name) {
				continue
			}
			kind := "child"
			if record.team != nil {
				kind = "team"
			}
			collisions = append(collisions, kind)
		}
		rows = append(rows, map[string]any{"kind": "independent_session", "name": peer.Name, "ref": peer.Ref.String(), "acceptInbound": peer.AcceptInbound, "collisions": collisions})
	}
	o.tree.mu.Unlock()
	return map[string]any{"state": state, "sessions": rows, "issues": page.Issues, "truncated": page.Truncated, "next": page.Next, "report": failure, "self": runtime.peer.ref.String()}
}

func (o *owner) localTerminalList() string {
	view := o.localDiscovery("")
	state, _ := view["state"].(string)
	lines := []string{"Independent local sessions: " + state}
	rows, _ := view["sessions"].([]map[string]any)
	for _, row := range rows {
		accept := "inbound denied"
		if row["acceptInbound"] == true {
			accept = "inbound accepted"
		}
		lines = append(lines, fmt.Sprintf("%s | %s | %s", row["name"], row["ref"], accept))
		if collisions, ok := row["collisions"].([]string); ok && len(collisions) > 0 {
			lines = append(lines, "Name collision: "+strings.Join(collisions, ", "))
		}
	}
	if report, _ := view["report"].(string); report != "" {
		lines = append(lines, report)
	}
	if issues, _ := view["issues"].([]localDiscoveryIssue); len(issues) > 0 {
		lines = append(lines, fmt.Sprintf("Discovery incomplete: %d unreadable or unavailable records", len(issues)))
	}
	if view["truncated"] == true || view["next"] != "" && view["next"] != nil {
		lines = append(lines, "More sessions exist; use team_list with the returned cursor.")
	}
	lines = append(lines, "Cloud, Remote Control, and Desktop routes are unsupported.")
	return paintText(strings.Join(lines, "\n"), true)
}

func (o *owner) localNameCollision(name string) (bool, bool) {
	runtime := o.localRuntime()
	if runtime == nil {
		return false, false
	}
	runtime.peer.mu.Lock()
	snapshot, err := runtime.peer.scanPresence(time.Now())
	runtime.peer.mu.Unlock()
	degraded := err != nil || snapshot.truncated || len(snapshot.issues) != 0
	for _, row := range snapshot.rows {
		if strings.EqualFold(row.Name, name) {
			return true, degraded
		}
	}
	return false, degraded
}

func (o *owner) resolveLocal(address string) (localSessionRef, error) {
	runtime := o.localRuntime()
	if runtime == nil {
		return localSessionRef{}, reject("unavailable", "local sessions are disabled")
	}
	name := strings.TrimPrefix(address, "local:")
	if strings.Contains(name, "@") {
		if !strings.HasPrefix(address, "local:") {
			return localSessionRef{}, reject("rejected", "exact local session pins require the local: prefix")
		}
		ref, err := parseLocalSessionRef("local:" + name)
		if err != nil {
			return localSessionRef{}, reject("rejected", "invalid local session pin")
		}
		return ref, nil
	}
	if err := validateTeamName(name); err != nil {
		return localSessionRef{}, err
	}
	var matches []localSessionRef
	cursor := ""
	for {
		page, err := runtime.peer.discover(cursor)
		if err != nil || page.Truncated || len(page.Issues) != 0 {
			return localSessionRef{}, reject("unavailable", "local discovery is incomplete")
		}
		for _, row := range page.Rows {
			if strings.EqualFold(row.Name, name) {
				matches = append(matches, row.Ref)
			}
		}
		cursor = page.Next
		if cursor == "" {
			break
		}
	}
	if len(matches) == 0 {
		return localSessionRef{}, reject("unavailable", "local session name was not found")
	}
	if len(matches) > 1 {
		return localSessionRef{}, reject("ambiguous", "local session name matches more than one session; use an exact pin")
	}
	return matches[0], nil
}

func unsupportedPeerRoute(address string) bool {
	class, _, prefixed := strings.Cut(address, ":")
	if !prefixed {
		return false
	}
	switch class {
	case "cloud", "remote", "remote-control", "remote_control", "desktop":
		return true
	default:
		return false
	}
}

func localResultValue(result localResult, err error, ref localSessionRef) map[string]any {
	if err != nil {
		switch {
		case errors.Is(err, errLocalStale):
			result.Status = "stale_identity"
		case errors.Is(err, errLocalUncertain):
			result.Status = "uncertain"
		case errors.Is(err, errLocalUnreachable):
			result.Status = "unreachable"
		default:
			if result.Status == "" {
				result.Status = "invalid"
			}
		}
	}
	value := map[string]any{"state": result.Status, "recipient": ref.String()}
	if result.Receipt != nil {
		value["receipt"] = result.Receipt
	}
	if result.ExitMonitoring != "" {
		value["exitMonitoring"] = result.ExitMonitoring
	}
	if err != nil {
		value["report"] = err.Error()
	}
	return value
}

func (o *owner) localOperation(ctx context.Context, operation string, raw json.RawMessage) (map[string]any, error) {
	var keys []string
	switch operation {
	case "team_send":
		keys = []string{"recipient", "messageId", "text", "summary"}
	case "team_peer_receipt":
		keys = []string{"recipient", "messageId"}
	case "team_subscribe":
		keys = []string{"recipient", "subscriptionId", "expiresAt"}
	default:
		return nil, reject("rejected", "unknown local session operation")
	}
	if _, err := exactJSONObject(raw, keys...); err != nil {
		return nil, reject("rejected", "invalid local session arguments")
	}
	var args struct {
		Recipient      string `json:"recipient"`
		MessageID      string `json:"messageId"`
		Text           string `json:"text"`
		Summary        string `json:"summary"`
		SubscriptionID string `json:"subscriptionId"`
		ExpiresAt      string `json:"expiresAt"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if checkUniqueJSON(raw) != nil || decoder.Decode(&args) != nil {
		return nil, reject("rejected", "invalid local session arguments")
	}
	if unsupportedPeerRoute(args.Recipient) {
		return map[string]any{"state": "unsupported", "recipient": args.Recipient}, nil
	}
	if operation == "team_peer_receipt" && !strings.Contains(args.Recipient, "@") {
		return nil, reject("rejected", "receipt queries require an exact local session pin")
	}
	ref, err := o.resolveLocal(args.Recipient)
	if err != nil {
		return nil, err
	}
	var result localResult
	runtime := o.localRuntime()
	if runtime == nil {
		return nil, reject("unavailable", "local sessions are disabled")
	}
	switch operation {
	case "team_send":
		result, err = runtime.peer.send(ctx, ref, localMessage{ID: args.MessageID, Text: args.Text, Summary: args.Summary})
	case "team_peer_receipt":
		result, err = runtime.peer.receipt(ctx, ref, args.MessageID)
	case "team_subscribe":
		expires, parseErr := time.Parse(time.RFC3339Nano, args.ExpiresAt)
		if parseErr != nil {
			return nil, reject("rejected", "subscription requires an RFC3339 expiry")
		}
		result, err = runtime.peer.subscribe(ctx, ref, args.SubscriptionID, expires)
	}
	value := localResultValue(result, err, ref)
	if presence, lookupErr := runtime.peer.lookup(ref); lookupErr == nil {
		collisions := []string{}
		o.tree.mu.Lock()
		for _, record := range o.tree.records {
			if record.parent != o.tree.root || !strings.EqualFold(record.name, presence.Name) {
				continue
			}
			if record.team == nil {
				collisions = append(collisions, "child")
			} else {
				collisions = append(collisions, "team")
			}
		}
		o.tree.mu.Unlock()
		value["collisions"] = collisions
	}
	return value, nil
}

func (o *owner) addressedSend(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var request peerRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if checkUniqueJSON(raw) != nil || decoder.Decode(&request) != nil {
		return nil, reject("rejected", "invalid message arguments")
	}
	original := request.Recipient
	if len(request.Text) > 16384 || len(request.Summary) > 256 {
		return map[string]any{"state": "too_large", "recipient": original}, nil
	}
	request.Recipient = "local"
	checked, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if _, err := parsePeerRequest(checked); err != nil {
		return nil, err
	}
	if strings.HasPrefix(original, "local:") || unsupportedPeerRoute(original) {
		return o.localOperation(ctx, "team_send", raw)
	}
	class, name, prefixed := strings.Cut(original, ":")
	if !prefixed {
		name, class = original, ""
	} else if class != "child" && class != "team" {
		return nil, reject("rejected", "unknown recipient class")
	}
	if strings.TrimSpace(name) == "" {
		return nil, reject("rejected", "recipient name is required")
	}
	o.tree.mu.Lock()
	var child, member []*run
	members := make(map[*run]teamMember)
	for _, record := range o.tree.records {
		if record.parent != o.tree.root || !strings.EqualFold(record.name, name) {
			continue
		}
		if record.team == nil {
			child = append(child, record.run)
		} else {
			member = append(member, record.run)
			members[record.run] = *record.team
		}
	}
	o.tree.mu.Unlock()
	var matches []*run
	switch class {
	case "child":
		matches = child
	case "team":
		matches = member
	default:
		matches = child
		if len(matches) == 0 {
			matches = member
		}
	}
	if len(matches) > 1 {
		return nil, reject("ambiguous", "recipient name matches more than one session")
	}
	if len(matches) == 1 {
		if matches[0].owner.role == "observer" {
			return nil, reject("permission_denied", "observers do not accept peer messages")
		}
		localCollision, discoveryDegraded := o.localNameCollision(name)
		collisions := []string{}
		if localCollision {
			collisions = append(collisions, "local")
		}
		if target, ok := members[matches[0]]; ok {
			if len(child) > 0 {
				collisions = append(collisions, "child")
			}
			o.teamMu.Lock()
			store := o.teamStore
			o.teamMu.Unlock()
			if store == nil {
				return nil, reject("unavailable", "team store is unavailable")
			}
			request.Recipient = target.Name
			lead := MemberRef{Team: target.Ref.Team, ID: target.Ref.Team.Root, Generation: 1}
			o.tree.lifecycleMu.Lock()
			receipt, _, err := store.acceptPeerTo(lead, request, &target.Ref)
			o.tree.lifecycleMu.Unlock()
			if err != nil {
				return nil, err
			}
			if receipt.State == peerAccepted {
				o.requestPeerWake(receipt.Envelope.Recipient)
			}
			delivery := string(receipt.State)
			if receipt.State == peerObserved {
				delivery = "unverified"
			}
			return map[string]any{"state": "accepted", "messageId": request.MessageID, "delivery": delivery, "recipientClass": "team", "collisions": collisions, "discoveryDegraded": discoveryDegraded}, nil
		}
		if len(member) > 0 {
			collisions = append(collisions, "team")
		}
		snapshot, _, err := o.tree.message(ctx, nil, matches[0], "steer", request.Text)
		if err != nil {
			return nil, err
		}
		return map[string]any{"state": "queued", "child": snapshot, "recipientClass": "child", "collisions": collisions, "discoveryDegraded": discoveryDegraded}, nil
	}
	if prefixed {
		return nil, reject("unavailable", "recipient name was not found")
	}
	return o.localOperation(ctx, "team_send", raw)
}
