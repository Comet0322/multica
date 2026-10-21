#!/usr/bin/env python3
"""run_trace: explain where the wall-clock time of agent runs goes.

Usage:
  run_trace.py TEST-77 [TEST-78 ...]        every run of the issue(s) + handoff gaps
  Options: --ends (what agents do at start/end)   --html FILE (interactive timeline)
           --mc /path/to/multica   --log daemon.log (optional, repeatable: exact daemon phase timing)
           --top N (slowest tool calls shown, default 5)

Sources: `multica issue runs` and `multica issue run-messages` only, i.e. the
workspace's own records, so it works from any machine with a logged-in CLI. An
optional --log adds the daemon's exact phase timing. Every second of
a run is assigned to exactly one bucket:

  queue_wait     run created -> daemon claimed it (server wakeup / daemon poll)
  daemon_prepare claimed -> agent CLI launched (skills, env, workdir)
  agent_boot     CLI launched -> first output (CLI start + first model call)
  model          agent window time with no tool running (thinking / generating)
  tool:<name>    time inside a tool call (union of overlapping calls)
  final_answer   last tool result -> turn complete (the closing message)
  teardown       turn complete -> daemon finished reporting
  handoff_gap    previous run finished -> next run created, same issue, up to 120s (longer = idle, excluded)
"""
import argparse, collections, glob, gzip, json, os, re, subprocess, sys
from datetime import datetime, timedelta

LINE = re.compile(r"^(\d\d):(\d\d):(\d\d)\.(\d{3}) (\w+) (.*)$")
TASK = re.compile(r"\btask=(\S+)")
TOOL_START = re.compile(r"^tool #(\d+): (\S+)")
TOOL_RESULT = re.compile(r"^tool_result observed .*\bseq=(\d+) tool=(\S+) call_id=(\S+)")
IDLE_AFTER = 120  # a longer pause between runs of one issue is waiting, not handoff
PHASE = re.compile(r"task_phase=(\w+)")


