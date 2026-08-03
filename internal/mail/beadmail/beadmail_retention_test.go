package beadmail

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/mail"
)

// readMailSeed builds a seed Bead for NewMemStoreFrom representing an open read
// message bead created at createdAt. opts mutate the bead (e.g. drop the "read"
// label or mark it closed) so a single helper covers every candidate variant.
func readMailSeed(id string, createdAt time.Time, opts ...func(*beads.Bead)) beads.Bead {
	b := beads.Bead{
		ID:        id,
		Type:      "message",
		Status:    "open",
		Labels:    []string{"read"},
		CreatedAt: createdAt,
	}
	for _, opt := range opts {
		opt(&b)
	}
	return b
}

// closeErrStore errors on Close for the configured IDs, exercising the
// per-bead (non-fatal) error path of the retention sweep.
type closeErrStore struct {
	*beads.MemStore
	failClose map[string]error
}

func (s closeErrStore) Close(id string) error {
	if err, ok := s.failClose[id]; ok {
		return err
	}
	return s.MemStore.Close(id)
}

// listErrStore errors on any message-typed List, exercising the fatal
// candidate-listing error path of the retention sweep.
type listErrStore struct {
	*beads.MemStore
	err error
}

func (s listErrStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type == "message" {
		return nil, s.err
	}
	return s.MemStore.List(query)
}

// deleteTrackStore records every successful Delete and can be told to fail a
// specific ID, mirroring the wisp-GC test double for the purge path.
type deleteTrackStore struct {
	*beads.MemStore
	failDelete map[string]error
	deleted    []string
}

func (s *deleteTrackStore) Delete(id string) error {
	if err, ok := s.failDelete[id]; ok {
		return err
	}
	if err := s.MemStore.Delete(id); err != nil {
		return err
	}
	s.deleted = append(s.deleted, id)
	return nil
}

func TestSweepReadMessagesBefore_ClosesAgedReadMailWithReason(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now
	old := now.Add(-time.Minute)
	fresh := now.Add(time.Minute)

	seed := []beads.Bead{
		readMailSeed("old-1", old),
		readMailSeed("old-2", old),
		readMailSeed("fresh", fresh),
		readMailSeed("unread", old, func(b *beads.Bead) { b.Labels = nil }),
		readMailSeed("already-closed", old, func(b *beads.Bead) { b.Status = "closed" }),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	mailStore := beads.MailStore{Store: store}

	const reason = "mail gc-swept: test retention reason padded to length"
	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, cutoff, 0, reason)
	if listErr != nil {
		t.Fatalf("unexpected list error: %v", listErr)
	}
	if len(closeErrs) != 0 {
		t.Fatalf("unexpected per-bead errors: %v", closeErrs)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2", closed)
	}

	for _, id := range []string{"old-1", "old-2"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "closed" {
			t.Errorf("%s status = %q, want closed", id, b.Status)
		}
		if got := b.Metadata["close_reason"]; got != reason {
			t.Errorf("%s close_reason = %q, want %q", id, got, reason)
		}
	}

	for _, id := range []string{"fresh", "unread"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "open" {
			t.Errorf("%s status = %q, want open (must not be swept)", id, b.Status)
		}
		if _, ok := b.Metadata["close_reason"]; ok {
			t.Errorf("%s unexpectedly stamped close_reason", id)
		}
	}
}

func TestSweepReadMessagesBefore_LimitCapsCloses(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)

	seed := []beads.Bead{
		readMailSeed("old-1", old),
		readMailSeed("old-2", old),
		readMailSeed("old-3", old),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	mailStore := beads.MailStore{Store: store}

	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, now, 2, "reason padded to twenty plus characters")
	if listErr != nil || len(closeErrs) != 0 {
		t.Fatalf("unexpected errors: list=%v perBead=%v", listErr, closeErrs)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2 (limit)", closed)
	}

	openCount := 0
	all, err := store.List(beads.ListQuery{Type: "message", Label: "read", TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		if b.Status == "open" {
			openCount++
		}
	}
	if openCount != 1 {
		t.Fatalf("open read beads = %d, want 1 (limit left one)", openCount)
	}
}

