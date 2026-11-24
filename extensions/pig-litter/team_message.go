package piglitter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

const peerInboxLimit = 32

func teamLeadRef(team TeamID) MemberRef {
	return MemberRef{Team: team, ID: team.Root, Generation: 1}
}

func validPeerDeliveryAttempt(attempt peerDeliveryAttempt) bool {
	return attempt.Host.PID > 0 && attempt.Host.Birth != "" && len(attempt.Host.Birth) <= 128 && attempt.Host.Scope != "" && len(attempt.Host.Scope) <= 256 && filepath.IsAbs(attempt.SessionFile) && filepath.Clean(attempt.SessionFile) == attempt.SessionFile
}

func peerKey(envelope peerEnvelope) (string, error) {
	data, err := json.Marshal(struct {
		Team      TeamID    `json:"team"`
		Sender    MemberRef `json:"sender"`
		Recipient MemberRef `json:"recipient"`
		MessageID string    `json:"messageId"`
	}{envelope.Team, envelope.Sender, envelope.Recipient, envelope.MessageID})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func peerPaths(dir, key string) (string, string) {
	return filepath.Join(dir, "peer-body-"+key+".json"), filepath.Join(dir, "peer-receipt-"+key+".json")
}

func readPeerFile(path string) ([]byte, error) {
	if err := checkMetadataFile(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if err := errors.Join(statErr, pathErr); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return nil, errors.Join(errors.New("peer metadata file was replaced"), file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, teamJSONLimit+1))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(readErr, syncErr, closeErr); err != nil {
		return nil, err
	}
	if len(data) > teamJSONLimit {
		return nil, errors.New("peer metadata exceeds size limit")
	}
	return data, nil
}

func readPeerReceipt(path string) (peerReceipt, error) {
	var receipt peerReceipt
	data, err := readPeerFile(path)
	if err != nil {
		return receipt, err
	}
	if err := checkUniqueJSON(data); err != nil {
		return receipt, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, err
	}
	switch receipt.State {
	case peerAccepted, peerAttempted, peerObserved, peerAmbiguous:
	default:
		return receipt, errors.New("invalid peer receipt state")
	}
	envelope := receipt.Envelope
	if !childUUID.MatchString(string(envelope.Team.Root)) || envelope.Team.Epoch == 0 || envelope.Sender.Team != envelope.Team || envelope.Recipient.Team != envelope.Team || !childUUID.MatchString(string(envelope.Sender.ID)) || !childUUID.MatchString(string(envelope.Recipient.ID)) || envelope.Sender.Generation == 0 || envelope.Recipient.Generation == 0 || envelope.Sender == envelope.Recipient || !peerMessageIDPattern.MatchString(envelope.MessageID) {
		return receipt, errors.New("invalid peer receipt identity")
	}
	if hash, err := hex.DecodeString(envelope.BodySHA256); err != nil || len(hash) != sha256.Size || hex.EncodeToString(hash) != envelope.BodySHA256 {
		return receipt, errors.New("invalid peer body digest")
	}
	if attempt := receipt.Attempt; attempt != nil && (receipt.State == peerAccepted || !validPeerDeliveryAttempt(*attempt)) {
		return receipt, errors.New("invalid peer delivery attempt")
	}
	if receipt.State == peerObserved {
		if receipt.SessionID != envelope.Recipient.ID || receipt.NativeEntryID == "" {
			return receipt, errors.New("invalid peer native observation")
		}
	} else if receipt.SessionID != "" || receipt.NativeEntryID != "" {
		return receipt, errors.New("unexpected peer native observation")
	}
	return receipt, nil
}

type classifiedPeer struct {
	Receipt    peerReceipt
	Body       peerBody
	SenderName string
	Provenance peerProvenance
	Reason     string
	Assigned   bool
}

func (s *teamStore) classifyPeer(roster *teamRoster, members map[MemberRef]string, recipient MemberRef, filename string, receipt peerReceipt) classifiedPeer {
	item := classifiedPeer{Receipt: receipt, Provenance: peerRejected, Reason: "invalid receipt"}
	envelope := receipt.Envelope
	if envelope.Team != roster.Team {
		return item
	}
	if _, ok := members[envelope.Recipient]; !ok && envelope.Recipient != teamLeadRef(roster.Team) {
		return item
	}
	item.Assigned = true
	if envelope.Recipient != recipient {
		return item
	}
	key, err := peerKey(envelope)
	if err != nil || filename != "peer-receipt-"+key+".json" {
		item.Reason = "receipt identity does not match its filename"
		return item
	}
	bodyPath, _ := peerPaths(s.dir, key)
	data, err := readPeerFile(bodyPath)
	if err != nil {
		item.Reason = "message body is unavailable"
		return item
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != envelope.BodySHA256 {
		item.Reason = "message body does not match its receipt"
		return item
	}
	if envelope.Sender == teamLeadRef(roster.Team) {
		item.Provenance, item.Reason = peerForged, "team lead cannot be a peer sender"
		return item
	}
	item.SenderName = members[envelope.Sender]
	if item.SenderName == "" {
		item.Reason = "sender is not a committed teammate"
		return item
	}
	if !utf8.Valid(data) || checkUniqueJSON(data) != nil {
		item.Reason = "invalid message body"
		return item
	}
	if _, err := exactJSONObject(data, "text", "summary"); err != nil || json.Unmarshal(data, &item.Body) != nil || validatePeerBody(item.Body) != nil {
		item.Reason = "invalid message body"
		return item
	}
	authenticated, ok := s.peerAuth[key]
	if !ok {
		item.Provenance, item.Reason = peerUnverifiable, "no current-owner sender authentication"
		return item
	}
	if authenticated.Envelope != envelope || authenticated.SenderName != item.SenderName {
		item.Provenance, item.Reason = peerForged, "receipt conflicts with authenticated sender"
		return item
	}
	item.Provenance, item.Reason = peerVerified, "authenticated by current owner"
	return item
}

func isPeerReceiptFile(name string) bool {
	return strings.HasPrefix(name, "peer-receipt-") && strings.HasSuffix(name, ".json")
}

func (s *teamStore) peerInbox(recipient MemberRef) (peerInboxSummary, error) {
	summary := peerInboxSummary{
		VerificationScope: "current_owner_runtime",
		Notice:            "Receipt metadata cannot prove provenance against arbitrary processes running as the same OS user.",
		Items:             []peerInboxItem{},
	}
	var details [3][]peerInboxItem
	err := s.transact(func(roster *teamRoster) (bool, error) {
		members := make(map[MemberRef]string, len(roster.Members))
		for _, member := range roster.Members {
			members[member.Ref] = member.Name
		}
		if recipient.Team != roster.Team || members[recipient] == "" && recipient != teamLeadRef(roster.Team) {
			return false, reject("permission_denied", "recipient is not in the current team")
		}
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			return false, err
		}
		for _, entry := range entries {
			if !isPeerReceiptFile(entry.Name()) {
				continue
			}
			receipt, err := readPeerReceipt(filepath.Join(s.dir, entry.Name()))
			if err != nil {
				summary.UnassignedRejected++
				continue
			}
			item := s.classifyPeer(roster, members, recipient, entry.Name(), receipt)
			if !item.Assigned {
				summary.UnassignedRejected++
				continue
			}
			if item.Receipt.Envelope.Recipient != recipient {
				continue
			}
			priority := 0
			switch item.Provenance {
			case peerVerified:
				summary.Verified++
				continue
			case peerUnverifiable:
				summary.Unverifiable++
				priority = 2
			case peerForged:
				summary.Forged++
			case peerRejected:
				summary.Rejected++
				priority = 1
			}
			if len(details[priority]) < peerInboxLimit {
				details[priority] = append(details[priority], peerInboxItem{MessageID: item.Receipt.Envelope.MessageID, Provenance: item.Provenance, ReceiptState: item.Receipt.State, Reason: item.Reason})
			}
		}
		return false, nil
	})
	for _, group := range details {
		for _, item := range group {
			if len(summary.Items) == peerInboxLimit {
				break
			}
			summary.Items = append(summary.Items, item)
		}
	}
	summary.More = summary.Unverifiable+summary.Forged+summary.Rejected > len(summary.Items)
	return summary, err
}

func publishPeerFile(path string, data []byte, replace bool) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".peer-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	if replace {
		err = os.Rename(name, path)
	} else {
		err = os.Link(name, path)
	}
	if err != nil {
		return err
	}
	if !replace {
		if err := os.Remove(name); err != nil {
			return err
		}
	}
	return syncDir(dir)
}

