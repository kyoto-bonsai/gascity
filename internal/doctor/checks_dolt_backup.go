package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// defaultDoltBackupArtifactMaxAge is how stale the newest entry in a scope's
// .dolt-backup/<db>/ directory may be before DoltBackupCheck downgrades an
// otherwise-present backup from OK to a staleness warning. mol-dog-backup
// syncs on a ~6h cadence; 24h gives ~4x slack for a missed cycle or two
// before treating the mechanism as stopped rather than merely late (see
// ga-0avnxn: hq's backup artifact dir sat 12.5 days stale while every other
// rig kept syncing, invisible to this check's prior existence-only signal).
const defaultDoltBackupArtifactMaxAge = 24 * time.Hour

// DoltBackupCheck verifies that a rig's Dolt database has a backup remote
// configured. `gc rig add` provisions the rig but does not register a
// backup. mol-dog-backup auto-configures a local <db>-backup remote on its
// next run (#3176), so this warning self-heals within one backup interval;
// it catches the not-yet-covered window up-front in `gc doctor` and stays
// loud when the backup dog itself is failing.
//
// Two signals satisfy the check; either is sufficient:
//
//   - Filesystem: <city>/.dolt-backup/<db>/ exists with backup contents.
//     mol-dog-backup syncs here, so populated contents are evidence that
//     a sync has run.
//   - Repo state: <managed-dolt-data-dir>/<db>/.dolt/repo_state.json
//     contains a backup entry named <db>-backup. This is the
//     post-registration, pre-sync state.
//
// When both signals are absent the check emits StatusWarning with the
// exact copy-pasteable invocation needed to register and sync the
// backup. We deliberately do NOT auto-fix: backup destination is
// operator policy (local fs vs S3 vs B2 etc.) and a one-way door.
//
// The check is intended to be registered per non-suspended rig; the
// caller in cmd_doctor.go skips suspended rigs before constructing this
// check.
type DoltBackupCheck struct {
	cityPath    string
	rig         config.Rig
	doltDataDir string

	// scopeLabel, when non-empty, overrides the "rig:<name>" prefix in Name()
	// with "<scopeLabel>:dolt-backup". Used for scopes that back this check
	// with a synthetic config.Rig rather than a real one — see
	// NewCityDoltBackupCheck.
	scopeLabel string
	// artifactMaxAge bounds how stale the newest .dolt-backup/<db>/ entry may
	// be before a present-and-nonempty backup dir is downgraded from OK to a
	// staleness warning. Zero means defaultDoltBackupArtifactMaxAge.
	artifactMaxAge time.Duration
	// now is the injectable clock for freshness comparisons; defaults to
	// time.Now. Tests override it for deterministic staleness fixtures.
	now func() time.Time
}

// NewDoltBackupCheck creates a per-rig dolt-backup registration check.
func NewDoltBackupCheck(cityPath string, rig config.Rig, doltDataDir string) *DoltBackupCheck {
	if strings.TrimSpace(doltDataDir) == "" {
		doltDataDir = filepath.Join(cityPath, ".beads", "dolt")
	}
	return &DoltBackupCheck{
		cityPath:       cityPath,
		rig:            rig,
		doltDataDir:    doltDataDir,
		artifactMaxAge: defaultDoltBackupArtifactMaxAge,
		now:            time.Now,
	}
}

// NewCityDoltBackupCheck creates a dolt-backup artifact check for the city's
// own store (hq). The per-rig registration loop in cmd_doctor.go iterates
// cfg.Rigs, which never includes the city itself, so hq previously had no
// dolt-backup artifact check at all — not even the existence-only signal
// every configured rig gets. hq is modeled as a synthetic rig rooted at
// cityPath so the existing resolution logic (metadata.json dolt_database
// lookup, external-endpoint detection, the backup-dir and repo_state.json
// signals) applies unchanged; only Name() differs, via scopeLabel, so the
// check reads "city:dolt-backup" rather than the misleading
// "rig:hq:dolt-backup".
func NewCityDoltBackupCheck(cityPath, doltDataDir string) *DoltBackupCheck {
	c := NewDoltBackupCheck(cityPath, config.Rig{Name: "hq", Path: cityPath}, doltDataDir)
	c.scopeLabel = "city"
	return c
}