func TestSweepReadMessagesBefore_PerBeadCloseErrorIsCollected(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)

	// good is older so the created_asc sweep visits it first; both are aged.
	seed := []beads.Bead{
		readMailSeed("good", old.Add(-time.Minute)),
		readMailSeed("bad", old),
	}
	base := beads.NewMemStoreFrom(100, seed, nil)
	store := closeErrStore{MemStore: base, failClose: map[string]error{"bad": errors.New("close boom")}}
	mailStore := beads.MailStore{Store: store}

	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, now, 0, "reason padded to twenty plus characters")
	if listErr != nil {
		t.Fatalf("unexpected list error: %v", listErr)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 (good only)", closed)
	}
	if len(closeErrs) != 1 {
		t.Fatalf("closeErrs = %v, want exactly one", closeErrs)
	}
	if got := closeErrs[0].Error(); !strings.Contains(got, "bad") || !strings.Contains(got, "close boom") {
		t.Fatalf("closeErrs[0] = %q, want it to name the bead and the close failure", got)
	}
}

func TestSweepReadMessagesBefore_ListErrorIsFatal(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := listErrStore{MemStore: beads.NewMemStore(), err: errors.New("store down")}
	mailStore := beads.MailStore{Store: store}

	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, now, 0, "reason padded to twenty plus characters")
	if listErr == nil {
		t.Fatal("expected fatal list error")
	}
	if closed != 0 || len(closeErrs) != 0 {
		t.Fatalf("closed=%d closeErrs=%v, want zero on list failure", closed, closeErrs)
	}
}

func TestCountReadMessagesBefore_CountsWithoutMutating(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)
	fresh := now.Add(time.Minute)

	seed := []beads.Bead{
		readMailSeed("old-1", old),
		readMailSeed("old-2", old),
		readMailSeed("fresh", fresh),
		readMailSeed("unread", old, func(b *beads.Bead) { b.Labels = nil }),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	mailStore := beads.MailStore{Store: store}

	count, err := CountReadMessagesBefore(mailStore, now, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	// No mutation: every seeded bead is still open.
	for _, id := range []string{"old-1", "old-2", "fresh", "unread"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "open" {
			t.Errorf("%s status = %q, count must not mutate", id, b.Status)
		}
	}
}

func TestCountReadMessagesBefore_LimitCapsCount(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)
	seed := []beads.Bead{
		readMailSeed("old-1", old),
		readMailSeed("old-2", old),
		readMailSeed("old-3", old),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	mailStore := beads.MailStore{Store: store}

	count, err := CountReadMessagesBefore(mailStore, now, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2 (limit)", count)
	}
}

func TestPurgeReadMessageWisps_DeletesAgedReadWisps(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-time.Hour)
	aged := now.Add(-2 * time.Hour)
	recent := now.Add(-30 * time.Minute)

	wisp := func(id string, createdAt time.Time, meta map[string]string) beads.Bead {
		return beads.Bead{ID: id, Type: "message", Status: "open", CreatedAt: createdAt, Metadata: meta, Ephemeral: true}
	}
	seed := []beads.Bead{
		wisp("read-old", aged, map[string]string{mail.ReadMetadataKey: "true"}),
		wisp("unread-old", aged, map[string]string{mail.ReadMetadataKey: "false"}),
		wisp("unset-old", aged, nil),
		wisp("read-recent", recent, map[string]string{mail.ReadMetadataKey: "true"}),
		// Main-tier read message: excluded by the TierWisps query.
		{ID: "read-main", Type: "message", Status: "open", CreatedAt: aged, Metadata: map[string]string{mail.ReadMetadataKey: "true"}},
		// Wisp-tier but not a message bead: excluded by Type=message.
		{ID: "read-task-wisp", Type: "task", Status: "open", CreatedAt: aged, Metadata: map[string]string{mail.ReadMetadataKey: "true"}, Ephemeral: true},
	}
	store := &deleteTrackStore{MemStore: beads.NewMemStoreFrom(100, seed, nil), failDelete: map[string]error{}}
	mailStore := beads.MailStore{Store: store}

	purged, err := PurgeReadMessageWisps(mailStore, cutoff)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "read-old" {
		t.Fatalf("deleted = %v, want [read-old]", store.deleted)
	}
	for _, id := range []string{"unread-old", "unset-old", "read-recent", "read-main", "read-task-wisp"} {
		if _, err := store.Get(id); err != nil {
			t.Errorf("%s should be preserved: %v", id, err)
		}
	}
}