func (s *teamStore) acceptPeer(sender MemberRef, request peerRequest) (peerReceipt, string, error) {
	var accepted peerReceipt
	var senderName string
	err := s.transact(func(roster *teamRoster) (bool, error) {
		if sender.Team != roster.Team {
			return false, reject("permission_denied", "sender is not in the current team")
		}
		var senderFound, recipientFound bool
		var recipient MemberRef
		if strings.EqualFold(request.Recipient, "team-lead") {
			recipientFound, recipient = true, teamLeadRef(roster.Team)
		}
		for _, member := range roster.Members {
			if member.Ref == sender {
				senderFound, senderName = true, member.Name
			}
			if strings.EqualFold(member.Name, request.Recipient) {
				recipientFound, recipient = true, member.Ref
			}
		}
		if !senderFound || !recipientFound || recipient.Team != sender.Team || recipient == sender {
			return false, reject("permission_denied", "sender or recipient is not an eligible teammate")
		}
		body, err := json.Marshal(peerBody{Text: request.Text, Summary: request.Summary})
		if err != nil {
			return false, err
		}
		bodyHash := sha256.Sum256(body)
		envelope := peerEnvelope{Team: roster.Team, MessageID: request.MessageID, Sender: sender, Recipient: recipient, BodySHA256: hex.EncodeToString(bodyHash[:])}
		key, err := peerKey(envelope)
		if err != nil {
			return false, err
		}
		bodyPath, receiptPath := peerPaths(s.dir, key)
		existing, err := readPeerReceipt(receiptPath)
		if err == nil {
			stored, readErr := readPeerFile(bodyPath)
			if readErr != nil || existing.Envelope != envelope || !bytes.Equal(stored, body) {
				return false, reject("id_conflict", "message ID already has different content or identity")
			}
			if err := syncDir(s.dir); err != nil {
				return false, err
			}
			if s.peerAuth == nil {
				s.peerAuth = make(map[string]peerAuthorization)
			}
			s.peerAuth[key] = peerAuthorization{Envelope: envelope, SenderName: senderName}
			accepted = existing
			return false, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			return false, err
		}
		count := 0
		receipts := make(map[string]bool)
		for _, entry := range entries {
			if !isPeerReceiptFile(entry.Name()) {
				continue
			}
			receiptKey := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "peer-receipt-"), ".json")
			if decoded, err := hex.DecodeString(receiptKey); err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != receiptKey {
				count++
				continue
			}
			receipts[receiptKey] = true
			receipt, err := readPeerReceipt(filepath.Join(s.dir, entry.Name()))
			if err != nil {
				count++
				continue
			}
			actualKey, err := peerKey(receipt.Envelope)
			if err != nil || actualKey != receiptKey {
				count++
				continue
			}
			if receipt.Envelope.Recipient == recipient && receipt.State != peerObserved {
				count++
			}
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "peer-body-") || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			bodyKey := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "peer-body-"), ".json")
			if decoded, err := hex.DecodeString(bodyKey); err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != bodyKey {
				count++
				continue
			}
			if !receipts[bodyKey] && bodyKey != key {
				count++
			}
		}
		if count >= peerInboxLimit {
			return false, reject("capacity", "teammate inbox is full")
		}
		stored, err := readPeerFile(bodyPath)
		if errors.Is(err, os.ErrNotExist) {
			err = publishPeerFile(bodyPath, body, false)
		} else if err == nil && !bytes.Equal(stored, body) {
			err = reject("id_conflict", "message ID already has different content")
		}
		if err != nil {
			return false, err
		}
		accepted = peerReceipt{Envelope: envelope, State: peerAccepted}
		data, err := json.Marshal(accepted)
		if err != nil {
			return false, err
		}
		if err := publishPeerFile(receiptPath, data, false); err != nil {
			return false, fmt.Errorf("persist peer receipt: %w", err)
		}
		if s.peerAuth == nil {
			s.peerAuth = make(map[string]peerAuthorization)
		}
		s.peerAuth[key] = peerAuthorization{Envelope: envelope, SenderName: senderName}
		return false, nil
	})
	return accepted, senderName, err
}

