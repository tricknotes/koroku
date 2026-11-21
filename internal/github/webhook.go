package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Webhook headers GitHub sends with every delivery.
const (
	// SignatureHeader carries "sha256=<hex HMAC-SHA256 of the body>".
	SignatureHeader = "X-Hub-Signature-256"
	// EventHeader names the event (push, ping, ...).
	EventHeader = "X-GitHub-Event"
	// DeliveryHeader is the unique ID of a delivery. A redelivery from
	// GitHub keeps the ID.
	DeliveryHeader = "X-GitHub-Delivery"
)

// ErrBadSignature is returned when a webhook body does not match its
// signature, or the signature is missing or malformed.
var ErrBadSignature = errors.New("github: webhook signature does not match")

// VerifySignature checks the X-Hub-Signature-256 value of a webhook body
// against the webhook secret, in constant time. An empty secret matches
// nothing.
func VerifySignature(secret []byte, body []byte, header string) error {
	if len(secret) == 0 {
		return ErrBadSignature
	}
	hexSum, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return ErrBadSignature
	}
	got, err := hex.DecodeString(hexSum)
	if err != nil || len(got) != sha256.Size {
		return ErrBadSignature
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrBadSignature
	}
	return nil
}

// Sign returns the X-Hub-Signature-256 value GitHub would send for body.
// Tests and local tools use it.
func Sign(secret []byte, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// PushEvent is the part of a push webhook payload koroku uses.
type PushEvent struct {
	// Ref is the full ref that was pushed: refs/heads/<branch> or refs/tags/<tag>.
	Ref string
	// After is the commit the ref points at after the push; all zeros when
	// the ref was deleted.
	After string
	// Deleted is true when the push deleted the ref.
	Deleted bool
	// RepoID and RepoFullName identify the repository.
	RepoID       int64
	RepoFullName string
	// Pusher is the GitHub login that pushed (the sender), or the pusher's
	// name when the sender is missing.
	Pusher string
	// HeadMessage is the message of the head commit when it is After.
	HeadMessage string
}

const zeroSHA = "0000000000000000000000000000000000000000"

// Branch returns the branch name of a push to a branch, and false for tags
// and other refs.
func (p PushEvent) Branch() (string, bool) {
	return strings.CutPrefix(p.Ref, "refs/heads/")
}

// RemovesRef reports whether the push deleted its ref.
func (p PushEvent) RemovesRef() bool {
	return p.Deleted || p.After == "" || p.After == zeroSHA
}

// ParsePushEvent decodes a push webhook payload.
func ParsePushEvent(body []byte) (PushEvent, error) {
	var raw struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Deleted    bool   `json:"deleted"`
		Repository struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repository"`
		Pusher struct {
			Name string `json:"name"`
		} `json:"pusher"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		HeadCommit *struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		} `json:"head_commit"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return PushEvent{}, fmt.Errorf("github: decode push event: %w", err)
	}
	if raw.Ref == "" || (raw.Repository.ID == 0 && raw.Repository.FullName == "") {
		return PushEvent{}, errors.New("github: push event lacks ref or repository")
	}
	ev := PushEvent{
		Ref:          raw.Ref,
		After:        strings.ToLower(raw.After),
		Deleted:      raw.Deleted,
		RepoID:       raw.Repository.ID,
		RepoFullName: raw.Repository.FullName,
		Pusher:       raw.Sender.Login,
	}
	if ev.Pusher == "" {
		ev.Pusher = raw.Pusher.Name
	}
	if raw.HeadCommit != nil && strings.EqualFold(raw.HeadCommit.ID, raw.After) {
		ev.HeadMessage = raw.HeadCommit.Message
	}
	return ev, nil
}
