package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

// mailProviderWithoutExpectsReply wraps a real provider behind the bare
// mail.Provider interface, hiding beadmail's extra MarkExpectsReply/
// ClearExpectsReply/ListSent methods from its method set (Go promotes only
// the embedded INTERFACE's declared methods, not the dynamic value's extra
// ones) -- exercising the same "unsupported provider" path a real exec:
// configuration would hit, without needing to shell out to a script.
type mailProviderWithoutExpectsReply struct {
	mail.Provider
}

func TestDoMailSendJSON_ExpectsReplyMarksMessage(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	var stdout, stderr bytes.Buffer
	code := doMailSendJSON(mp, events.Discard, recipients, "human", []string{"mayor", "ship it?"}, nil, false, &stdout, &stderr, mailAskOptions{ExpectsReply: true})
	if code != 0 {
		t.Fatalf("doMailSendJSON = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata[mail.ExpectsReplyMetadataKey] != "true" {
		t.Errorf("bead Metadata[%s] = %q, want %q", mail.ExpectsReplyMetadataKey, b.Metadata[mail.ExpectsReplyMetadataKey], "true")
	}
}

func TestDoMailSendJSON_WithoutOptsDoesNotMark(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	var stdout, stderr bytes.Buffer
	// No trailing mailAskOptions argument at all -- the pre-existing call shape
	// every other test in this package uses. Must behave byte-identically.
	code := doMailSendJSON(mp, events.Discard, recipients, "human", []string{"mayor", "fyi"}, nil, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailSendJSON = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Metadata[mail.ExpectsReplyMetadataKey]; ok {
		t.Errorf("bead unexpectedly carries %s metadata: %q", mail.ExpectsReplyMetadataKey, b.Metadata[mail.ExpectsReplyMetadataKey])
	}
}

func TestDoMailSendAllJSON_ExpectsReplyMarksEveryBroadcastMessage(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "alice": true, "bob": true}

	var stdout, stderr bytes.Buffer
	code := doMailSendAllJSON(mp, events.Discard, recipients, "sender", []string{"broadcast ask"}, nil, false, &stdout, &stderr, mailAskOptions{ExpectsReply: true})
	if code != 0 {
		t.Fatalf("doMailSendAllJSON = %d, want 0; stderr: %s", code, stderr.String())
	}
	all, err := store.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("created %d messages, want 2 (alice, bob)", len(all))
	}
	for _, b := range all {
		if b.Metadata[mail.ExpectsReplyMetadataKey] != "true" {
			t.Errorf("broadcast message %s Metadata[%s] = %q, want %q", b.ID, mail.ExpectsReplyMetadataKey, b.Metadata[mail.ExpectsReplyMetadataKey], "true")
		}
	}
}