// Name returns the check identifier ("rig:<name>:dolt-backup", or
// "<scopeLabel>:dolt-backup" for non-rig scopes — see scopeLabel).
func (c *DoltBackupCheck) Name() string {
	if c.scopeLabel != "" {
		return c.scopeLabel + ":dolt-backup"
	}
	return "rig:" + c.rig.Name + ":dolt-backup"
}

// Run executes the check.
func (c *DoltBackupCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	rigPath := c.normalizedRigPath()

	// An external (non-managed) Dolt endpoint owns its own backups; gc does not
	// manage them, so the local .dolt-backup directory and managed-Dolt
	// repo_state.json signals never apply, and the localhost fix hint below is
	// actively wrong for it. Treat a resolved external endpoint as satisfied
	// rather than warning. Note that External classifies the endpoint's
	// ownership, not its location — an explicit endpoint can resolve to a local
	// host — so the message must not imply a remote machine. See
	// gastownhall/gascity#3868. A resolution error falls through to the
	// local-signal checks so a genuinely missing local backup still surfaces.
	if target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, c.cityPath, rigPath); err == nil && target.External {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("rig %q: external Dolt endpoint %s:%s — backups assumed self-managed at the endpoint", c.rig.Name, target.Host, target.Port)
		return r
	}

	dbName, details := c.resolveDBName(rigPath)
	r.Details = append(r.Details, details...)
	backupDir := filepath.Join(c.cityPath, ".dolt-backup", dbName)

	// Signal 1: backup directory exists on disk with contents.
	backupDirHasContent, err := dirHasEntries(backupDir)
	switch {
	case err != nil:
		r.Details = append(r.Details, fmt.Sprintf("read backup dir: %v", err))
	case backupDirHasContent:
		return c.freshnessResult(r, backupDir, dbName)
	}

	// Signal 2: backup remote is registered in repo_state.json.
	registered, err := backupRemoteRegistered(c.doltDataDir, dbName)
	switch {
	case err != nil:
		// Treat read errors as "not registered" but record the cause in
		// Details for verbose runs. We still want the warning + fix
		// command to reach the operator.
		r.Details = append(r.Details, fmt.Sprintf("read repo_state.json: %v", err))
	case registered:
		r.Status = StatusOK
		r.Message = fmt.Sprintf("backup remote %q registered (sync pending)", dbName+"-backup")
		return r
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("rig %q: no dolt backup registered (expected %s)", c.rig.Name, backupDir)
	r.FixHint = doltBackupFixHint(dbName, backupDir)
	return r
}

// freshnessResult finalizes r for a backup directory already confirmed to
// have contents (dirHasEntries): OK when the newest entry is within
// artifactMaxAge, a staleness warning otherwise. This is the recency check
// the directory-existence signal alone cannot provide — a scope whose sync
// mechanism stopped days ago still has a nonempty, "previously synced"
// directory, which is exactly how hq's 12.5-day-stale backup hid behind this
// check before ga-0avnxn (existence, not recency, was all this check knew).
//
// A directory whose newest-entry mtime can't be determined (e.g. a stat
// error on an already-listed entry) falls back to the pre-existing
// "assumed previously synced" OK behavior rather than failing the check on
// an incidental read error — staying fail-open here matches how the
// repo_state.json read-error path below already behaves.
func (c *DoltBackupCheck) freshnessResult(r *CheckResult, backupDir, dbName string) *CheckResult {
	newest, err := newestEntryModTime(backupDir)
	if err != nil {
		r.Details = append(r.Details, fmt.Sprintf("stat newest backup entry: %v", err))
		r.Status = StatusOK
		r.Message = fmt.Sprintf("backup artifact dir present (assumed previously synced): %s", backupDir)
		return r
	}

	now := c.now
	if now == nil {
		now = time.Now
	}
	maxAge := c.artifactMaxAge
	if maxAge <= 0 {
		maxAge = defaultDoltBackupArtifactMaxAge
	}
	age := now().Sub(newest)

	if age <= maxAge {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("backup artifact dir present and fresh (newest entry %s old): %s", age.Round(time.Minute), backupDir)
		return r
	}

	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	r.Message = fmt.Sprintf("%q: backup artifact dir present but STALE — newest entry %s old (> %s): %s",
		c.rig.Name, age.Round(time.Minute), maxAge, backupDir)
	r.FixHint = fmt.Sprintf(
		"the sync mechanism (e.g. mol-dog-backup) may have stopped for this scope specifically — check "+
			"`gc order check` / `gc order history mol-dog-backup`, then confirm a manual sync lands a fresh "+
			"file:\n%s",
		doltBackupFixHint(dbName, backupDir),
	)
	return r
}