func TestPurgeReadMessageWisps_DeleteErrorSurfacedAndContinues(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-time.Hour)
	aged := now.Add(-2 * time.Hour)

	wisp := func(id string) beads.Bead {
		return beads.Bead{ID: id, Type: "message", Status: "open", CreatedAt: aged, Metadata: map[string]string{mail.ReadMetadataKey: "true"}, Ephemeral: true}
	}
	store := &deleteTrackStore{
		MemStore:   beads.NewMemStoreFrom(100, []beads.Bead{wisp("bad"), wisp("good")}, nil),
		failDelete: map[string]error{"bad": errors.New("delete boom")},
	}
	mailStore := beads.MailStore{Store: store}

	purged, err := PurgeReadMessageWisps(mailStore, cutoff)
	if err == nil {
		t.Fatal("expected delete error to be surfaced")
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 (good deleted)", purged)
	}
	if !contains(store.deleted, "good") {
		t.Fatalf("deleted = %v, want to include good", store.deleted)
	}
}

func TestPurgeReadMessageWisps_ListErrorSurfaced(t *testing.T) {
	store := listErrStore{MemStore: beads.NewMemStore(), err: errors.New("store down")}
	mailStore := beads.MailStore{Store: store}
	purged, err := PurgeReadMessageWisps(mailStore, time.Now())
	if err == nil {
		t.Fatal("expected list error to be surfaced")
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0", purged)
	}
}

func TestIsMessageBead(t *testing.T) {
	if !IsMessageBead(beads.Bead{Type: "message"}) {
		t.Error("Type=message must be a message bead")
	}
	if IsMessageBead(beads.Bead{Type: "task"}) {
		t.Error("Type=task must not be a message bead")
	}
	if IsMessageBead(beads.Bead{}) {
		t.Error("empty-type bead must not be a message bead")
	}

	// A message bead that also carries wisp metadata is still a message bead:
	// IsMessageBead is a bare Type check, deliberately NOT coordclass.Classify
	// (which would route the wisp-marked bead to ClassGraph). This preserves the
	// historical inline `b.Type == "message"` behavior at the order single-flight
	// gate.
	wispMsg := beads.Bead{Type: "message", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}}
	if !IsMessageBead(wispMsg) {
		t.Error("wisp-marked message bead must still report true")
	}
	if coordclass.Classify(wispMsg) != coordclass.ClassGraph {
		t.Fatalf("precondition: expected wisp-marked message to Classify as ClassGraph, got %v", coordclass.Classify(wispMsg))
	}
}

