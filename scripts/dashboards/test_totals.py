#!/usr/bin/env python3
"""Evaluates the dashboards' run totals, T() in generate.py, on a real Prometheus.

promtool runs the very expressions T() writes into the dashboards, over series
shaped as the relay miner exports them, and checks each total counts only the
work done inside the dashboard range: a pod that died inside it, one born
inside it, one alive across it, a scrape gap at its start, a restart inside a
pod, a restart at its start, and a counter that never fired.

    python3 scripts/dashboards/test_totals.py

promtool comes from PROMTOOL when set, else from the image the examples ship.
"""

import os
import subprocess
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import generate  # noqa: E402

IMAGE = "prom/prometheus:v3.5.0"
METRIC = "ha_miner_relays_claimed_total"
NEVER_FIRED = "ha_miner_sessions_reinstated_total"

# One sample a minute, evaluated at minute 119 with a 60m range: the range holds
# minutes 60 to 119, "before the range" is minute 59 and earlier.
SERIES = [
    # (pod, supplier, values, work inside the range)
    # Served 2950 before the range, 84 inside it, died at minute 64.
    ("standalone-old", "pokt1s1", "0+50x59 3000 3010 3020 3034 stale", 84),
    # Born at minute 80, served 3024.
    ("standalone-new", "pokt1s1", "_x80 24+100x30", 3024),
    # Alive across the range: 1000 before it, 1295 at its end.
    ("relayer-a", "pokt1s2", "1000x59 1000+5x60", 295),
    # No sample from minute 50 to 64, longer than the 5m lookback, across the
    # start: 500 before the range, 1100 at minute 115.
    ("relayer-b", "pokt1s2", "500x49 _x15 600+10x50", 600),
    # Restarted inside the pod at minute 62: counts from the restart, 30 x 57.
    ("miner-a", "pokt1s3", "0+20x59 1200 1220 0+30x57", 1710),
    # Restarted at the start of the range: 900 before it, 590 after.
    ("miner-b", "pokt1s3", "900x59 0+10x60", 590),
]


def concrete(expr):
    return expr.replace("$__range", "60m").replace("$supplier", ".*").replace("$service", ".*")


def labels(pod, supplier):
    return '{exported_instance="%s",instance="relay-miner",supplier="%s",service_id="develop-http"}' % (pod, supplier)


def case(expr, samples):
    lines = ["      - expr: '%s'" % concrete(expr), "        eval_time: 119m", "        exp_samples:"]
    for lab, value in samples:
        lines.append("          - labels: '%s'" % lab)
        lines.append("            value: %d" % value)
    return lines


def test_file():
    by_supplier = {}
    for _, supplier, _, work in SERIES:
        by_supplier[supplier] = by_supplier.get(supplier, 0) + work
    out = ["tests:", "  - interval: 1m", "    input_series:"]
    for pod, supplier, values, _ in SERIES:
        out.append("      - series: '%s%s'" % (METRIC, labels(pod, supplier)))
        out.append("        values: '%s'" % values)
    out.append("    promql_expr_test:")
    out += case(generate.T(METRIC), [("{}", sum(w for _, _, _, w in SERIES))])
    out += case(generate.T(METRIC, by="supplier"),
                [('{supplier="%s"}' % s, v) for s, v in sorted(by_supplier.items())])
    out += case(generate.T(NEVER_FIRED), [("{}", 0)])
    return "\n".join(out) + "\n"


def main():
    with tempfile.TemporaryDirectory() as d:
        with open(os.path.join(d, "totals.yaml"), "w") as f:
            f.write(test_file())
        # The image runs promtool as nobody.
        os.chmod(d, 0o755)
        os.chmod(os.path.join(d, "totals.yaml"), 0o644)
        if os.environ.get("PROMTOOL"):
            cmd = [os.environ["PROMTOOL"], "test", "rules", "totals.yaml"]
        else:
            cmd = ["docker", "run", "--rm", "-v", d + ":/t", "-w", "/t", "--entrypoint", "promtool",
                   IMAGE, "test", "rules", "totals.yaml"]
        run = subprocess.run(cmd, cwd=d, capture_output=True, text=True)
    if run.returncode != 0:
        sys.stdout.write(run.stdout + run.stderr)
        return 1
    print("run totals: %d series, 3 expressions, exact on promtool" % len(SERIES))
    return 0


if __name__ == "__main__":
    sys.exit(main())