func TestDoMailReplyJSON_ExpectsReplyMarksReply(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	original, err := mp.Send("bob", "human", "need input", "how should we proceed?")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doMailReplyJSON(mp, events.Discard, original.ID, "human", "", "here's a follow-up question", nil, false, &stdout, &stderr, mailAskOptions{ExpectsReply: true})
	if code != 0 {
		t.Fatalf("doMailReplyJSON = %d, want 0; stderr: %s", code, stderr.String())
	}

	all, err := store.List(beads.ListQuery{Type: "message", Label: "reply-to:" + original.ID, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("found %d replies, want 1", len(all))
	}
	if all[0].Metadata[mail.ExpectsReplyMetadataKey] != "true" {
		t.Errorf("reply Metadata[%s] = %q, want %q", mail.ExpectsReplyMetadataKey, all[0].Metadata[mail.ExpectsReplyMetadataKey], "true")
	}
}

func TestApplyExpectsReplyOption_UnsupportedProviderWarnsWithoutFailing(t *testing.T) {
	store := beads.NewMemStore()
	mp := mailProviderWithoutExpectsReply{Provider: beadmail.New(store)}

	m, err := mp.Send("human", "mayor", "s", "b")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var stderr bytes.Buffer
	applyExpectsReplyOption(mp, m.ID, mailAskOptions{ExpectsReply: true}, "gc mail send", &stderr)
	if !strings.Contains(stderr.String(), "requires the beadmail provider") {
		t.Errorf("stderr = %q, want a warning naming the unsupported-provider fallback", stderr.String())
	}
	if !strings.Contains(stderr.String(), m.ID) {
		t.Errorf("stderr = %q, want it to name the message id so the sender knows which message lacks the exemption", stderr.String())
	}
}

func TestApplyExpectsReplyOption_NoOptDoesNothing(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	m, err := mp.Send("human", "mayor", "s", "b")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	var stderr bytes.Buffer
	applyExpectsReplyOption(mp, m.ID, mailAskOptions{}, "gc mail send", &stderr)
	if stderr.Len() != 0 {
		t.Errorf("unexpected stderr with ExpectsReply=false: %q", stderr.String())
	}
	b, err := store.Get(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Metadata[mail.ExpectsReplyMetadataKey]; ok {
		t.Errorf("bead unexpectedly carries %s metadata", mail.ExpectsReplyMetadataKey)
	}
}

func TestMailDispositionAndReadEvidence(t *testing.T) {
	cases := []struct {
		name         string
		m            beadmail.SentMessage
		disposition  string
		readEvidence string
	}{
		{
			name:         "ordinary unread",
			m:            beadmail.SentMessage{},
			disposition:  "-",
			readEvidence: "no evidence",
		},
		{
			name:         "ordinary read",
			m:            beadmail.SentMessage{Message: mail.Message{Read: true}},
			disposition:  "-",
			readEvidence: "seen",
		},
		{
			name:         "unanswered ask",
			m:            beadmail.SentMessage{ExpectsReply: true, Answered: false},
			disposition:  "unanswered",
			readEvidence: "no evidence",
		},
		{
			name:         "answered ask",
			m:            beadmail.SentMessage{ExpectsReply: true, Answered: true, Message: mail.Message{Read: true}},
			disposition:  "answered",
			readEvidence: "seen",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mailDisposition(tc.m); got != tc.disposition {
				t.Errorf("mailDisposition = %q, want %q", got, tc.disposition)
			}
			if got := mailReadEvidence(tc.m); got != tc.readEvidence {
				t.Errorf("mailReadEvidence = %q, want %q (never \"unread\" -- absence of the read flag is not proof a message went unseen)", got, tc.readEvidence)
			}
		})
	}
}

// setUpTestCity creates a minimal file-backed city (mirroring
// TestCmdMailSendFromControllerCreatesMessage's fixture) and points env vars
// at it, returning the resolved city path.
func setUpTestCity(t *testing.T) string {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_AGENT", "")

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
	return cityPath
}

func TestCmdMailResolve_ClearsExemptionAndReleasesFromOutstanding(t *testing.T) {
	cityPath := setUpTestCity(t)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	mp := beadmail.New(store)
	ask, err := mp.Send("human", "mayor", "decision needed", "ship it?")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := mp.MarkExpectsReply(ask.ID); err != nil {
		t.Fatalf("MarkExpectsReply: %v", err)
	}

	outstanding, err := mp.ListSent("human", true)
	if err != nil {
		t.Fatalf("ListSent before resolve: %v", err)
	}
	if len(outstanding) != 1 {
		t.Fatalf("precondition: outstanding = %v, want exactly [%s]", outstanding, ask.ID)
	}

	var stdout, stderr bytes.Buffer
	code := cmdMailResolve(ask.ID, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailResolve = %d, want 0; stderr: %s", code, stderr.String())
	}

	storeAfter, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt after resolve: %v", err)
	}
	mpAfter := beadmail.New(storeAfter)
	outstandingAfter, err := mpAfter.ListSent("human", true)
	if err != nil {
		t.Fatalf("ListSent after resolve: %v", err)
	}
	if len(outstandingAfter) != 0 {
		t.Fatalf("outstanding after resolve = %v, want empty", outstandingAfter)
	}
}

func TestCmdMailResolve_UnknownIDErrors(t *testing.T) {
	setUpTestCity(t)
	var stdout, stderr bytes.Buffer
	code := cmdMailResolve("does-not-exist", false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdMailResolve(missing) = 0, want nonzero; stdout=%s", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Error("expected an error message on stderr")
	}
}

func TestCmdMailSent_OutstandingFiltersToUnansweredExpectsReplyAsks(t *testing.T) {
	cityPath := setUpTestCity(t)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	mp := beadmail.New(store)

	unanswered, err := mp.Send("human", "mayor", "s1", "outstanding")
	if err != nil {
		t.Fatalf("Send unanswered: %v", err)
	}
	if err := mp.MarkExpectsReply(unanswered.ID); err != nil {
		t.Fatalf("MarkExpectsReply: %v", err)
	}
	if _, err := mp.Send("human", "mayor", "s2", "ordinary fyi"); err != nil {
		t.Fatalf("Send ordinary: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdMailSent(true /* outstanding */, true /* json */, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailSent = %d, want 0; stderr: %s", code, stderr.String())
	}
	var got mailSentJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if !got.Outstanding || got.Sender != "human" {
		t.Fatalf("payload = %+v, want Outstanding=true Sender=human", got)
	}
	if len(got.Messages) != 1 || got.Messages[0].ID != unanswered.ID {
		t.Fatalf("Messages = %+v, want exactly [%s]", got.Messages, unanswered.ID)
	}
	if got.Messages[0].Disposition != "unanswered" {
		t.Errorf("Disposition = %q, want %q", got.Messages[0].Disposition, "unanswered")
	}
}

func TestCmdMailSent_NoFlagListsEverythingWithMixedDisposition(t *testing.T) {
	cityPath := setUpTestCity(t)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	mp := beadmail.New(store)
	if _, err := mp.Send("human", "mayor", "s1", "ordinary"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ask, err := mp.Send("human", "mayor", "s2", "ask")
	if err != nil {
		t.Fatalf("Send ask: %v", err)
	}
	if err := mp.MarkExpectsReply(ask.ID); err != nil {
		t.Fatalf("MarkExpectsReply: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdMailSent(false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailSent = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "unanswered") {
		t.Errorf("table output = %q, want it to show the unanswered ask", out)
	}
	if strings.Count(out, "\n") < 3 {
		t.Errorf("table output = %q, want a header row plus 2 message rows", out)
	}
}