// newestEntryModTime returns the most recent modification time among dir's
// direct (non-recursive) entries. A shallow scan is sufficient here because
// the sync mechanism this check observes (mol-dog-backup) creates or touches
// a new top-level entry in .dolt-backup/<db>/ on every run — matching
// dirHasEntries' own shallow-scan precedent one signal up. Callers should
// have already confirmed the directory is non-empty (dirHasEntries); an
// empty or unreadable directory returns an error.
func newestEntryModTime(dir string) (time.Time, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, err
	}
	var newest time.Time
	found := false
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			// Racing against a concurrent sync (e.g. a file renamed away
			// mid-listing) — skip rather than fail the whole scan.
			continue
		}
		if !found || info.ModTime().After(newest) {
			newest = info.ModTime()
			found = true
		}
	}
	if !found {
		return time.Time{}, fmt.Errorf("no readable entries in %s", dir)
	}
	return newest, nil
}

// CanFix returns false. Registering a backup destination is operator
// policy (local fs vs cloud bucket vs offsite); auto-creating a local
// backup would silently bypass that decision.
func (c *DoltBackupCheck) CanFix() bool { return false }

// Fix is a no-op. See CanFix.
func (c *DoltBackupCheck) Fix(_ *CheckContext) error { return nil }

func (c *DoltBackupCheck) normalizedRigPath() string {
	return normalizedRigPath(c.cityPath, c.rig)
}

// resolveDBName returns the rig's Dolt database name from
// .beads/metadata.json, falling back to rig.Name when the metadata is
// missing or unreadable. Falling back preserves a useful warning even
// for rigs whose metadata never landed — the operator can correct the
// db name in the suggested command if needed.
func (c *DoltBackupCheck) resolveDBName(rigPath string) (string, []string) {
	return resolveDoltDBName(c.rig, rigPath)
}

func normalizedRigPath(cityPath string, rig config.Rig) string {
	rigPath := rig.Path
	if !filepath.IsAbs(rigPath) {
		rigPath = filepath.Join(cityPath, rigPath)
	}
	return rigPath
}

func resolveDoltDBName(rig config.Rig, rigPath string) (string, []string) {
	metadataPath := filepath.Join(rigPath, ".beads", "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rig.Name, nil
		}
		return rig.Name, []string{fmt.Sprintf("read metadata.json: %v; using rig name %q", err, rig.Name)}
	}
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return rig.Name, []string{fmt.Sprintf("parse metadata.json: %v; using rig name %q", err, rig.Name)}
	}
	if s := strings.TrimSpace(meta.DoltDatabase); s != "" {
		return s, nil
	}
	return rig.Name, nil
}

// backupRemoteRegistered reports whether
// <managed-dolt-data-dir>/<db>/.dolt/repo_state.json declares a backup remote
// named "<db>-backup". A missing file returns (false, nil) — that is the
// expected state for a freshly-provisioned rig and not itself an error.
func backupRemoteRegistered(doltDataDir, dbName string) (bool, error) {
	statePath := filepath.Join(doltDataDir, dbName, ".dolt", "repo_state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	var state struct {
		Backups map[string]json.RawMessage `json:"backups"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return false, fmt.Errorf("parse %s: %w", statePath, err)
	}
	_, ok := state.Backups[dbName+"-backup"]
	return ok, nil
}

func dirHasEntries(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// doltBackupFixHint returns the multi-line DOLT_BACKUP add+sync
// invocation as a copy-pasteable shell command. The command targets the
// running managed Dolt server (port comes from $GC_DOLT_PORT, which
// `gc dolt status` surfaces); it does not assume the operator has
// stopped the server.
func doltBackupFixHint(dbName, backupDir string) string {
	return fmt.Sprintf(
		"register the backup remote (requires GC_DOLT_PORT from `gc dolt status`):\n"+
			"  DOLT_CLI_PASSWORD='' dolt --host 127.0.0.1 --port ${GC_DOLT_PORT:?set this via gc dolt status} --user root --no-tls sql -q \\\n"+
			"    \"USE \\`%s\\`; \\\n"+
			"     CALL DOLT_BACKUP('add', '%s-backup', 'file://%s'); \\\n"+
			"     CALL DOLT_BACKUP('sync', '%s-backup');\"",
		dbName, dbName, backupDir, dbName,
	)
}
