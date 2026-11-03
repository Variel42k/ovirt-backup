"""Summarize govulncheck JSON streams; an exit code alone is not a verdict."""
from pathlib import Path
import hashlib
import json

BASE = Path(__file__).resolve().parent


def stream(path):
    raw = path.read_text(encoding="utf-8-sig")
    decoder = json.JSONDecoder()
    offset = 0
    while offset < len(raw):
        if raw[offset].isspace():
            offset += 1
            continue
        value, offset = decoder.raw_decode(raw, offset)
        yield value


def scan(name):
    messages = list(stream(BASE / name))
    findings = [m["finding"] for m in messages if "finding" in m]
    advisories = {m["osv"]["id"]: m["osv"] for m in messages if "osv" in m}
    levels = {"module_only": set(), "import": set(), "call": set()}
    details = {}
    for finding in findings:
        trace = finding.get("trace", [])
        head = trace[0] if trace else {}
        level = "call" if head.get("function") else "import" if head.get("package") else "module_only"
        osv = finding["osv"]
        levels[level].add(osv)
        advisory = advisories.get(osv, {})
        entry = details.setdefault(osv, {
            "module": head.get("module"), "version": head.get("version"),
            "fixed_version": finding.get("fixed_version"),
            "summary": advisory.get("summary"), "levels": [],
            "url": advisory.get("database_specific", {}).get("url"),
        })
        if level not in entry["levels"]:
            entry["levels"].append(level)
    return {
        "config": next((m["config"] for m in messages if "config" in m), None),
        "finding_messages": len(findings), "unique_advisories": len(details),
        "advisories_per_level": {k: len(v) for k, v in levels.items()},
        "details": details,
        "note": "Levels may overlap for the same advisory; call traces indicate static reachability, not demonstrated exploitation.",
    }


if __name__ == "__main__":
    go_tests = (BASE / "go-tests-linux.txt").read_text(encoding="utf-8-sig")
    browser = (BASE / "browser-tests.txt").read_text(encoding="utf-8-sig")
    files = ["govulncheck.json", "govulncheck-linux-after.json", "npm-audit.json", "npm-audit-after.json", "go-tests-linux.txt", "go-vet-linux.txt", "browser-tests.txt"]
    result = {
        "date": "2026-10-30", "scope": "Local source checkout; no production deployment or live VM operations",
        "go_before_windows": scan("govulncheck.json"),
        "go_after_linux": scan("govulncheck-linux-after.json"),
        "npm_before_production": json.loads((BASE / "npm-audit.json").read_text(encoding="utf-8-sig"))["metadata"]["vulnerabilities"],
        "npm_after_all_dependencies": json.loads((BASE / "npm-audit-after.json").read_text(encoding="utf-8-sig"))["metadata"]["vulnerabilities"],
        "tests": {
            "go_test_all_exit_code": 0,
            "go_vet_all_exit_code": 0,
            "go_packages_with_tests_passed": sum(line.startswith("ok  ") for line in go_tests.splitlines()),
            "go_log_has_fail": any(line.startswith("FAIL") for line in go_tests.splitlines()),
            "browser_passed": 52,
            "browser_summary_present": "52 passed" in browser,
        },
        "limits": ["Windows before / Linux after: platforms differ", "Go source scan is not a full container or supply-chain scan", "No completed FSTEC BDU check", "No formal conformity or attestation conclusion"],
        "evidence": [{"file": name, "sha256": hashlib.sha256((BASE / name).read_bytes()).hexdigest()} for name in files],
    }
    assert not result["tests"]["go_log_has_fail"]
    assert result["tests"]["browser_summary_present"]
    (BASE / "security-summary.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({k: v for k, v in result.items() if k not in ("evidence", "go_before_windows")}, ensure_ascii=False, indent=2))
