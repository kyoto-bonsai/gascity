#!/usr/bin/env bash
# Exercise the gate's phase ordering and failure propagation with a fake Go
# command. This proves the runner launches every planned shard after a core
# failure without running repository tests or relying on host services.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$repo_root/scripts/tmp"
fixture_dir="$(mktemp -d "$repo_root/scripts/tmp/sharded-gate-phases.XXXXXX")"
trap 'rm -rf "$fixture_dir"' EXIT
mkdir -p "$fixture_dir/bin" "$fixture_dir/home"

cat > "$fixture_dir/bin/go" <<'FAKE_GO'
#!/usr/bin/env bash
set -euo pipefail
module=github.com/gastownhall/gascity
case "${1:-}" in
  env)
    case "${2:-}" in
      GOPATH) echo "$HOME/go" ;;
      GOCACHE) echo "$HOME/cache" ;;
      GOMODCACHE) echo "$HOME/modcache" ;;
      GOTMPDIR) echo "$HOME/tmp" ;;
      GOROOT) echo "$HOME/toroot" ;;
      *) echo '' ;;
    esac
    ;;
  list)
    if [[ "${2:-}" == -m ]]; then
      echo "$module"
    else
      printf '%s\n' "$module/cmd/gc" "$module/internal/fake"
    fi
    ;;
  test)
    args=" $* "
    if [[ "$args" == *' -list '* ]]; then
      for number in $(seq -w 1 12); do echo "Test${number}"; done
      exit 0
    fi
    if [[ "$args" == *' -c '* ]]; then exit 0; fi
    if [[ "$args" != *' -json '* ]]; then exit 9; fi
    if [[ "$args" == *' ./cmd/gc '* ]]; then
      printf 'cmdgc %s\n' "${GOFLAGS:-}" >> "$HOME/phase.trace"
      regex=''
      previous=''
      for arg in "$@"; do
        if [[ "$previous" == -run ]]; then regex="$arg"; break; fi
        previous="$arg"
      done
      for number in $(seq -w 1 12); do
        name="Test${number}"
        if [[ "$regex" == *"$name"* ]]; then
          printf '{"Action":"pass","Package":"%s/cmd/gc","Test":"%s","Elapsed":0.01}\n' "$module" "$name"
        fi
      done
      printf '{"Action":"pass","Package":"%s/cmd/gc","Elapsed":0.02}\n' "$module"
    else
      printf 'core %s\n' "${GOFLAGS:-}" >> "$HOME/phase.trace"
      if [[ -e "$HOME/fail-core" ]]; then
        printf '{"Action":"fail","Package":"%s/internal/fake","Elapsed":0.01}\n' "$module"
        exit 1
      fi
      printf '{"Action":"pass","Package":"%s/internal/fake","Elapsed":0.01}\n' "$module"
    fi
    ;;
  *) exit 10 ;;
esac
FAKE_GO
chmod +x "$fixture_dir/bin/go"
ln -s "$fixture_dir/bin" "$fixture_dir/home/bin"
printf 'export PATH="$HOME/bin:$PATH"\n' > "$fixture_dir/home/.bash_profile"

run_gate() {
  local evidence_dir="$1"
  mkdir -p "$evidence_dir"
  cd "$repo_root"
  env PATH="$fixture_dir/bin:$PATH" HOME="$fixture_dir/home" TMPDIR="$fixture_dir" \
    GC_TEST_NO_SLICE=1 GC_TEST_NO_ORPHAN_SWEEP=1 GC_PUSH_GATE_NO_CAP=1 \
    LOCAL_TEST_LOG_DIR="$evidence_dir" LOCAL_TEST_JOBS=4 \
    CMD_GC_PROCESS_TOTAL=12 GO_TEST_TIMEOUT=15m \
    ./scripts/test-local-parallel gate > "${evidence_dir}.runner.log" 2>&1
}

if ! run_gate "$fixture_dir/success"; then
  cat "$fixture_dir/success.runner.log" >&2
  exit 1
fi
python3 - "$fixture_dir/home/phase.trace" "$fixture_dir/success/gate-inventory.json" <<'PY'
import json
import sys
from pathlib import Path
trace = Path(sys.argv[1]).read_text().splitlines()
assert len(trace) == 13 and trace[0].startswith('core '), trace
assert '-p=4' in trace[0] and all(line.startswith('cmdgc ') and '-p=2' in line for line in trace[1:]), trace
inventory = json.loads(Path(sys.argv[2]).read_text())
assert inventory['all_packages'] == 2, inventory
assert inventory['cmdgc_planned_top_level_tests'] == 12, inventory
assert inventory['cmdgc_terminal_top_level'] == {'pass': 12}, inventory
PY

: > "$fixture_dir/home/phase.trace"
touch "$fixture_dir/home/fail-core"
if run_gate "$fixture_dir/core-failure"; then
  echo 'gate passed despite the core worker failure' >&2
  exit 1
fi
python3 - "$fixture_dir/home/phase.trace" "$fixture_dir/core-failure" <<'PY'
import json
import sys
from pathlib import Path
trace = Path(sys.argv[1]).read_text().splitlines()
root = Path(sys.argv[2])
assert len(trace) == 13 and trace[0].startswith('core '), trace
assert '-p=4' in trace[0] and all(line.startswith('cmdgc ') and '-p=2' in line for line in trace[1:]), trace
assert (root / 'unit-core.exit').read_text().strip() != '0'
for index in range(1, 13):
    assert (root / f'unit-cmd-gc-{index}-of-12.exit').read_text().strip() == '0', index
    events = [json.loads(line) for line in (root / f'cmdgc-{index}.jsonl').read_text().splitlines()]
    assert sum(event.get('Action') == 'pass' and bool(event.get('Test')) for event in events) == 1, index
assert 'gate: cmd/gc phase jobs=12' in (root.parent / 'core-failure.runner.log').read_text()
assert not (root / 'gate-inventory.json').exists()
PY
echo 'sharded gate phase smoke PASS: core failure retained all 12 shard runs and failed the gate'
