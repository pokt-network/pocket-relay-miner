#!/usr/bin/env python3
"""Generates the Grafana dashboards in examples/observability/grafana/dashboards.

The metric list is read from the Go source (every NewCounter/Gauge/Histogram,
their Func forms, and the Redis pool collector), so the dashboards cannot name a
metric that does not exist, and --check fails when a metric is on no panel.

    python3 scripts/dashboards/generate.py          # write the dashboards
    python3 scripts/dashboards/generate.py --check  # fail on drift or a metric on no panel
"""
import json
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
OUT = os.path.join(ROOT, "examples/observability/grafana/dashboards")


# ---------------------------------------------------------------- inventory
def inventory():
    files = subprocess.run(["git", "ls-files", "*.go"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.split()
    files = [f for f in files if not f.endswith("_test.go") and not f.startswith(("tilt/", "examples/", "scripts/"))]
    consts = {}
    for f in files:
        d = consts.setdefault(os.path.dirname(f), {})
        for m in re.finditer(r'\b(\w+)\s*=\s*"([^"]*)"', open(os.path.join(ROOT, f)).read()):
            d.setdefault(m.group(1), m.group(2))
    ctor = re.compile(r"\bNew(Counter|Gauge|Histogram|Summary)(Vec|Func)?\(\s*prometheus\.(\w+)Opts\{(.*?)\n\t*\}\s*(?:,\s*(\[\]string\{[^}]*\}|\w+))?", re.S)
    out = {}
    for f in files:
        src = open(os.path.join(ROOT, f)).read()
        for m in ctor.finditer(src):
            kind, _, _, body, labels = m.groups()

            def val(k):
                v = re.search(r"\b" + k + r':\s*("(?:[^"\\]|\\.)*"|[\w.]+)', body)
                if not v:
                    return ""
                v = v.group(1)
                if v[0] == '"':
                    return v[1:-1]
                parts = v.split(".")
                if len(parts) == 2:
                    for d, c in consts.items():
                        if os.path.basename(d) == parts[0] and parts[1] in c:
                            return c[parts[1]]
                    raise SystemExit("unresolved %s in %s" % (v, f))
                if v not in consts[os.path.dirname(f)]:
                    raise SystemExit("unresolved %s in %s" % (v, f))
                return consts[os.path.dirname(f)][v]

            name = "_".join(x for x in (val("Namespace"), val("Subsystem"), val("Name")) if x)
            lab = re.findall(r'"([^"]+)"', labels) if labels and labels.startswith("[]") else []
            cl = re.search(r'ConstLabels:\s*prometheus\.Labels\{\s*"(\w+)"', body)
            if cl:
                lab.append(cl.group(1))
            elif "ConstLabels: labels" in body:
                lab.append("component")
            out[name] = {"type": kind.lower(), "labels": lab}
    src = open(os.path.join(ROOT, "transport/redis/pool_metrics.go")).read()
    for m in re.finditer(r'desc\("(\w+)",', src):
        n = "ha_transport_redis_pool_" + m.group(1)
        out[n] = {"type": "counter" if n.endswith("_total") else "gauge", "labels": ["component"]}
    return out


INV = inventory()
USED = set()


# ---------------------------------------------------------------- query helpers
def q(name, extra=""):
    """A selector for NAME with the dashboard's variables on the labels it carries."""
    base = name
    for suf in ("_bucket", "_count", "_sum"):
        if name.endswith(suf) and name[: -len(suf)] in INV:
            base = name[: -len(suf)]
    if base not in INV:
        raise SystemExit("unknown metric in a panel: " + name)
    USED.add(base)
    labels = INV[base]["labels"] + (["le"] if name.endswith("_bucket") else [])
    sel = []
    if "supplier" in labels:
        sel.append('supplier=~"$supplier"')
    if "supplier_addr" in labels:
        sel.append('supplier_addr=~"$supplier"')
    if "service_id" in labels:
        sel.append('service_id=~"$service"')
    if extra:
        sel.append(extra)
    return name + ("{" + ",".join(sel) + "}" if sel else "")


def T(name, extra="", by=""):
    """Run total over the dashboard range: the work each series did inside it.

    Per series: its last value minus its last value before the range, when it
    was alive before the range and did not reset; otherwise its last value (a
    series born inside the range, or restarted in it). A plain
    last_over_time would add a pod's whole lifetime, work done before the
    range included, so a range spanning a restart doubled; increase() would
    miss series born inside the range. Ungrouped, a counter that never fired
    has no series; "or vector(0)" makes it 0, so an identity that subtracts it
    still has a value."""
    sel = q(name, extra)
    last = "last_over_time(%s[$__range])" % sel
    before = "last_over_time(%s[$__range] offset $__range)" % sel
    inner = "((%s - %s) >= 0 and resets(%s[$__range]) == 0) or %s" % (last, before, sel, last)
    if by:
        return "sum by (%s)(%s)" % (by, inner)
    return "(sum(%s) or vector(0))" % inner


def R(name, by="", extra=""):
    agg = "sum by (%s)" % by if by else "sum"
    return "%s(rate(%s[$__rate_interval]))" % (agg, q(name, extra))


def INC(name, by="", extra=""):
    agg = "sum by (%s)" % by if by else "sum"
    return "%s(increase(%s[$__range]))" % (agg, q(name, extra))


def G(name, agg="sum", by="", extra=""):
    a = "%s by (%s)" % (agg, by) if by else agg
    return "%s(%s)" % (a, q(name, extra))


def P(name, qn, by="", extra=""):
    b = "le" + ("," + by if by else "")
    return "histogram_quantile(%s, sum by (%s)(rate(%s[$__rate_interval])))" % (qn, b, q(name + "_bucket", extra))


def ext(expr):
    """An expression over series this repository does not define (cadvisor, redis_exporter, Go runtime)."""
    return expr


# ---------------------------------------------------------------- panel builders
GREEN, YELLOW, RED, BLUE = "green", "#EAB839", "red", "blue"
ZERO_GOOD = [(None, GREEN), (1e-9, RED)]
ZERO_GOOD_ABS = [(None, RED), (-1e-9, GREEN), (1e-9, RED)]


def _fieldcfg(unit, thresholds, decimals=None, mappings=None):
    steps = [{"color": c, "value": v} for v, c in (thresholds or [(None, GREEN)])]
    d = {"unit": unit, "thresholds": {"mode": "absolute", "steps": steps}, "color": {"mode": "thresholds" if thresholds else "palette-classic"}}
    if decimals is not None:
        d["decimals"] = decimals
    if mappings:
        d["mappings"] = mappings
    return {"defaults": d, "overrides": []}


def _targets(exprs, instant=False, fmt=None):
    ts = []
    for i, e in enumerate(exprs):
        expr, legend = (e, "") if isinstance(e, str) else e
        t = {"refId": chr(65 + i), "expr": expr, "legendFormat": legend, "datasource": {"type": "prometheus", "uid": "${datasource}"}}
        if instant:
            t["instant"], t["range"] = True, False
        if fmt:
            t["format"] = fmt
        ts.append(t)
    return ts


def stat(title, exprs, unit="short", thr=None, desc="", w=4, h=4, decimals=None, nodata="0"):
    p = {"type": "stat", "title": title, "description": desc, "targets": _targets(exprs, instant=True),
         "fieldConfig": _fieldcfg(unit, thr, decimals), "_w": w, "_h": h,
         "options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "background", "graphMode": "none", "textMode": "value_and_name" if len(exprs) > 1 else "value"}}
    p["fieldConfig"]["defaults"]["noValue"] = nodata
    return p


def ts(title, exprs, unit="short", desc="", w=12, h=8, stack=False, thr=None):
    fc = _fieldcfg(unit, thr)
    fc["defaults"]["custom"] = {"drawStyle": "line", "lineWidth": 1, "fillOpacity": 25 if stack else 8, "stacking": {"mode": "normal" if stack else "none"}, "showPoints": "never", "spanNulls": True}
    if thr:
        fc["defaults"]["custom"]["thresholdsStyle"] = {"mode": "dashed"}
        fc["defaults"]["color"] = {"mode": "palette-classic"}
    return {"type": "timeseries", "title": title, "description": desc, "targets": _targets(exprs), "fieldConfig": fc, "_w": w, "_h": h,
            "options": {"legend": {"displayMode": "table", "placement": "bottom", "calcs": ["lastNotNull", "max"]}, "tooltip": {"mode": "multi", "sort": "desc"}}}


def table(title, exprs, unit="short", desc="", w=12, h=8, thr=None):
    # An instant query in table format names its value column "Value" (one
    # query) or "Value #A".. (several); rename them to the queries' legends.
    legends = [("" if isinstance(e, str) else e[1]) for e in exprs]
    if len(exprs) == 1:
        rename = {"Value": legends[0] or "value"}
    else:
        rename = {"Value #%s" % chr(65 + i): (lg or chr(65 + i)) for i, lg in enumerate(legends)}
    return {"type": "table", "title": title, "description": desc, "targets": _targets(exprs, instant=True, fmt="table"),
            "fieldConfig": _fieldcfg(unit, thr), "_w": w, "_h": h,
            "transformations": [{"id": "merge"}, {"id": "organize", "options": {"excludeByName": {"Time": True}, "renameByName": rename}}],
            "options": {"showHeader": True, "cellHeight": "sm"}}


def bar(title, exprs, unit="short", desc="", w=12, h=8, thr=None):
    return {"type": "bargauge", "title": title, "description": desc, "targets": _targets(exprs, instant=True),
            "fieldConfig": _fieldcfg(unit, thr), "_w": w, "_h": h,
            "options": {"orientation": "horizontal", "displayMode": "gradient", "reduceOptions": {"calcs": ["lastNotNull"]}, "showUnfilled": True}}


def state(title, exprs, desc="", w=12, h=6, mappings=None, thr=None):
    fc = _fieldcfg("short", thr or [(None, RED), (1, GREEN)], mappings=mappings)
    return {"type": "state-timeline", "title": title, "description": desc, "targets": _targets(exprs), "fieldConfig": fc, "_w": w, "_h": h,
            "options": {"showValue": "never", "mergeValues": True, "rowHeight": 0.8}}


def text(title, md, w=24, h=3):
    return {"type": "text", "title": title, "_w": w, "_h": h, "options": {"mode": "markdown", "content": md}}


def row(title, *panels, collapsed=False):
    return {"row": title, "panels": list(panels), "collapsed": collapsed}


# ---------------------------------------------------------------- dashboard assembly
def build(uid, title, desc, rows, tags):
    panels, y, pid = [], 0, 1
    for r in rows:
        rp = {"type": "row", "title": r["row"], "id": pid, "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}, "collapsed": r["collapsed"], "panels": []}
        pid += 1
        y += 1
        x, rowh = 0, 0
        children = []
        for p in r["panels"]:
            w, h = p.pop("_w"), p.pop("_h")
            if x + w > 24:
                x, y, rowh = 0, y + rowh, 0
            p["id"] = pid
            pid += 1
            p["gridPos"] = {"h": h, "w": w, "x": x, "y": y}
            p["datasource"] = {"type": "prometheus", "uid": "${datasource}"}
            x += w
            rowh = max(rowh, h)
            children.append(p)
        y += rowh
        if r["collapsed"]:
            rp["panels"] = children
            panels.append(rp)
        else:
            panels.append(rp)
            panels.extend(children)
    var = lambda name, label, query, multi=True: {
        "name": name, "label": label, "type": "query", "datasource": {"type": "prometheus", "uid": "${datasource}"},
        "query": {"query": query, "refId": name}, "definition": query, "refresh": 2, "multi": multi, "includeAll": True,
        "allValue": ".*", "current": {"selected": True, "text": ["All"], "value": ["$__all"]}, "sort": 1}
    return {
        "uid": uid, "title": title, "description": desc, "tags": ["pocket-relay-miner"] + tags, "schemaVersion": 39,
        "editable": True, "graphTooltip": 1, "time": {"from": "now-6h", "to": "now"}, "refresh": "30s",
        "timezone": "", "fiscalYearStartMonth": 0, "liveNow": False, "weekStart": "",
        "links": [{"title": "Dashboards", "type": "dashboards", "tags": ["pocket-relay-miner"], "asDropdown": True}],
        "annotations": {"list": []},
        "templating": {"list": [
            {"name": "datasource", "label": "Prometheus", "type": "datasource", "query": "prometheus", "current": {}, "hide": 0},
            var("supplier", "Supplier", "label_values(ha_miner_sessions_created_total, supplier)"),
            var("service", "Service", "label_values(ha_relayer_relays_served_total, service_id)"),
            var("instance", "Process", "label_values(ha_runtime_goroutines, instance)"),
        ]},
        "panels": panels,
    }


sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dashboards_spec  # noqa: E402  (the panel definitions, next to this file)

SPECS = dashboards_spec.specs(globals())


def render():
    out = {}
    for d in SPECS:
        out[d["file"]] = json.dumps(build(d["uid"], d["title"], d["desc"], d["rows"], d["tags"]), indent=2, sort_keys=True) + "\n"
    return out


def main():
    check = "--check" in sys.argv
    files = render()
    missing = sorted(set(INV) - USED - set(dashboards_spec.NOT_CHARTED))
    stale = sorted(set(dashboards_spec.NOT_CHARTED) - set(INV))
    problems = []
    if missing:
        problems.append("metrics on no panel (chart them or list them in NOT_CHARTED with a reason):\n  " + "\n  ".join(missing))
    if stale:
        problems.append("NOT_CHARTED names that no longer exist:\n  " + "\n  ".join(stale))
    if check:
        for f, body in files.items():
            path = os.path.join(OUT, f)
            if not os.path.exists(path) or open(path).read() != body:
                problems.append("%s is not what the generator writes: run python3 scripts/dashboards/generate.py" % path)
        extra = set(os.listdir(OUT)) - set(files) if os.path.isdir(OUT) else set()
        if extra:
            problems.append("files in %s the generator does not write: %s" % (OUT, sorted(extra)))
    else:
        os.makedirs(OUT, exist_ok=True)
        for f, body in files.items():
            open(os.path.join(OUT, f), "w").write(body)
    print("%d metrics, %d charted, %d not charted by decision, %d dashboards" % (len(INV), len(USED), len(dashboards_spec.NOT_CHARTED), len(files)))
    if problems:
        print("\n".join(problems))
        sys.exit(1)


if __name__ == "__main__":
    main()