// TestRetentionSweptReadMailStaysAddressableUntilPurge pins the boundary between
// system-aged mail and user-removed mail through the Provider surface. The
// always-on nudge-mail watchdog closes read mail past its TTL (stamping
// RetentionSweepCloseReason) and PurgeReadMessageWisps deletes it later; during
// that closed-but-not-purged window the message must stay addressable by direct
// ID, matching pre-sweep behavior, so a caller holding the message ID still
// resolves it. Only a message bead closed for a non-retention reason (a legacy
// close-on-archive user removal) is not-found. This ties SweepReadMessagesBefore
// to Provider.Get/Read/Reply so a future edit to isRemovedMessageBead cannot
// silently diverge the retention path from the read path.
func TestRetentionSweptReadMailStaysAddressableUntilPurge(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("alice", "bob", "aged", "read long ago")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Mark it read so the retention sweep treats it as a candidate.
	if _, err := p.Read(sent.ID); err != nil {
		t.Fatalf("Read before sweep: %v", err)
	}

	// The retention sweep closes the aged read mail with the canonical reason,
	// exactly as the production nudge-mail watchdog does.
	closed, closeErrs, listErr := SweepReadMessagesBefore(
		beads.MailStore{Store: store}, time.Now().Add(time.Hour), 0, RetentionSweepCloseReason)
	if listErr != nil {
		t.Fatalf("sweep list error: %v", listErr)
	}
	if len(closeErrs) != 0 {
		t.Fatalf("sweep per-bead errors: %v", closeErrs)
	}
	if closed != 1 {
		t.Fatalf("swept %d beads, want 1", closed)
	}

	// Precondition: the bead is closed and carries the retention marker.
	raw, err := store.Get(sent.ID)
	if err != nil {
		t.Fatalf("store.Get after sweep: %v", err)
	}
	if raw.Status != "closed" || raw.Metadata["close_reason"] != RetentionSweepCloseReason {
		t.Fatalf("swept bead status=%q close_reason=%q, want closed / %q",
			raw.Status, raw.Metadata["close_reason"], RetentionSweepCloseReason)
	}

	// Retention-swept mail stays addressable by direct ID until purge.
	if _, err := p.Get(sent.ID); err != nil {
		t.Errorf("Get(retention-swept) = %v, want addressable", err)
	}
	if _, err := p.Read(sent.ID); err != nil {
		t.Errorf("Read(retention-swept) = %v, want addressable", err)
	}
	reply, err := p.Reply(sent.ID, "bob", "RE: aged", "still replying after retention")
	if err != nil {
		t.Fatalf("Reply(retention-swept) = %v, want addressable", err)
	}
	if reply.ID == "" {
		t.Error("Reply(retention-swept) returned an empty message")
	}

	// But it is retired from the active list views, which already gate on open
	// status — the same asymmetry as before this PR.
	inbox, err := p.Inbox("bob")
	if err != nil {
		t.Fatalf("Inbox after sweep: %v", err)
	}
	for _, m := range inbox {
		if m.ID == sent.ID {
			t.Errorf("Inbox surfaced retention-swept message %q", sent.ID)
		}
	}

	// Contrast: a message bead closed for a non-retention reason is a user
	// removal and must be not-found through the same direct-ID operations.
	removed, err := p.Send("alice", "bob", "removed", "closed by a non-retention path")
	if err != nil {
		t.Fatalf("Send removed: %v", err)
	}
	if err := store.SetMetadata(removed.ID, "close_reason", "manual removal: legacy close-on-archive path"); err != nil {
		t.Fatalf("SetMetadata removed: %v", err)
	}
	if err := store.Close(removed.ID); err != nil {
		t.Fatalf("Close removed: %v", err)
	}
	if _, err := p.Get(removed.ID); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("Get(non-retention closed) = %v, want ErrNotFound", err)
	}
	if _, err := p.Reply(removed.ID, "bob", "too late", "must not create"); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("Reply(non-retention closed) = %v, want ErrNotFound", err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// expectsReplyAskSeed builds a seed Bead for an open, read, aged message
// marked mail.expects_reply=true — the retention-exemption candidate shape.
func expectsReplyAskSeed(id string, createdAt time.Time) beads.Bead {
	return beads.Bead{
		ID:        id,
		Type:      "message",
		Status:    "open",
		Labels:    []string{"read"},
		CreatedAt: createdAt,
		Metadata:  map[string]string{mail.ExpectsReplyMetadataKey: "true"},
	}
}

// replyToSeed builds a seed Bead labelled reply-to:askID, satisfying
// HasReply(askID). closed marks it retention-swept-closed, exercising F2
// (closed-reply blindness).
func replyToSeed(id, askID string, createdAt time.Time, closed bool) beads.Bead {
	b := beads.Bead{
		ID:        id,
		Type:      "message",
		Status:    "open",
		Labels:    []string{"thread:t", "reply-to:" + askID},
		CreatedAt: createdAt,
	}
	if closed {
		b.Status = "closed"
	}
	return b
}

// exemptSetErrStore errors only on the markedExpectsReplyIDs query (Type +
// Metadata[expects_reply]), leaving the ordinary read-message candidate query
// (Label="read") and any reply-to: HasReply query untouched — isolating the
// exempt-set-lookup failure path from the candidate-listing failure path
// listErrStore already covers.
type exemptSetErrStore struct {
	*beads.MemStore
	err error
}

func (s exemptSetErrStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type == "message" && query.Metadata[mail.ExpectsReplyMetadataKey] == "true" {
		return nil, s.err
	}
	return s.MemStore.List(query)
}

// hasReplyErrStore errors only on a HasReply-shaped query (Label prefixed
// reply-to:), leaving every other query (candidate listing, exempt-set
// lookup) untouched — isolates the per-candidate answered-check failure path.
type hasReplyErrStore struct {
	*beads.MemStore
	err error
}

func (s hasReplyErrStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if strings.HasPrefix(query.Label, "reply-to:") {
		return nil, s.err
	}
	return s.MemStore.List(query)
}

// replyToCountingStore counts List calls shaped like a HasReply query (Label
// prefixed reply-to:), so a test can assert the exemption logic queries
// HasReply only for candidates that intersect the marked-expects-reply set
// (ga-eibq22 F4), not once per read-mail candidate.
type replyToCountingStore struct {
	*beads.MemStore
	hasReplyQueries int
}

func (s *replyToCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if strings.HasPrefix(query.Label, "reply-to:") {
		s.hasReplyQueries++
	}
	return s.MemStore.List(query)
}

func TestSweepReadMessagesBefore_RetainsUnansweredExpectsReplyAsk(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)

	seed := []beads.Bead{
		expectsReplyAskSeed("ask-unanswered", old),
		readMailSeed("ordinary", old),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	mailStore := beads.MailStore{Store: store}

	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, now, 0, "reason padded to twenty plus characters")
	if listErr != nil || len(closeErrs) != 0 {
		t.Fatalf("unexpected errors: list=%v perBead=%v", listErr, closeErrs)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 (ordinary only)", closed)
	}

	ask, err := store.Get("ask-unanswered")
	if err != nil {
		t.Fatalf("Get(ask-unanswered): %v", err)
	}
	if ask.Status != "open" {
		t.Errorf("unanswered expects-reply ask status = %q, want open (must be retained)", ask.Status)
	}

	ordinary, err := store.Get("ordinary")
	if err != nil {
		t.Fatalf("Get(ordinary): %v", err)
	}
	if ordinary.Status != "closed" {
		t.Errorf("ordinary read mail status = %q, want closed (opt-in exemption must not regress ordinary retention)", ordinary.Status)
	}
}

