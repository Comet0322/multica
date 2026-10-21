# run_trace

Explains where the wall-clock time of agent runs goes: one command, no dependencies beyond Python 3 and a logged-in `multica` CLI.

It reads the workspace's own records (`multica issue runs`, `multica issue run-messages`), so it works from any machine that can reach the workspace, regardless of where the runs executed.

## Usage

```bash
python3 run_trace.py ISSUE-1 ISSUE-2                  # text report with a one-line bottleneck verdict
python3 run_trace.py ISSUE-1 ISSUE-2 --ends           # + what agents do right after starting and right before finishing
python3 run_trace.py ISSUE-1 ISSUE-2 --html out.html  # + interactive timeline, one row per run
```

| Option | Meaning |
| --- | --- |
| `--mc PATH` | `multica` binary, default `multica` on `PATH` (or `MULTICA_BIN`) |
| `--ends` | Classify the first and last tool calls (multica CLI, Skill, ToolSearch, git gateway, real work) and time the part before the first and after the last real work call |
| `--html FILE` | Self-contained report: zoom slider, click a block to pin its command, click an agent name to list all of its events |
| `--save-baseline FILE` | Save boot time and tokens/s medians from probe runs made while the model service is idle |
| `--baseline FILE` | Compare the given runs against a saved baseline; flags runs more than 1.5x slower at generating or 2x slower to boot |
| `--log FILE` | Optional daemon log (repeatable) for exact daemon phase timing; only usable on the daemon's machine |
| `--top N` | How many slowest tool calls to list (default 5) |

## What the buckets mean

Every second of a run goes into exactly one bucket.

| Bucket | Covers |
| --- | --- |
| `queue_wait` | run created until the daemon claimed it |
| `daemon_prepare` | claimed until the agent CLI launched (only with `--log`; otherwise folded into `agent_boot`) |
| `agent_boot` | agent CLI launched until its first output |
| `model` | time inside the agent's turn with no tool running: thinking, and generating the next tool call's arguments |
| `tool:<name>` | time inside a tool call (overlapping calls share the time) |
| `final_answer` | last tool result until the turn ends |
| `teardown` | turn end until the run is closed |
| `handoff_gap` | previous run finished until the next run of the same issue was created, up to 120 s; longer pauses are treated as someone waiting and excluded |

The report also splits the total into platform-owned time (queue, prepare, teardown, handoff), design-induced time (`agent_boot` + `final_answer`, which scales with model speed) and useful work (model + tools).

## Measuring against an idle baseline

To tell service load apart from the model's normal speed, make a few identical probe runs while the service is idle (a fixed prompt, no tools, a fixed output length of roughly 800 tokens), then:

```bash
python3 run_trace.py PROBE-1 PROBE-2 ... --save-baseline idle.json
python3 run_trace.py ISSUE-1 --baseline idle.json
```

Only runs with more than 300 output tokens and more than 5 s of generation are compared. The probe has no tool calls, so tool round-trip latency is not covered.

## Limits

- Run-level timestamps from the server have one-second resolution, so sub-second queue time is not measurable and `agent_boot` can be off by up to about a second. Tool call times come from message timestamps and are millisecond-accurate.
- `model` includes the time spent generating tool arguments (for example a long heredoc that writes a file); it cannot be split into thinking and generating.
- Final-answer time ends at the last text message the agent reported, which is up to a few seconds shorter than the daemon's own end-of-run mark.
- `--ends` classifies Bash commands by matching `multica <subcommand>` in the command text and skips runs with no real work call.
- Token figures come from the run's `usage`; if the model does not report output tokens, the tokens/s comparison is unavailable.

## 繁體中文簡述

分析 agent run 的時間花在哪。只需要 Python 3 和已登入的 `multica` CLI,資料取自 workspace 的 issue 紀錄,不需要本機 daemon 日誌。

```bash
python3 run_trace.py ISSUE-1 ISSUE-2            # 瓶頸分析
python3 run_trace.py ISSUE-1 ISSUE-2 --ends     # 另外分析 agent 剛啟動與要結束時的 tool call
python3 run_trace.py ISSUE-1 ISSUE-2 --html out.html   # 互動時序圖
```

每一秒都只會歸入一個類別(排隊、啟動、模型、各個 tool、最終回報等)。想區分「服務負載」和「模型本來的速度」,先在服務空閒時跑幾個相同的探針 run,用 `--save-baseline` 存基準,之後用 `--baseline` 比較。限制見上方 Limits。
