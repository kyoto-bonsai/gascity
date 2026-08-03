package beadmail

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

func TestHasReply(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	ask, err := p.Send("alice", "bob", "need a decision", "ship it?")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got, err := HasReply(store, ask.ID); err != nil || got {
		t.Fatalf("HasReply before any reply = (%v, %v), want (false, nil)", got, err)
	}

	reply, err := p.Reply(ask.ID, "bob", "", "yes")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if got, err := HasReply(store, ask.ID); err != nil || !got {
		t.Fatalf("HasReply after reply = (%v, %v), want (true, nil)", got, err)
	}

	// F2: the reply is itself mail and gets read-swept closed by the same
	// retention mechanism HasReply feeds into. IncludeClosed:true must still
	// find it, or every answered ask outside its reply's TTL window would read
	// as unanswered.
	if err := store.SetMetadata(reply.ID, "close_reason", RetentionSweepCloseReason); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := store.Close(reply.ID); err != nil {
		t.Fatalf("Close reply: %v", err)
	}
	if got, err := HasReply(store, ask.ID); err != nil || !got {
		t.Fatalf("HasReply after reply closed = (%v, %v), want (true, nil) — closed-reply blindness (F2)", got, err)
	}
}

func TestHasReply_UnrelatedRepliesDoNotMatch(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	askA, err := p.Send("alice", "bob", "ask A", "?")
	if err != nil {
		t.Fatalf("Send askA: %v", err)
	}
	askB, err := p.Send("alice", "carol", "ask B", "?")
	if err != nil {
		t.Fatalf("Send askB: %v", err)
	}
	if _, err := p.Reply(askB.ID, "carol", "", "answering B, not A"); err != nil {
		t.Fatalf("Reply to askB: %v", err)
	}

	if got, err := HasReply(store, askA.ID); err != nil || got {
		t.Fatalf("HasReply(askA) = (%v, %v), want (false, nil) — a reply to a different ask must not match", got, err)
	}
}

func TestMarkExpectsReplyAndClearExpectsReply(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	msg, err := p.Send("alice", "bob", "s", "b")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	got, err := p.Get(msg.ID)
	if err != nil || got.ExpectsReply {
		t.Fatalf("before MarkExpectsReply: Get = (%+v, %v), want ExpectsReply=false", got, err)
	}

	if err := p.MarkExpectsReply(msg.ID); err != nil {
		t.Fatalf("MarkExpectsReply: %v", err)
	}
	got, err = p.Get(msg.ID)
	if err != nil || !got.ExpectsReply {
		t.Fatalf("after MarkExpectsReply: Get = (%+v, %v), want ExpectsReply=true", got, err)
	}

	if err := p.ClearExpectsReply(msg.ID); err != nil {
		t.Fatalf("ClearExpectsReply: %v", err)
	}
	got, err = p.Get(msg.ID)
	if err != nil || got.ExpectsReply {
		t.Fatalf("after ClearExpectsReply: Get = (%+v, %v), want ExpectsReply=false", got, err)
	}
}

func TestMarkExpectsReply_NotFound(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	if err := p.MarkExpectsReply("does-not-exist"); !errors.Is(err, mail.ErrNotFound) {
		t.Fatalf("MarkExpectsReply(missing) = %v, want ErrNotFound", err)
	}
}

func TestClearExpectsReply_NotFound(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	if err := p.ClearExpectsReply("does-not-exist"); !errors.Is(err, mail.ErrNotFound) {
		t.Fatalf("ClearExpectsReply(missing) = %v, want ErrNotFound", err)
	}
}

func TestListSent(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	unanswered, err := p.Send("alice", "bob", "s1", "outstanding ask")
	if err != nil {
		t.Fatalf("Send unanswered: %v", err)
	}
	if err := p.MarkExpectsReply(unanswered.ID); err != nil {
		t.Fatalf("MarkExpectsReply unanswered: %v", err)
	}

	answered, err := p.Send("alice", "carol", "s2", "answered ask")
	if err != nil {
		t.Fatalf("Send answered: %v", err)
	}
	if err := p.MarkExpectsReply(answered.ID); err != nil {
		t.Fatalf("MarkExpectsReply answered: %v", err)
	}
	if _, err := p.Reply(answered.ID, "carol", "", "here's your answer"); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	ordinary, err := p.Send("alice", "dave", "s3", "just an FYI")
	if err != nil {
		t.Fatalf("Send ordinary: %v", err)
	}
	if _, err := p.Read(ordinary.ID); err != nil {
		t.Fatalf("Read ordinary: %v", err)
	}

	notMine, err := p.Send("someone-else", "bob", "s4", "not from alice")
	if err != nil {
		t.Fatalf("Send notMine: %v", err)
	}
	if err := p.MarkExpectsReply(notMine.ID); err != nil {
		t.Fatalf("MarkExpectsReply notMine: %v", err)
	}

	all, err := p.ListSent("alice", false)
	if err != nil {
		t.Fatalf("ListSent(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListSent(alice, false) returned %d messages, want 3 (unanswered, answered, ordinary — not notMine)", len(all))
	}
	byID := make(map[string]SentMessage, len(all))
	for _, m := range all {
		byID[m.ID] = m
	}
	if _, ok := byID[notMine.ID]; ok {
		t.Error("ListSent(alice) must not include a message sent by someone-else")
	}
	if m, ok := byID[unanswered.ID]; !ok || !m.ExpectsReply || m.Answered {
		t.Errorf("unanswered ask in ListSent = %+v (ok=%v), want ExpectsReply=true Answered=false", m, ok)
	}
	if m, ok := byID[answered.ID]; !ok || !m.ExpectsReply || !m.Answered {
		t.Errorf("answered ask in ListSent = %+v (ok=%v), want ExpectsReply=true Answered=true", m, ok)
	}
	if m, ok := byID[ordinary.ID]; !ok || m.ExpectsReply {
		t.Errorf("ordinary message in ListSent = %+v (ok=%v), want ExpectsReply=false", m, ok)
	}
	if m, ok := byID[ordinary.ID]; !ok || !m.Read {
		t.Errorf("ordinary message Read = %v (ok=%v), want true (it was marked read)", m.Read, ok)
	}

	outstanding, err := p.ListSent("alice", true)
	if err != nil {
		t.Fatalf("ListSent(outstanding): %v", err)
	}
	if len(outstanding) != 1 || outstanding[0].ID != unanswered.ID {
		t.Fatalf("ListSent(alice, true) = %v, want exactly [unanswered] — this is the set the retention sweep is holding open (F5: must be listable, not silently accumulated)", outstanding)
	}
}

func TestListSent_EmptySenderErrors(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	if _, err := p.ListSent("", false); err == nil {
		t.Fatal("ListSent(\"\") should error, not silently return all senders' mail")
	}
}
