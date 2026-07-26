package session

import (
	"strconv"
	"strings"
	"time"
)

// Exit classification for dead sessions, extracted from the reconciler's
// stability/churn pair. Pure decision ladder over caller-gathered facts:
// the reconciler owns the drain tracker, provider screen peeks, store
// writes, and counters; this package owns lane precedence and the band
// arithmetic.
//
// Lanes, in order:
//
//  1. rate-limit: a crash candidate whose provider screen shows a rate-limit
//     message is quarantined without counting a crash.
//  2. rapid crash: a crash candidate that died inside the stability window.
//  3. churn band: a death that survived the stability window but did not
//     reach the productivity threshold (context-exhaustion death spiral).
//     Deaths at or past the productivity threshold clear the churn counter.
//
// The rapid-crash lanes intentionally ignore pending_create_claim and
// sleep_reason; the churn lane respects both. That asymmetry is inherited
// reconciler behavior: an expired-lease pending create must retry its wake
// (not accrue churn), while a genuine rapid crash counts even when a stale
// claim is still set.

// ScreenFact is the tri-state provider-screen fact. It is Unknown until the
// caller has peeked the session's terminal content for a rate-limit screen.
type ScreenFact int

// Provider-screen fact states.
const (
	ScreenUnknown ScreenFact = iota
	ScreenRateLimit
	ScreenOther
)

// ExitOutcome is the classification of one dead session on one tick.
type ExitOutcome int

const (
	// ExitNone means nothing to record for this session.
	ExitNone ExitOutcome = iota
	// ExitGatherScreen means supply ExitFacts.Screen and decide again.
	ExitGatherScreen
	// ExitRateLimitQuarantine means back the session off with a rate-limit
	// quarantine instead of counting a crash.
	ExitRateLimitQuarantine
	// ExitRapidCrash means record a wake failure and clear the wake stamp.
	ExitRapidCrash
	// ExitChurn means record a churn event and clear the wake stamp.
	ExitChurn
	// ExitProductiveDeath means the session died after doing useful work;
	// clear any accrued churn count and continue processing the session.
	ExitProductiveDeath
)

// ExitFacts are the inputs for classifying one dead session's exit.
type ExitFacts struct {
	// Alive short-circuits the ladder; live sessions are never classified.
	Alive bool
	// SubprocessProvider reports the city runs the subprocess session
	// provider, whose workers exit intentionally after a unit of work.
	SubprocessProvider bool
	// DrainPending reports an intentional drain is tracked for the session.
	DrainPending bool
	// PendingCreateClaim reports pending_create_claim=true on the bead.
	// Suppresses churn only; see the package comment for the asymmetry.
	PendingCreateClaim bool
	// PendingCreateStartInFlight reports the create lease is still live, so
	// the death is startup noise rather than a crash.
	PendingCreateStartInFlight bool
	// SleepReason is the bead's sleep_reason metadata. Deliberate reasons
	// suppress churn only.
	SleepReason string
	// LastWokeAt is the raw last_woke_at metadata (RFC3339). Empty or
	// unparseable values disable exit tracking entirely.
	LastWokeAt string
	// Now is the tick clock reading.
	Now time.Time
	// StabilityThreshold bounds the rapid-crash window; deaths inside it
	// count as wake failures.
	StabilityThreshold time.Duration
	// ProductivityThreshold bounds the churn band; deaths at or past it are
	// productive.
	ProductivityThreshold time.Duration
	// ScreenAvailable reports the caller can peek the provider screen.
	// Without it the rate-limit lane is skipped.
	ScreenAvailable bool
	// Screen is the provider-screen fact, gathered on demand.
	Screen ScreenFact
}

// DecideSessionExit classifies a dead session's exit. It performs no I/O;
// when the rate-limit lane needs the provider screen it returns
// ExitGatherScreen and the caller supplies the fact and decides again.
func DecideSessionExit(f ExitFacts) ExitOutcome {
	if f.Alive {
		return ExitNone
	}
	woke, wokeValid := parseWakeStamp(f.LastWokeAt)
	crashCandidate := !f.SubprocessProvider && !f.DrainPending && wokeValid &&
		!f.PendingCreateStartInFlight
	if crashCandidate && f.ScreenAvailable {
		switch f.Screen {
		case ScreenUnknown:
			return ExitGatherScreen
		case ScreenRateLimit:
			return ExitRateLimitQuarantine
		}
	}
	if crashCandidate && f.Now.Sub(woke) < f.StabilityThreshold {
		return ExitRapidCrash
	}
	if f.PendingCreateClaim || f.SubprocessProvider || f.DrainPending ||
		IsDeliberateSleepReason(f.SleepReason) || !wokeValid {
		return ExitNone
	}
	elapsed := f.Now.Sub(woke)
	if elapsed < f.StabilityThreshold {
		return ExitNone
	}
	if elapsed >= f.ProductivityThreshold {
		return ExitProductiveDeath
	}
	return ExitChurn
}