func (s *teamStore) offerPeer(recipient MemberRef) (peerReceipt, string, bool, error) {
	return s.offerPeerAttempt(recipient, nil)
}

func (s *teamStore) offerPeerAttempt(recipient MemberRef, attempt *peerDeliveryAttempt) (peerReceipt, string, bool, error) {
	var offered peerReceipt
	var content string
	found := false
	err := s.transact(func(roster *teamRoster) (bool, error) {
		if roster.Team != recipient.Team {
			return false, reject("unavailable", "team identity changed")
		}
		if attempt != nil && !validPeerDeliveryAttempt(*attempt) {
			return false, reject("unavailable", "peer delivery identity is invalid")
		}
		members := make(map[MemberRef]string, len(roster.Members))
		for _, member := range roster.Members {
			members[member.Ref] = member.Name
		}
		if _, ok := members[recipient]; !ok && recipient != teamLeadRef(roster.Team) {
			return false, reject("permission_denied", "recipient is not a committed teammate")
		}
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			return false, err
		}
		for _, entry := range entries {
			if !isPeerReceiptFile(entry.Name()) {
				continue
			}
			path := filepath.Join(s.dir, entry.Name())
			receipt, err := readPeerReceipt(path)
			if err != nil || receipt.Envelope.Recipient != recipient {
				continue
			}
			if receipt.State != peerAccepted && (attempt == nil || receipt.Attempt == nil || receipt.State != peerAttempted && receipt.State != peerAmbiguous) {
				continue
			}
			item := s.classifyPeer(roster, members, recipient, entry.Name(), receipt)
			if !item.Assigned || item.Provenance != peerVerified {
				continue
			}
			content, err = peerMessageContent(receipt.Envelope, item.SenderName, item.Body)
			if err != nil {
				return false, err
			}
			if receipt.State == peerAccepted && attempt != nil {
				id, err := nativePeerEntry(attempt.SessionFile, receipt.Envelope, content)
				if err != nil {
					return false, err
				}
				if id != "" {
					return false, errors.New("native peer entry predates its delivery attempt")
				}
			}
			if receipt.State != peerAccepted {
				if receipt.Attempt.SessionFile != attempt.SessionFile {
					continue
				}
				id, err := nativePeerEntry(attempt.SessionFile, receipt.Envelope, content)
				if err != nil {
					return false, err
				}
				if id != "" {
					receipt.State, receipt.SessionID, receipt.NativeEntryID = peerObserved, recipient.ID, id
					data, err := json.Marshal(receipt)
					if err != nil {
						return false, err
					}
					if err := publishPeerFile(path, data, true); err != nil {
						return false, err
					}
					continue
				}
				if !peerHostGone(receipt.Attempt.Host, attempt.Host) {
					continue
				}
			}
			receipt.State = peerAttempted
			receipt.Attempt = attempt
			data, err := json.Marshal(receipt)
			if err != nil {
				return false, err
			}
			if err := publishPeerFile(path, data, true); err != nil {
				return false, err
			}
			offered, found = receipt, true
			break
		}
		return false, nil
	})
	return offered, content, found, err
}

