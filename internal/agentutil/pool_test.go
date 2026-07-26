package agentutil

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

type partialSessionLister struct {
	running []string
	err     error
}

func (p partialSessionLister) ListRunning(prefix string) ([]string, error) {
	var filtered []string
	for _, name := range p.running {
		if len(prefix) == 0 || len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			filtered = append(filtered, name)
		}
	}
	return filtered, p.err
}

func TestExpandAgentsFixedAgent(t *testing.T) {
	agents := []config.Agent{
		{Name: "mayor", MaxActiveSessions: intPtr(1)},
	}
	result := ExpandAgents(agents, "city", "", nil)
	if len(result) != 1 {
		t.Fatalf("got %d agents, want 1", len(result))
	}
	if result[0].QualifiedName != "mayor" {
		t.Errorf("name = %q, want mayor", result[0].QualifiedName)
	}
	if result[0].Pool != "" {
		t.Errorf("pool = %q, want empty", result[0].Pool)
	}
}

func TestExpandAgentsBoundedPool(t *testing.T) {
	agents := []config.Agent{
		{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(3)},
	}
	result := ExpandAgents(agents, "city", "", nil)
	if len(result) != 3 {
		t.Fatalf("got %d agents, want 3", len(result))
	}
	if result[0].QualifiedName != "myrig/polecat-1" {
		t.Errorf("[0] name = %q, want myrig/polecat-1", result[0].QualifiedName)
	}
	if result[0].Pool != "myrig/polecat" {
		t.Errorf("[0] pool = %q, want myrig/polecat", result[0].Pool)
	}
	if result[2].QualifiedName != "myrig/polecat-3" {
		t.Errorf("[2] name = %q, want myrig/polecat-3", result[2].QualifiedName)
	}
}

func TestExpandAgentsCanonicalSingletonPoolUsesBaseName(t *testing.T) {
	agents := []config.Agent{
		{Name: "worker", Dir: "myrig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
	}
	result := ExpandAgents(agents, "city", "", nil)
	if len(result) != 1 {
		t.Fatalf("got %d agents, want 1", len(result))
	}
	if result[0].QualifiedName != "myrig/worker" {
		t.Errorf("name = %q, want myrig/worker", result[0].QualifiedName)
	}
}

func TestExpandAgentsNamepool(t *testing.T) {
	agents := []config.Agent{
		{
			Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(2),
			NamepoolNames: []string{"alpha", "beta"},
		},
	}
	result := ExpandAgents(agents, "city", "", nil)
	if len(result) != 2 {
		t.Fatalf("got %d agents, want 2", len(result))
	}
	if result[0].QualifiedName != "myrig/alpha" {
		t.Errorf("[0] = %q, want myrig/alpha", result[0].QualifiedName)
	}
	if result[1].QualifiedName != "myrig/beta" {
		t.Errorf("[1] = %q, want myrig/beta", result[1].QualifiedName)
	}
}

func TestExpandAgentsMixed(t *testing.T) {
	agents := []config.Agent{
		{Name: "mayor", MaxActiveSessions: intPtr(1)},
		{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(2)},
	}
	result := ExpandAgents(agents, "city", "", nil)
	if len(result) != 3 {
		t.Fatalf("got %d agents, want 3 (1 fixed + 2 pool)", len(result))
	}
}

func TestExpandAgentsSuspended(t *testing.T) {
	agents := []config.Agent{
		{Name: "mayor", MaxActiveSessions: intPtr(1), Suspended: true},
	}
	result := ExpandAgents(agents, "city", "", nil)
	if len(result) != 1 || !result[0].Suspended {
		t.Error("expected suspended agent")
	}
}

func TestExpandAgentsUnlimitedPoolFailsClosedOnPartialListResults(t *testing.T) {
	agents := []config.Agent{
		{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(-1)},
	}
	sp := partialSessionLister{
		running: []string{"myrig--polecat-1", "myrig--polecat-2"},
		err:     &runtime.PartialListError{Err: errors.New("remote backend down")},
	}

	result := ExpandAgents(agents, "city", "", sp)
	if len(result) != 0 {
		t.Fatalf("got %d agents, want fail-closed empty result on partial list", len(result))
	}
}

func TestPoolInstanceName(t *testing.T) {
	a := config.Agent{Name: "polecat", MaxActiveSessions: intPtr(3)}
	if got := PoolInstanceName("polecat", 2, a); got != "polecat-2" {
		t.Errorf("got %q, want polecat-2", got)
	}

	a2 := config.Agent{Name: "polecat", MaxActiveSessions: intPtr(1)}
	if got := PoolInstanceName("polecat", 1, a2); got != "polecat" {
		t.Errorf("single instance: got %q, want polecat", got)
	}

	a2.MinActiveSessions = intPtr(0)
	if got := PoolInstanceName("polecat", 1, a2); got != "polecat" {
		t.Errorf("canonical singleton pool: got %q, want polecat", got)
	}

	a3 := config.Agent{
		Name: "polecat", MaxActiveSessions: intPtr(2),
		NamepoolNames: []string{"alpha", "beta"},
	}
	if got := PoolInstanceName("polecat", 1, a3); got != "alpha" {
		t.Errorf("namepool: got %q, want alpha", got)
	}
}

// seedSessionBead creates a session bead with the BARE metadata keys
// internal/session actually writes (template, session_name) — not the
// beadmeta gc.-prefixed constants findSessionNameByTemplate used to read.
// This is the real shape ga-8jgfy0 found production beads never matched.
func seedSessionBead(t *testing.T, store beads.Store, template, sessionName string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  "session for " + template,
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":     template,
			"session_name": sessionName,
		},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	return b
}

func TestFindSessionNameByTemplateMatchesRealSessionBead(t *testing.T) {
	store := beads.NewMemStore()
	seedSessionBead(t, store, "myrig/worker-1", "city--myrig--worker-1")

	if got := findSessionNameByTemplate(store, "myrig/worker-1"); got != "city--myrig--worker-1" {
		t.Errorf("got %q, want city--myrig--worker-1", got)
	}
}

func TestFindSessionNameByTemplateNoMatchReturnsEmpty(t *testing.T) {
	store := beads.NewMemStore()
	seedSessionBead(t, store, "myrig/other-1", "city--myrig--other-1")

	if got := findSessionNameByTemplate(store, "myrig/worker-1"); got != "" {
		t.Errorf("got %q, want empty (no session bead for this template)", got)
	}
}

func TestFindSessionNameByTemplateIgnoresClosedSession(t *testing.T) {
	store := beads.NewMemStore()
	b := seedSessionBead(t, store, "myrig/worker-1", "city--myrig--worker-1-old")
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("closing session bead: %v", err)
	}

	if got := findSessionNameByTemplate(store, "myrig/worker-1"); got != "" {
		t.Errorf("got %q, want empty (closed session must not be returned)", got)
	}
}

func TestLookupSessionNamePrefersBeadDerivedName(t *testing.T) {
	store := beads.NewMemStore()
	seedSessionBead(t, store, "myrig/worker-1", "custom-bead-derived-name")

	if got := LookupSessionName(store, "city", "myrig/worker-1", ""); got != "custom-bead-derived-name" {
		t.Errorf("got %q, want custom-bead-derived-name", got)
	}
}

func TestLookupSessionNameFallsBackToSynthesisWhenNoBeadFound(t *testing.T) {
	store := beads.NewMemStore()

	got := LookupSessionName(store, "city", "myrig/worker-1", "")
	want := agent.SessionNameFor("city", "myrig/worker-1", "")
	if got != want {
		t.Errorf("got %q, want synthesis fallback %q", got, want)
	}
}