func parseWakeStamp(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// ExitAccrual is the built write for one more crash or churn event plus the
// threshold verdict. Quarantined accruals carry the full quarantine patch;
// otherwise only the counter advances.
type ExitAccrual struct {
	Patch       MetadataPatch
	Quarantined bool
}

// WakeFailureAccrualPatch builds the write for one more rapid-crash wake
// failure on top of priorAttempts. Reaching maxAttempts quarantines the
// session until the given time with sleep reason "quarantine". The patch is
// metadata-only; unlike QuarantinePatch it does not move the state machine.
func WakeFailureAccrualPatch(priorAttempts, maxAttempts int, until time.Time) ExitAccrual {
	return exitAccrual("wake_attempts", priorAttempts+1, maxAttempts, string(SleepReasonQuarantine), until)
}

// ChurnAccrualPatch builds the write for one more churn cycle on top of
// priorCycles. Reaching maxCycles quarantines the session until the given
// time with sleep reason "context-churn".
func ChurnAccrualPatch(priorCycles, maxCycles int, until time.Time) ExitAccrual {
	return exitAccrual("churn_count", priorCycles+1, maxCycles, string(SleepReasonContextChurn), until)
}

func exitAccrual(counterKey string, next, threshold int, sleepReason string, until time.Time) ExitAccrual {
	count := strconv.Itoa(next)
	if next >= threshold {
		return ExitAccrual{
			Quarantined: true,
			Patch: MetadataPatch{
				counterKey:          count,
				"quarantined_until": until.UTC().Format(time.RFC3339),
				"sleep_reason":      sleepReason,
			},
		}
	}
	return ExitAccrual{Patch: MetadataPatch{counterKey: count}}
}

// ConversationResetPatch clears the runtime conversation binding so the next
// wake starts a fresh conversation. Wake failures also clear
// started_config_hash so the next start runs as a first start instead of
// resuming a conversation that no longer exists; churn keeps the hash.
func ConversationResetPatch(clearStartedConfigHash bool) MetadataPatch {
	patch := MetadataPatch{
		"session_key":                "",
		"continuation_reset_pending": "true",
	}
	if clearStartedConfigHash {
		patch["started_config_hash"] = ""
		// Priming markers share started_config_hash's lifetime (S19 Stage 2): a
		// wake failure re-primes; a churn keeps the hash and its markers.
		clearPrimingMarkers(patch)
	}
	return patch
}

// RateLimitQuarantinePatch backs a session off a provider rate-limit screen
// until the given time without counting a crash or resetting its
// conversation metadata.
func RateLimitQuarantinePatch(until time.Time) MetadataPatch {
	return MetadataPatch{
		"state":                     string(StateAsleep),
		"quarantined_until":         until.UTC().Format(time.RFC3339),
		"sleep_reason":              string(SleepReasonRateLimit),
		"last_woke_at":              "",
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}
}

// ProviderResourceExhaustionQuarantinePatch backs a session off a detected
// quota/credit exhaustion condition until the given time, same shape as
// RateLimitQuarantinePatch (does not count a crash, does not reset
// conversation metadata — a topped-up/reset account should resume the same
// conversation, not start fresh). reason is the specific detected condition
// (quota_exceeded, credit_exhausted — runtime.ProviderResourceExhaustionReason's
// return value), recorded separately from the broad sleep_reason class label,
// mirroring how markProviderTerminalError records its own specific reason
// alongside the generic provider-terminal-error sleep_reason.
func ProviderResourceExhaustionQuarantinePatch(until time.Time, reason string) MetadataPatch {
	return MetadataPatch{
		"state":                               string(StateAsleep),
		"quarantined_until":                   until.UTC().Format(time.RFC3339),
		"sleep_reason":                        string(SleepReasonProviderResourceExhausted),
		"provider_resource_exhaustion_reason": strings.TrimSpace(reason),
		"last_woke_at":                        "",
		"pending_create_claim":                "",
		"pending_create_started_at":           "",
	}
}

// LoginExpiredQuarantinePatch backs a session off a detected login/auth-expiry
// prompt until the given time, same shape as RateLimitQuarantinePatch and
// ProviderResourceExhaustionQuarantinePatch — not a crash, conversation
// metadata preserved so a re-authenticated seat resumes the same
// conversation rather than restarting from zero (the ga-uwptpu incident this
// whole durability wave traces back to). Kept as its own dedicated function
// rather than folded into ProviderResourceExhaustionQuarantinePatch: distinct
// sleep_reason per the operator's ruling on ga-5gsyts (S-metrics need to
// track login-expiry match quality separately from quota/credit exhaustion).
func LoginExpiredQuarantinePatch(until time.Time) MetadataPatch {
	return MetadataPatch{
		"state":                     string(StateAsleep),
		"quarantined_until":         until.UTC().Format(time.RFC3339),
		"sleep_reason":              string(SleepReasonLoginExpired),
		"last_woke_at":              "",
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}
}