func (s *teamStore) peerWakeEligible(recipient MemberRef) (bool, error) {
	eligible := false
	err := s.transact(func(roster *teamRoster) (bool, error) {
		if roster.Team != recipient.Team {
			return false, reject("unavailable", "team identity changed")
		}
		idle := false
		members := make(map[MemberRef]string, len(roster.Members))
		for _, member := range roster.Members {
			members[member.Ref] = member.Name
			if member.Ref == recipient && member.State == "idle" {
				idle = true
			}
		}
		if !idle {
			return false, nil
		}
		for key, authenticated := range s.peerAuth {
			if authenticated.Envelope.Recipient != recipient {
				continue
			}
			if members[authenticated.Envelope.Sender] != authenticated.SenderName {
				continue
			}
			_, path := peerPaths(s.dir, key)
			receipt, err := readPeerReceipt(path)
			if err != nil || receipt.State != peerAccepted {
				continue
			}
			item := s.classifyPeer(roster, members, recipient, filepath.Base(path), receipt)
			if item.Provenance == peerVerified && item.Receipt.State == peerAccepted {
				eligible = true
				break
			}
		}
		return false, nil
	})
	return eligible, err
}

func nativePeerEntry(path string, envelope peerEnvelope, content string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	info, statErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if err := errors.Join(statErr, pathErr); err != nil {
		return "", errors.Join(err, file.Close())
	}
	if !info.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(info, current) || info.Size() > 32*1024*1024 {
		return "", errors.Join(errors.New("invalid native peer transcript"), file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, info.Size()))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return "", err
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return "", errors.New("native peer transcript is incomplete")
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
	var header struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(lines[0], &header); err != nil || header.Type != "session" || header.ID != string(envelope.Recipient.ID) {
		return "", errors.New("native peer session identity differs")
	}
	var id string
	for _, line := range lines[1:] {
		var entry struct {
			Type       string          `json:"type"`
			ID         string          `json:"id"`
			CustomType string          `json:"customType"`
			Content    json.RawMessage `json:"content"`
			Details    json.RawMessage `json:"details"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			return "", err
		}
		if entry.Type != "custom_message" || entry.CustomType != "pig_litter_peer" {
			continue
		}
		var details struct {
			Envelope peerEnvelope `json:"envelope"`
		}
		var entryContent string
		if json.Unmarshal(entry.Details, &details) != nil || json.Unmarshal(entry.Content, &entryContent) != nil {
			return "", errors.New("invalid native peer message")
		}
		other := details.Envelope
		if other.Team != envelope.Team || other.Sender != envelope.Sender || other.Recipient != envelope.Recipient || other.MessageID != envelope.MessageID {
			continue
		}
		if other != envelope || entryContent != content {
			return "", errors.New("native peer transcript has conflicting message content")
		}
		if id != "" || entry.ID == "" {
			return "", errors.New("native peer transcript has duplicate or missing entry IDs")
		}
		id = entry.ID
	}
	return id, nil
}

func (s *teamStore) observePeers(recipient MemberRef, sessionFile string) error {
	return s.transact(func(roster *teamRoster) (bool, error) {
		if roster.Team != recipient.Team {
			return false, reject("unavailable", "team identity changed")
		}
		members := make(map[MemberRef]string, len(roster.Members))
		for _, member := range roster.Members {
			members[member.Ref] = member.Name
		}
		if _, ok := members[recipient]; !ok && recipient != teamLeadRef(roster.Team) {
			return false, reject("permission_denied", "recipient is not a committed teammate")
		}
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			return false, err
		}
		for _, entry := range entries {
			if !isPeerReceiptFile(entry.Name()) {
				continue
			}
			path := filepath.Join(s.dir, entry.Name())
			receipt, err := readPeerReceipt(path)
			if err != nil || receipt.Envelope.Recipient != recipient || receipt.State != peerAttempted && receipt.State != peerAmbiguous {
				continue
			}
			item := s.classifyPeer(roster, members, recipient, entry.Name(), receipt)
			if !item.Assigned || item.Provenance != peerVerified {
				continue
			}
			content, err := peerMessageContent(receipt.Envelope, item.SenderName, item.Body)
			if err != nil {
				return false, err
			}
			id, err := nativePeerEntry(sessionFile, receipt.Envelope, content)
			if err != nil {
				return false, err
			}
			if id == "" {
				continue
			}
			receipt.State, receipt.SessionID, receipt.NativeEntryID = peerObserved, recipient.ID, id
			data, err := json.Marshal(receipt)
			if err != nil {
				return false, err
			}
			if err := publishPeerFile(path, data, true); err != nil {
				return false, err
			}
		}
		return false, nil
	})
}

func (o *owner) peerRunStore(r *record, selected *run) (*teamStore, bool) {
	o.tree.mu.Lock()
	current, err := o.tree.callerLocked(selected)
	valid := err == nil && current == r && r.team != nil && selected.ctx.Err() == nil
	o.tree.mu.Unlock()
	if !valid {
		return nil, false
	}
	o.teamMu.Lock()
	store := o.teamStore
	o.teamMu.Unlock()
	if store == nil {
		return nil, false
	}
	team, members, err := store.Snapshot()
	if err != nil || team != r.team.Ref.Team {
		return nil, false
	}
	for _, member := range members {
		if member.Ref == r.team.Ref {
			return store, true
		}
	}
	return nil, false
}

func embeddedPeerAttempt(session *coding.Session, recipient MemberRef) (*peerDeliveryAttempt, error) {
	if session == nil || session.SessionManager().GetSessionId() != string(recipient.ID) {
		return nil, reject("unavailable", "peer Session identity changed")
	}
	file := session.SessionManager().GetSessionFile()
	if file == nil || *file == "" {
		return nil, reject("unavailable", "peer Session file is unavailable")
	}
	host, err := embeddedPeerHost()
	if err != nil {
		return nil, err
	}
	attempt := &peerDeliveryAttempt{Host: host, SessionFile: filepath.Clean(*file)}
	if !validPeerDeliveryAttempt(*attempt) {
		return nil, reject("unavailable", "peer delivery identity is invalid")
	}
	return attempt, nil
}

func (o *owner) observePeerRun(r *record, selected *run) error {
	store, ok := o.peerRunStore(r, selected)
	if !ok {
		return nil
	}
	selected.mu.Lock()
	session := selected.session
	selected.mu.Unlock()
	if session == nil {
		return nil
	}
	path := session.SessionManager().GetSessionFile()
	if path == nil || *path == "" {
		return nil
	}
	if err := store.observePeers(r.team.Ref, *path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (o *owner) peerHooks(r *record, selected *run) extension.Extension {
	observe := func(...any) (any, error) {
		return nil, o.observePeerRun(r, selected)
	}
	return extension.Extension{Name: "pig-litter-peer-messages", Handlers: map[string][]extension.HandlerFn{
		"turn_start":    {observe},
		"agent_settled": {observe},
		"agent_before_settle": {func(...any) (any, error) {
			store, ok := o.peerRunStore(r, selected)
			if !ok {
				return nil, nil
			}
			selected.mu.Lock()
			session := selected.session
			selected.mu.Unlock()
			attempt, err := embeddedPeerAttempt(session, r.team.Ref)
			if err != nil {
				return nil, nil
			}
			receipt, content, found, err := store.offerPeerAttempt(r.team.Ref, attempt)
			if err != nil || !found {
				return nil, err
			}
			continuation := true
			entries := []extension.SessionBoundaryDraft{{Type: extension.SessionBoundaryDraftCustomMessage, CustomType: "pig_litter_peer", Content: content, Display: true, Details: map[string]any{"envelope": receipt.Envelope}}}
			return extension.BoundaryResult{Continue: &continuation, Entries: &entries}, nil
		}},
	}}
}