func TestSweepReadMessagesBefore_ClosesAnsweredExpectsReplyAsk(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)

	seed := []beads.Bead{
		expectsReplyAskSeed("ask-answered", old),
		replyToSeed("reply-1", "ask-answered", old.Add(time.Second), false),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	mailStore := beads.MailStore{Store: store}

	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, now, 0, "reason padded to twenty plus characters")
	if listErr != nil || len(closeErrs) != 0 {
		t.Fatalf("unexpected errors: list=%v perBead=%v", listErr, closeErrs)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 (answered ask closes like ordinary mail)", closed)
	}
	ask, err := store.Get("ask-answered")
	if err != nil {
		t.Fatalf("Get(ask-answered): %v", err)
	}
	if ask.Status != "closed" {
		t.Errorf("answered expects-reply ask status = %q, want closed", ask.Status)
	}
}

// TestSweepReadMessagesBefore_ClosedReplyStillCountsAsAnswered pins F2: the
// reply is itself mail and gets read-swept closed by this same mechanism, so
// HasReply's IncludeClosed:true must see it — an open-only query would make
// every answered ask outside its own reply's TTL window read as unanswered
// and retained forever.
func TestSweepReadMessagesBefore_ClosedReplyStillCountsAsAnswered(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)

	seed := []beads.Bead{
		expectsReplyAskSeed("ask-closed-reply", old),
		replyToSeed("reply-closed", "ask-closed-reply", old.Add(time.Second), true /* closed */),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	mailStore := beads.MailStore{Store: store}

	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, now, 0, "reason padded to twenty plus characters")
	if listErr != nil || len(closeErrs) != 0 {
		t.Fatalf("unexpected errors: list=%v perBead=%v", listErr, closeErrs)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 (ask must close: its reply exists even though the reply itself is closed)", closed)
	}
}

func TestSweepReadMessagesBefore_HasReplyErrorRetainsCandidateAndSurfacesError(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)

	seed := []beads.Bead{expectsReplyAskSeed("ask-erroring", old)}
	base := beads.NewMemStoreFrom(100, seed, nil)
	store := hasReplyErrStore{MemStore: base, err: errors.New("reply lookup boom")}
	mailStore := beads.MailStore{Store: store}

	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, now, 0, "reason padded to twenty plus characters")
	if listErr != nil {
		t.Fatalf("unexpected fatal list error: %v", listErr)
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 (fail-safe: retain on HasReply error)", closed)
	}
	if len(closeErrs) != 1 || !strings.Contains(closeErrs[0].Error(), "reply lookup boom") {
		t.Fatalf("closeErrs = %v, want one error naming the HasReply failure (observable, not silent)", closeErrs)
	}
	ask, err := base.Get("ask-erroring")
	if err != nil {
		t.Fatalf("Get(ask-erroring): %v", err)
	}
	if ask.Status != "open" {
		t.Errorf("ask status = %q, want open (retained on error)", ask.Status)
	}
}