def to_local(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00")).astimezone().replace(tzinfo=None)


def read_logs(paths):
    for p in paths:
        op = gzip.open if p.endswith(".gz") else open
        with op(p, "rt", errors="ignore") as f:
            for l in f:
                yield l.rstrip("\n")


def events_for(tasks, log_paths):
    """task_id -> [(datetime, msg)] with the date inferred from the run's created_at."""
    want = {t["id"]: t for t in tasks}
    out = collections.defaultdict(list)
    for l in read_logs(log_paths):
        m = LINE.match(l)
        if not m:
            continue
        tm = TASK.search(m.group(6))
        if not tm or tm.group(1) not in want:
            continue
        t = want[tm.group(1)]
        base = to_local(t["created_at"]).replace(hour=0, minute=0, second=0, microsecond=0)
        h, mi, s, ms = int(m.group(1)), int(m.group(2)), int(m.group(3)), int(m.group(4))
        ts = base + timedelta(hours=h, minutes=mi, seconds=s, milliseconds=ms)
        if ts < to_local(t["created_at"]) - timedelta(seconds=5):
            ts += timedelta(days=1)
        out[t["id"]].append((ts, m.group(6)))
    for v in out.values():
        v.sort(key=lambda e: e[0])
    return out


def union(intervals):
    res = []
    for a, b in sorted(intervals):
        if res and a <= res[-1][1]:
            res[-1][1] = max(res[-1][1], b)
        else:
            res.append([a, b])
    return res


def analyze(task, evs):
    """Return (buckets, slow_calls, marks) for one run."""
    B = collections.Counter()
    created = to_local(task["created_at"])
    marks, starts, results = {}, {}, {}
    order = []
    for ts, msg in evs:
        pm = PHASE.search(msg)
        if "task phase recorded" in msg and pm:
            marks.setdefault(pm.group(1), ts)
        if msg.startswith("task received"):
            marks.setdefault("claimed", ts)
        tm = TOOL_START.match(msg)
        if tm:
            starts[len(order)] = (ts, tm.group(2))
            order.append(["start", ts, tm.group(2), None])
        rm = TOOL_RESULT.match(msg)
        if rm:
            order.append(["result", ts, rm.group(2), rm.group(3)])
        if msg.startswith("agent finished"):
            marks.setdefault("agent_finished", ts)
    claimed = marks.get("claimed")
    if not claimed:
        return None
    B["queue_wait"] = (claimed - created).total_seconds()
    env = marks.get("environment_ready", claimed)
    rt = marks.get("runtime_started", env)
    first = marks.get("first_output_received", rt)
    done_turn = marks.get("turn_completed") or marks.get("agent_finished") or first
    fin = marks.get("finished", done_turn)
    B["daemon_prepare"] = (rt - claimed).total_seconds()
    B["agent_boot"] = (first - rt).total_seconds()

    # pair tool starts with results (FIFO per tool name; results carry the name)
    pending = collections.defaultdict(list)
    calls = []
    for kind, ts, name, _cid in order:
        if kind == "start":
            pending[name].append(ts)
        elif pending[name]:
            calls.append((pending[name].pop(0), ts, name))
    for name, lst in pending.items():  # started, never reported a result
        for ts in lst:
            calls.append((ts, done_turn, name))
    busy = union([(a, b) for a, b, _ in calls])
    # attribute each tool's time to itself; overlapping time split evenly
    edges = sorted({t for a, b, _ in calls for t in (a, b)})
    for x, y in zip(edges, edges[1:]):
        active = [n for a, b, n in calls if a <= x and y <= b]
        for n in active:
            B["tool:" + n] += (y - x).total_seconds() / len(active)
    window = (done_turn - first).total_seconds()
    tool_total = sum(v for k, v in B.items() if k.startswith("tool:"))
    last_result = max([b for _, b, _ in calls], default=first)
    final = max(0.0, (done_turn - last_result).total_seconds())
    B["final_answer"] = final
    B["model"] = max(0.0, window - tool_total - final)
    B["teardown"] = max(0.0, (fin - done_turn).total_seconds())
    slow = sorted(((b - a).total_seconds(), n, a) for a, b, n in calls)[::-1]
    return B, slow, marks


def fetch_messages(mc, task_id, issue):
    r = subprocess.run([mc, "issue", "run-messages", task_id, "--issue", issue, "--output", "json"], capture_output=True, text=True)
    if r.returncode:
        return []
    d = json.loads(r.stdout)
    return d if isinstance(d, list) else d.get("messages", [])


_MSG_CACHE = {}


def messages_for(mc, task):
    key = task["id"]
    if key not in _MSG_CACHE:
        _MSG_CACHE[key] = fetch_messages(mc, key, task["_issue"])
    return _MSG_CACHE[key]


def server_events(task, msgs):
    """Build the event list analyze() expects from server data only (no daemon log).

    Approximations: claim = dispatched_at; runtime_started = started_at; first output =
    first reported message; turn end = last text message. Daemon prepare time is not
    visible server-side and is folded into agent_boot."""
    ev = []
    ts = lambda k: to_local(task[k]) if task.get(k) else None
    disp, started, done = ts("dispatched_at") or ts("created_at"), ts("started_at"), ts("completed_at")
    ev.append((disp, "task received"))
    if started:
        ev.append((started, "task phase recorded task_phase=runtime_started"))
    times = sorted(to_local(m["created_at"]) for m in msgs)
    if times:
        ev.append((times[0], "task phase recorded task_phase=first_output_received"))
    texts = sorted(to_local(m["created_at"]) for m in msgs if m.get("type") == "text")
    end_turn = texts[-1] if texts else (times[-1] if times else done)
    if end_turn:
        ev.append((end_turn, "task phase recorded task_phase=turn_completed"))
    if done:
        ev.append((done, "task phase recorded task_phase=finished"))
    n = 0
    for m in sorted(msgs, key=lambda m: int(m.get("seq", 0))):
        t = to_local(m["created_at"])
        if m.get("type") == "tool_use":
            n += 1
            ev.append((t, f"tool #{n}: {m.get('tool', '?')}"))
        elif m.get("type") == "tool_result":
            ev.append((t, f"tool_result observed seq={m.get('seq')} tool={m.get('tool', '?')} call_id={m.get('call_id')}"))
    return sorted(ev, key=lambda e: e[0])


def tool_detail(name, raw):
    try:
        inp = json.loads(raw) if isinstance(raw, str) else (raw or {})
    except Exception:
        return str(raw)[:160]
    if not isinstance(inp, dict):
        return str(inp)[:160]
    for k in ("command", "skill", "query", "file_path", "path", "repo", "dest"):
        if k in inp:
            extra = f" {inp['branch']}" if name.endswith("git_push") and "branch" in inp else ""
            val = str(inp[k]) if k in ("command", "skill") else f"{k}={inp[k]}"
            return val + extra
    return json.dumps(inp)[:160]


def build_segments(task, marks, msgs):
    """Ordered [start_s, end_s, kind, label, detail] relative to the run's created_at."""
    t0 = to_local(task["created_at"])
    rel = lambda d: round((d - t0).total_seconds(), 3)
    claimed = marks.get("claimed", t0)
    rt = marks.get("runtime_started", claimed)
    first = marks.get("first_output_received", rt)
    done = marks.get("turn_completed") or marks.get("agent_finished") or first
    fin = marks.get("finished", done)
    segs = [[0, rel(claimed), "queue_wait", "queue", "created -> claimed by the daemon"],
            [rel(claimed), rel(rt), "daemon_prepare", "prepare", "skills + environment"],
            [rel(rt), rel(first), "agent_boot", "boot", "CLI start + first model call"]]
    uses, results = {}, {}
    for m in msgs:
        ts = to_local(m["created_at"])
        if m.get("type") == "tool_use":
            uses[m["call_id"]] = (ts, m.get("tool", "?"), tool_detail(m.get("tool", "?"), m.get("input")))
        elif m.get("type") == "tool_result":
            results[m["call_id"]] = ts
    calls = sorted((u[0], results.get(cid, done), u[1], u[2]) for cid, u in uses.items())
    cursor = first
    for a, b, name, detail in calls:
        if (a - cursor).total_seconds() > 0.05:
            segs.append([rel(cursor), rel(a), "model", "model", "thinking / generating"])
        segs.append([rel(a), rel(max(b, a)), "tool", name, detail])
        cursor = max(cursor, b)
    if done > cursor:
        segs.append([rel(cursor), rel(done), "final_answer", "final answer", "closing message"])
    if fin > done:
        segs.append([rel(done), rel(fin), "teardown", "teardown", "daemon reports the result"])
    return [x for x in segs if x[1] > x[0]]


MULTICA_CMD = re.compile(r"\bmultica\s+(issue|comment|agent|project|skill|runtime|workflow|autopilot|label|inbox)\b")
ORIENT = {"multica", "skill", "toolsearch"}


def call_kind(seg):
    name, det = seg[3], seg[4]
    if name == "Skill":
        return "skill"
    if name == "ToolSearch":
        return "toolsearch"
    if name == "Bash":
        return "multica" if MULTICA_CMD.search(det) else "work-bash"
    return "git-gateway" if name.startswith("mcp__git__") else "work-" + name.lower()


def ends_report(runs, evs, agents, mc):
    """What agents do right after starting and right before finishing.

    head = first output -> first real work call (anything that is not a multica
    CLI call, Skill or ToolSearch); tail = last real work call -> turn end."""
    rows = []
    for t in runs:
        res = analyze(t, evs.get(t["id"], []))
        if not res:
            continue
        marks = res[2]
        segs = build_segments(t, marks, messages_for(mc, t))
        body = [x for x in segs if x[2] not in ("queue_wait", "daemon_prepare", "agent_boot", "teardown")]
        tools = [x for x in body if x[2] == "tool"]
        cats = [call_kind(x) for x in tools]
        work = [i for i, c in enumerate(cats) if c not in ORIENT]
        if not work:
            continue
        fi, li = work[0], work[-1]
        head_end, tail_start = tools[fi][0], tools[li][1]
        head = [x for x in body if x[1] <= head_end + 1e-6]
        tail = [x for x in body if x[0] >= tail_start - 1e-6]
        dur = lambda xs, f: sum(x[1] - x[0] for x in xs if f(x))
        rows.append({"agent": agents.get(t["agent_id"], t["agent_id"][:8]), "cats": cats, "fi": fi, "li": li,
                     "head": head_end - body[0][0], "tail": body[-1][1] - tail_start,
                     "run": body[-1][1] - body[0][0] + res[0]["agent_boot"],
                     "h_mul": dur(head, lambda x: x[2] == "tool" and call_kind(x) == "multica"),
                     "t_mul": dur(tail, lambda x: x[2] == "tool" and call_kind(x) == "multica"),
                     "h_model": dur(head, lambda x: x[2] == "model"),
                     "t_model": dur(tail, lambda x: x[2] in ("model", "final_answer")),
                     "t_final": dur(tail, lambda x: x[2] == "final_answer")})
    if not rows:
        print("\nSTART/END: no run had a real work call to split on")
        return
    mean = lambda k, v=rows: sum(x[k] for x in v) / len(v)
    print(f"\nSTART / END OF RUNS ({len(rows)} of {len(runs)} runs have a real work call; head = before it, tail = after the last one)")
    for title, pick in (("first 3 tool calls", lambda r: r["cats"][:3]), ("last 3 tool calls", lambda r: r["cats"][-3:])):
        c = collections.Counter(k for r in rows for k in pick(r))
        n = sum(c.values())
        print(f"  {title}: " + ", ".join(f"{k} {v / n * 100:.0f}%" for k, v in c.most_common()))
    print(f"  {'agent':<14}{'runs':>5}{'head s':>8}{'tail s':>8}{'of run':>8}")
    by = collections.defaultdict(list)
    for r in rows:
        by[r["agent"]].append(r)
    for n, v in sorted(by.items()) + [("ALL", rows)]:
        h, tl, rr = mean("head", v), mean("tail", v), mean("run", v)
        print(f"  {n:<14}{len(v):5d}{h:8.1f}{tl:8.1f}{(h + tl) / rr * 100:7.0f}%")
    print(f"  head: multica calls {mean('h_mul'):.2f}s of {mean('head'):.1f}s (the rest is model round trips between orientation calls)")
    print(f"  tail: multica calls {mean('t_mul'):.2f}s, final answer {mean('t_final'):.1f}s, model before it {mean('t_model') - mean('t_final'):.1f}s")


def bar(frac, width=28):
    n = int(round(frac * width))
    return "█" * n + "·" * (width - n)


def fmt(s):
    return f"{s:6.1f}s"


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("issues", nargs="*")
    ap.add_argument("--mc", default=os.environ.get("MULTICA_BIN", "multica"))
    ap.add_argument("--log", action="append", default=[], help="optional daemon log(s): adds exact daemon phase timing (prepare/environment); default is server data only")
    ap.add_argument("--top", type=int, default=5)
    ap.add_argument("--save-baseline", metavar="FILE", help="store boot/tok-per-s medians of these runs (probe runs made while the service is idle)")
    ap.add_argument("--ends", action="store_true", help="also report what agents do right after starting and right before finishing")
    ap.add_argument("--html", metavar="FILE", help="write a self-contained HTML report comparing the issues")
    ap.add_argument("--baseline", metavar="FILE", help="compare these runs against a saved idle baseline")
    a = ap.parse_args()
    logs = a.log
    runs = []
    for key in a.issues:
        r = subprocess.run([a.mc, "issue", "runs", key, "--output", "json"], capture_output=True, text=True)
        if r.returncode:
            sys.exit(f"{key}: {r.stderr.strip()}")
        for t in json.loads(r.stdout):
            t["_issue"] = key
            runs.append(t)
    if not runs:
        sys.exit("no runs found")
    runs.sort(key=lambda t: t["created_at"])
    agents = {}
    r = subprocess.run([a.mc, "agent", "list", "--output", "json"], capture_output=True, text=True)
    if r.returncode == 0:
        try:
            agents = {x["id"]: x["name"] for x in json.loads(r.stdout)}
        except Exception:
            pass
    evs = events_for(runs, logs) if logs else {t["id"]: server_events(t, messages_for(a.mc, t)) for t in runs}

    total = collections.Counter()
    per_agent = collections.defaultdict(collections.Counter)
    records = []  # one per run, feeds --html
    first_created = {}
    samples = []  # (label, agent_boot_s, output_tokens, generation_s)
    all_slow = []
    prev = None
    handoffs, overlaps, qwaits = [], [], []
    spans = {}
    shift, idle_total = collections.Counter(), collections.Counter()
    wall0 = to_local(runs[0]["created_at"])
    wall1 = to_local(runs[0]["created_at"])
    print(f"{'#':>2} {'agent':<12} {'wall':>7}  top buckets")
    for i, t in enumerate(runs, 1):
        res = analyze(t, evs.get(t["id"], []))
        name = agents.get(t["agent_id"], t["agent_id"][:8])
        if not res:
            print(f"{i:>2} {name:<12} (no daemon log lines for task {t['id'][:8]})")
            continue
        B, slow, marks = res
        if prev is not None and prev[0] == t["_issue"]:
            gap = (to_local(t["created_at"]) - prev[1]).total_seconds()
            if 0 < gap <= IDLE_AFTER:
                B["handoff_gap"] = gap
                handoffs.append(gap)
            elif gap <= 0:
                overlaps.append(-gap)
            if gap > IDLE_AFTER:
                B["idle_wait"] = gap
                shift[t["_issue"]] += gap - 10  # drawn as a 10s break in the HTML
                idle_total[t["_issue"]] += gap
        end = marks.get("finished") or to_local(t["completed_at"])
        prev = (t["_issue"], end)
        wall1 = max(wall1, end)
        spans[t["_issue"]] = (spans.get(t["_issue"], (to_local(t["created_at"]), end))[0], end)
        wall = sum(v for k, v in B.items() if k not in ("handoff_gap", "idle_wait"))
        top = ", ".join(f"{k} {v:.1f}s" for k, v in B.most_common(3))
        print(f"{i:>2} {name:<12} {wall:6.1f}s  {top}")
        total.update(B)
        c0 = first_created.setdefault(t["_issue"], to_local(t["created_at"]))
        records.append({"issue": t["_issue"], "agent": name, "start": (to_local(t["created_at"]) - c0).total_seconds() - shift[t["_issue"]],
                        "idle_total": round(idle_total[t["_issue"]]),
                        "segs": build_segments(t, marks, messages_for(a.mc, t)) if a.html else [],
                        "buckets": {k: round(v, 2) for k, v in B.items() if k != "idle_wait"},
                        "out_tokens": sum(u.get("output_tokens", 0) for u in (t.get("usage") or [])),
                        "ctx_tokens": sum(u.get("cache_read_tokens", 0) + u.get("cache_write_tokens", 0) + u.get("input_tokens", 0) for u in (t.get("usage") or []))})
        out_tok = sum(u.get("output_tokens", 0) for u in (t.get("usage") or []))
        samples.append((f"{t['_issue']}/{name}", B["agent_boot"], out_tok, B["model"] + B["final_answer"]))
        if "queue_wait" in B:
            qwaits.append(B["queue_wait"])
        per_agent[name].update(B)
        for d, n, ts in slow[: a.top]:
            all_slow.append((d, n, name, ts))

    idle = total.pop("idle_wait", 0.0)
    grand = sum(total.values()) or 1
    wall = sum((b - a).total_seconds() for a, b in spans.values())
    print(f"\nWALL CLOCK {wall:.0f}s over {len(runs)} runs in {len(spans)} issue(s) (bucket sum {grand:.0f}s)\n")
    print("WHERE THE TIME WENT")
    for k, v in total.most_common():
        print(f"  {k:<28}{fmt(v)} {v / grand * 100:5.1f}%  {bar(v / grand)}")

    groups = collections.Counter()
    for k, v in total.items():
        groups["tools" if k.startswith("tool:") else k] += v
    print("\nBY STAGE")
    for k, v in groups.most_common():
        print(f"  {k:<28}{fmt(v)} {v / grand * 100:5.1f}%")

    plat = sum(groups[k] for k in ("queue_wait", "daemon_prepare", "teardown", "handoff_gap"))
    design = groups["agent_boot"] + groups["final_answer"]
    n = len(runs)
    print("\nHANDOFF OVERHEAD")
    if handoffs:
        hs = sorted(handoffs)
        print(f"  run-to-run gaps: n={len(hs)} median {hs[len(hs)//2]:.2f}s max {hs[-1]:.2f}s; excluded idle waits >{IDLE_AFTER}s: {idle:.0f}s")
    if qwaits:
        qs = sorted(qwaits)
        print(f"  claim latency (created -> daemon claimed): median {qs[len(qs)//2]:.2f}s p90 {qs[int(len(qs)*0.9)-1]:.2f}s max {qs[-1]:.2f}s over {len(qs)} runs")
    if overlaps:
        print(f"  next run was created {sum(overlaps)/len(overlaps):.1f}s on average BEFORE the previous run ended ({len(overlaps)} handoffs): handoff overlaps the sender's tail, so it is off the critical path")
    print(f"  platform-owned (queue+prepare+teardown+gap) {plat:7.1f}s {plat / grand * 100:5.1f}%  {plat / n:.2f}s per run")
    print(f"  design-induced (agent_boot+final_answer)    {design:7.1f}s {design / grand * 100:5.1f}%  {design / n:.2f}s per run (scales with model speed)")
    print(f"  useful work   (model thinking+tools)        {grand - plat - design:7.1f}s {(grand - plat - design) / grand * 100:5.1f}%")
    print("\nBY AGENT (seconds)")
    cols = ["queue_wait", "daemon_prepare", "agent_boot", "model", "tools", "final_answer", "handoff_gap"]
    print("  " + f"{'agent':<12}" + "".join(f"{c[:12]:>13}" for c in cols))
    for n, B in sorted(per_agent.items()):
        g = collections.Counter()
        for k, v in B.items():
            g["tools" if k.startswith("tool:") else k] += v
        print("  " + f"{n:<12}" + "".join(f"{g[c]:13.1f}" for c in cols))

    print(f"\nSLOWEST SINGLE TOOL CALLS")
    for d, n, ag, ts in sorted(all_slow, reverse=True)[: a.top]:
        print(f"  {d:6.1f}s  {n:<32} {ag:<12} {ts:%H:%M:%S}")

    # verdict
    stages = [(k, v) for k, v in groups.items()]
    worst, wv = max(stages, key=lambda kv: kv[1])
    hint = {
        "model": "the model spends the time thinking/writing: shrink prompts and context, use a faster model, cut review/verification loops",
        "tools": "tool calls dominate: see the slowest calls above (git_clone / test runs / skill loads)",
        "agent_boot": "CLI start + first model call dominates: trim skills, instructions and MCP tool definitions per agent",
        "queue_wait": "runs wait to be claimed: check daemon concurrency / polling",
        "handoff_gap": "time between runs: server reassign + wakeup, or a human/agent forgot to move the status",
        "daemon_prepare": "daemon workdir/skills preparation dominates",
        "final_answer": "closing message generation dominates: shorten required output templates",
        "teardown": "daemon result reporting dominates",
    }.get(worst, "")
    print(f"\nBOTTLENECK: {worst} = {wv / grand * 100:.0f}% of attributed time. {hint}")

    if a.ends:
        ends_report(runs, evs, agents, a.mc)

    if a.html:
        write_html(a.html, records)
        print(f"\nHTML report: {a.html}")

    def tps(x):
        return x[2] / x[3] if x[3] > 5 and x[2] > 300 else None

    if a.save_baseline:
        boots = sorted(x[1] for x in samples)
        rates = sorted(r for r in map(tps, samples) if r)
        if not boots or not rates:
            sys.exit("baseline needs runs with >300 output tokens and >5s generation")
        base = {"boot_s": boots[len(boots) // 2], "boot_p90_s": boots[int(len(boots) * 0.9) - 1],
                "tok_per_s": rates[len(rates) // 2], "tok_per_s_p10": rates[len(rates) // 10], "n": len(boots)}
        json.dump(base, open(a.save_baseline, "w"), indent=2)
        print(f"\nBASELINE saved to {a.save_baseline}: {base}")
    if a.baseline:
        base = json.load(open(a.baseline))
        print(f"\nLOAD CHECK vs idle baseline (boot {base['boot_s']:.1f}s, {base['tok_per_s']:.0f} tok/s, n={base['n']})")
        flagged, ratios = [], []
        for lab, boot, o, g in samples:
            r_boot = boot / base["boot_s"]
            r = tps((lab, boot, o, g))
            r_gen = base["tok_per_s"] / r if r else None  # >1 means slower than idle
            ratios.append(r_gen) if r_gen else None
            if r_boot > 2 or (r_gen and r_gen > 1.5):
                flagged.append((lab, r_boot, r_gen))
        for lab, rb, rg in flagged:
            print(f"  SLOW {lab:<24} boot x{rb:.1f}" + (f"  generation x{rg:.1f}" if rg else ""))
        med = sorted(ratios)[len(ratios) // 2] if ratios else None
        verdict = "no comparable runs"
        if med:
            verdict = ("generation at idle speed: the model is near its best, remaining time is workload" if med < 1.25
                       else "generation slower than idle baseline: service load (or heavier requests) is adding latency")
        print(f"  {len(flagged)}/{len(samples)} runs flagged; median generation slowdown x{med:.2f}" if med else "  no runs long enough to compare generation")
        print(f"  VERDICT: {verdict}")


HTML = r"""<!doctype html><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>Run trace</title>
<style>
:root{--bg:#fff;--fg:#1c1c1e;--mut:#6b6b70;--line:#e3e3e6;--card:#f6f6f8}
@media (prefers-color-scheme:dark){:root{--bg:#161618;--fg:#ececee;--mut:#9a9aa2;--line:#2c2c30;--card:#202024}}
body{margin:0;padding:20px 16px;background:var(--bg);color:var(--fg);font:14px/1.45 system-ui,sans-serif;max-width:1200px;margin:auto}
h1{font-size:20px;margin:0 0 4px}h2{font-size:15px;margin:26px 0 8px}.mut{color:var(--mut)}
.tabs button{border:1px solid var(--line);background:var(--card);color:var(--fg);padding:4px 12px;border-radius:14px;cursor:pointer;margin-right:6px}
.tabs button.on{background:var(--fg);color:var(--bg)}
.leg{display:flex;flex-wrap:wrap;gap:6px 14px;margin:12px 0}.leg span{display:flex;align-items:center;gap:5px;font-size:12px}
.leg b{width:11px;height:11px;border-radius:2px;display:inline-block}
.grp{margin:16px 0 4px;display:flex;justify-content:space-between;align-items:baseline;border-top:1px solid var(--line);padding-top:8px}
.lane{display:grid;grid-template-columns:96px 1fr;gap:8px;align-items:center;margin:2px 0}
.lane em{font-style:normal;font-size:12px;text-align:right;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.trk{position:relative;height:24px;background:var(--card);border-radius:3px}
.s{min-width:3px;cursor:pointer;position:absolute;top:0;height:100%;box-sizing:border-box;border-right:1px solid var(--bg);font-size:10px;line-height:24px;padding-left:3px;color:#fff;overflow:hidden;white-space:nowrap;text-shadow:0 0 2px rgba(0,0,0,.45)}
.ax{position:relative;height:14px;margin-left:104px;border-bottom:1px solid var(--line)}
.ax i{position:absolute;top:0;font-style:normal;font-size:10px;color:var(--mut);transform:translateX(-50%)}
#wrap{overflow-x:auto;border:1px solid var(--line);border-radius:6px;padding:0 8px 8px}#in{width:calc(var(--z)*100%)}
.lane em{position:sticky;left:0;background:var(--bg);z-index:2;cursor:pointer}
.grp{position:sticky;left:0;width:calc(100%/var(--z))}
.ctl{display:flex;align-items:center;gap:10px;margin-top:10px;font-size:12px}.ctl input{width:220px}
#sel{position:sticky;bottom:0;z-index:5;background:var(--card);border:1px solid var(--line);border-radius:6px;padding:8px 10px;margin-top:10px;font-size:12px;white-space:pre-wrap;word-break:break-all;min-height:18px}
.ev td{font-size:11px;text-align:left}.ev td:nth-child(-n+4){white-space:nowrap}
table{border-collapse:collapse;width:100%;font-size:12px}td,th{padding:4px 8px;border-bottom:1px solid var(--line);text-align:right}td:first-child,th:first-child{text-align:left}
#tip{position:fixed;max-width:520px;background:var(--fg);color:var(--bg);padding:6px 9px;border-radius:5px;font-size:12px;pointer-events:none;display:none;white-space:pre-wrap;word-break:break-all;z-index:9}
</style>
<h1>Run trace</h1><div class=mut>One row per run, in real time order. Hover a block for its duration and command.</div>
<div class=tabs style=margin-top:14px><button id=m1 class=on>Common time scale</button><button id=m2>Each issue fills the width</button></div>
<div class=ctl>Zoom <input type=range id=z min=1 max=30 step=0.5 value=1><span id=zv>1x</span> <button id=zr>reset</button></div>
<div class=leg id=leg></div><div id=wrap><div id=in><div id=tl></div></div></div><div id=sel>Click a block for details; click an agent name to list all of its events.</div>
<h2>Totals per issue <span class=mut>(summed by type, not in time order)</span></h2><div id=tbl></div><div id=tip></div>
<script>
const D=__DATA__;
const FIX={queue_wait:["queue wait","#8e8e93"],daemon_prepare:["daemon prepare","#af52de"],agent_boot:["agent boot","#ff9f0a"],model:["model thinking","#0a84ff"],final_answer:["final answer","#5ac8fa"],teardown:["teardown","#a2845e"]};
const PAL=["#30d158","#ff375f","#bf5af2","#64d2ff","#ffd60a","#ac8e68","#00c7be","#ff6482","#7d7aff","#34c759"];
const tools=[...new Set(D.flatMap(r=>r.segs.filter(s=>s[2]=="tool").map(s=>s[3])))].sort();
const col=s=>s[2]=="tool"?PAL[tools.indexOf(s[3])%PAL.length]:FIX[s[2]][1];
const issues=[...new Set(D.map(r=>r.issue))];
const sum=o=>Object.values(o).reduce((a,b)=>a+b,0);
const short=n=>n.replace(/^mcp__/,"").replace(/__/g,".");
let common=true;const tip=document.getElementById("tip");
function draw(){
 document.getElementById("leg").innerHTML=Object.values(FIX).map(([n,c])=>`<span><b style="background:${c}"></b>${n}</span>`).join("")+tools.map(t=>`<span><b style="background:${PAL[tools.indexOf(t)%PAL.length]}"></b>tool: ${short(t)}</span>`).join("");
 const ends=issues.map(i=>Math.max(...D.filter(r=>r.issue==i).map(r=>r.start+Math.max(...r.segs.map(s=>s[1])))));
 const gmax=Math.max(...ends);
 document.getElementById("tl").innerHTML=issues.map((i,k)=>{
  const rs=D.filter(r=>r.issue==i),W=common?gmax:ends[k],step=[1,2,5,10,20,30,60,120,300].find(x=>W/x<=12)||600;
  let ax="";for(let t=0;t<=W;t+=step)ax+=`<i style="left:${t/W*100}%">${t}s</i>`;
  return`<div class=grp><b>${i}</b><span class=mut>${ends[k].toFixed(0)}s end to end · ${rs.length} run${rs.length>1?"s":""}${Math.max(...rs.map(x=>x.idle_total))?` · ${Math.max(...rs.map(x=>x.idle_total))}s of idle waiting between runs drawn as 10s breaks`:""}</span></div><div class=ax>${ax}</div>`+rs.map(r=>{
   const segs=r.segs.map(s=>{const a=r.start+s[0],b=r.start+s[1],w=(b-a)/W*100,lab=s[2]=="tool"?short(s[3]):(w>4?FIX[s[2]][0]:"");
    const t=`${s[2]=="tool"?short(s[3]):FIX[s[2]][0]}  ${(s[1]-s[0]).toFixed(1)}s\n${s[4]}\n${a.toFixed(1)}s -> ${b.toFixed(1)}s`;
    return`<div class=s data-t="${t.replace(/"/g,"&quot;")}" data-k="${i}|${r.agent}|${r.start}" style="left:${a/W*100}%;width:${w}%;background:${col(s)}">${w>3?lab:""}</div>`}).join("");
   return`<div class=lane><em title="${r.agent}" data-run="${D.indexOf(r)}">${r.agent}</em><div class=trk>${segs}</div></div>`}).join("")}).join("");
 const tot=issues.map(i=>{const g={};D.filter(r=>r.issue==i).forEach(r=>{for(const[k,v]of Object.entries(r.buckets)){const n=k.startsWith("tool:")?"tools":k;g[n]=(g[n]||0)+v}});return[i,g]});
 const cols=["queue_wait","daemon_prepare","agent_boot","model","tools","final_answer","teardown","handoff_gap"];
 document.getElementById("tbl").innerHTML="<table><tr><th>issue</th><th>runs</th><th>total s</th>"+cols.map(c=>`<th>${c.replace("_"," ")}</th>`).join("")+"<th>out tok</th><th>ctx tok</th></tr>"+tot.map(([i,g])=>{const rs=D.filter(r=>r.issue==i);return`<tr><td>${i}</td><td>${rs.length}</td><td>${sum(g).toFixed(0)}</td>`+cols.map(c=>`<td>${(g[c]||0).toFixed(0)}</td>`).join("")+`<td>${(rs.reduce((a,r)=>a+r.out_tokens,0)/1000).toFixed(1)}K</td><td>${(rs.reduce((a,r)=>a+r.ctx_tokens,0)/1000).toFixed(0)}K</td></tr>`}).join("")+"</table>";
}
document.addEventListener("mousemove",e=>{const t=e.target.dataset&&e.target.dataset.t;tip.style.display=t?"block":"none";if(t){tip.textContent=t;tip.style.left=Math.min(e.clientX+12,innerWidth-540)+"px";tip.style.top=e.clientY+14+"px"}});
m1.onclick=()=>{common=true;m1.className="on";m2.className="";draw()};m2.onclick=()=>{common=false;m2.className="on";m1.className="";draw()};
z.oninput=()=>{document.documentElement.style.setProperty("--z",z.value);zv.textContent=z.value+"x"};zr.onclick=()=>{z.value=1;z.oninput()};z.oninput();
document.addEventListener("click",e=>{const t=e.target.dataset;if(t&&t.t){sel.textContent=t.t}
 if(t&&t.run!==undefined){const r=D[+t.run],nx=e.target.closest(".lane").nextElementSibling;if(nx&&nx.className=="evbox"){nx.remove();return}
  const rows=r.segs.map(g=>`<tr><td>${(r.start+g[0]).toFixed(1)}s</td><td>${(g[1]-g[0]).toFixed(2)}s</td><td>${g[2]=="tool"?short(g[3]):FIX[g[2]][0]}</td><td>${g[4].replace(/</g,"&lt;")}</td></tr>`).join("");
  e.target.closest(".lane").insertAdjacentHTML("afterend",`<div class=evbox><table class=ev><tr><th>at</th><th>took</th><th>what</th><th>detail</th></tr>${rows}</table></div>`)}});
draw();
</script>"""


def write_html(path, records):
    for r in records:
        for g in r["segs"]:
            g[4] = g[4][:300] + ("..." if len(g[4]) > 300 else "")
    open(path, "w").write(HTML.replace("__DATA__", json.dumps(records)))


if __name__ == "__main__":
    main()
