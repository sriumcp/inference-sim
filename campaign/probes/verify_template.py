"""Assert at.yaml.template matches the detector's ACTUAL config surface.

THE DEFECT THIS PREVENTS: a stale template carrying a previous design's knobs was copied into
place at launch. It parsed, it looked plausible, and it aborted all 12 rows of three
consecutive iterations with "pointer '/anytime/kappa' names nothing" -- because a config_patch
never creates structure. Three iterations of the circuit breaker to discover one wrong file.

A template cannot be validated by reading it: the old one and the new one are both
syntactically fine. It has to be checked against the Go struct that actually parses it, and
against the pointers the campaign actually patches.
"""
import pathlib
import re
import sys

REPO = pathlib.Path(__file__).resolve().parents[2]
TEMPLATE = REPO / "campaign" / "apparatus" / "at.yaml.template"
CONFIG_GO = REPO / "sim" / "saturation" / "config.go"
CAMPAIGN = REPO / "campaign" / "anytime-robust.yaml"

def go_keys():
    """The yaml keys AnytimeBlock actually declares."""
    src = CONFIG_GO.read_text()
    start = src.index("type AnytimeBlock struct {")
    end = src.index("\n}", start)
    return set(re.findall(r'yaml:"(\w+)"', src[start:end]))

def template_keys():
    keys = set()
    for line in TEMPLATE.read_text().splitlines():
        m = re.match(r"^  (\w+):", line)
        if m:
            keys.add(m.group(1))
    return keys

def patched_pointers():
    """The /anytime/<key> pointers the campaign patches."""
    return set(re.findall(r"pointer: /anytime/(\w+)", CAMPAIGN.read_text()))

def main():
    go, tmpl, patched = go_keys(), template_keys(), patched_pointers()
    ok = True

    unknown = tmpl - go
    if unknown:
        print(f"FAIL template declares keys the detector does not accept: {sorted(unknown)}")
        print("     (a stale template from a previous design -- strict YAML will reject it)")
        ok = False

    missing = patched - tmpl
    if missing:
        print(f"FAIL campaign patches pointers absent from the template: {sorted(missing)}")
        print("     (a config_patch never creates structure -- every row of those levels aborts)")
        ok = False

    if ok:
        print(f"PASS template keys {sorted(tmpl)}")
        print(f"     detector accepts {sorted(go)}")
        print(f"     campaign patches {sorted(patched)} -- all present")
    return 0 if ok else 1

if __name__ == "__main__":
    sys.exit(main())