func TestSweepReadMessagesBefore_ExemptSetListErrorIsFatal(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)
	seed := []beads.Bead{readMailSeed("ordinary", old)}
	base := beads.NewMemStoreFrom(100, seed, nil)
	store := exemptSetErrStore{MemStore: base, err: errors.New("exempt set store down")}
	mailStore := beads.MailStore{Store: store}

	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, now, 0, "reason padded to twenty plus characters")
	if listErr == nil {
		t.Fatal("expected fatal error when the exempt-set query fails")
	}
	if closed != 0 || len(closeErrs) != 0 {
		t.Fatalf("closed=%d closeErrs=%v, want zero on exempt-set list failure", closed, closeErrs)
	}
}

func TestSweepReadMessagesBefore_HasReplyQueriedOnlyForMarkedCandidates(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)

	seed := []beads.Bead{
		readMailSeed("ordinary-1", old),
		readMailSeed("ordinary-2", old),
		readMailSeed("ordinary-3", old),
		expectsReplyAskSeed("ask-1", old),
	}
	store := &replyToCountingStore{MemStore: beads.NewMemStoreFrom(100, seed, nil)}
	mailStore := beads.MailStore{Store: store}

	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, now, 0, "reason padded to twenty plus characters")
	if listErr != nil || len(closeErrs) != 0 {
		t.Fatalf("unexpected errors: list=%v perBead=%v", listErr, closeErrs)
	}
	if closed != 3 {
		t.Fatalf("closed = %d, want 3 (the 3 ordinary beads)", closed)
	}
	if store.hasReplyQueries != 1 {
		t.Fatalf("HasReply-shaped queries = %d, want exactly 1 (only the marked candidate) — "+
			"cost must scale with open asks, not the candidate/close budget (F4)", store.hasReplyQueries)
	}
}

func TestCountReadMessagesBefore_ExcludesRetainedExpectsReplyAsk(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)

	seed := []beads.Bead{
		expectsReplyAskSeed("ask-unanswered", old),
		readMailSeed("ordinary", old),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	mailStore := beads.MailStore{Store: store}

	count, err := CountReadMessagesBefore(mailStore, now, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1 — CountReadMessagesBefore must stay in lockstep with what SweepReadMessagesBefore would actually close", count)
	}
}

func TestPurgeReadMessageWisps_RetainsUnansweredExpectsReplyAsk(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-time.Hour)
	aged := now.Add(-2 * time.Hour)

	seed := []beads.Bead{
		{
			ID: "ask-unanswered", Type: "message", Status: "open", CreatedAt: aged, Ephemeral: true,
			Metadata: map[string]string{mail.ReadMetadataKey: "true", mail.ExpectsReplyMetadataKey: "true"},
		},
		{
			ID: "ordinary", Type: "message", Status: "open", CreatedAt: aged, Ephemeral: true,
			Metadata: map[string]string{mail.ReadMetadataKey: "true"},
		},
	}
	store := &deleteTrackStore{MemStore: beads.NewMemStoreFrom(100, seed, nil), failDelete: map[string]error{}}
	mailStore := beads.MailStore{Store: store}

	purged, err := PurgeReadMessageWisps(mailStore, cutoff)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 (ordinary only)", purged)
	}
	if _, err := store.Get("ask-unanswered"); err != nil {
		t.Errorf("unanswered expects-reply ask must survive the purge (second retirement path, F5): %v", err)
	}
	if contains(store.deleted, "ask-unanswered") {
		t.Error("unanswered expects-reply ask must not be deleted")
	}
}

func TestPurgeReadMessageWisps_DeletesAnsweredExpectsReplyAsk(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-time.Hour)
	aged := now.Add(-2 * time.Hour)

	seed := []beads.Bead{
		{
			ID: "ask-answered", Type: "message", Status: "open", CreatedAt: aged, Ephemeral: true,
			Metadata: map[string]string{mail.ReadMetadataKey: "true", mail.ExpectsReplyMetadataKey: "true"},
		},
		{
			ID: "reply-1", Type: "message", Status: "open", CreatedAt: aged.Add(time.Second),
			Labels: []string{"reply-to:ask-answered"},
		},
	}
	store := &deleteTrackStore{MemStore: beads.NewMemStoreFrom(100, seed, nil), failDelete: map[string]error{}}
	mailStore := beads.MailStore{Store: store}

	purged, err := PurgeReadMessageWisps(mailStore, cutoff)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if purged != 1 || !contains(store.deleted, "ask-answered") {
		t.Fatalf("purged = %d, deleted = %v, want the answered ask purged", purged, store.deleted)
	}
}
