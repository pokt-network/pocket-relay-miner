# CLAUDE.md

@AGENTS.md

@CONTRIBUTING.md

AGENTS.md routes a deployer or an operator; CONTRIBUTING.md holds every rule for
changing the code, and all of it applies to you. This file adds only what is
specific to an agent working here.

## How to work

- **Verify, then state.** Support a claim with a file and line, a command's
  output or a link. If you have not checked something, say so. If a task is
  ambiguous, ask before implementing; if you disagree with an approach, say why.
- **A review that finds nothing is suspicious.** Never "looks good" without test
  or build output behind it. Verify a claim from the user or another agent
  yourself before acting on it.
- **Durable artifacts are read as fact**: commit messages, PR descriptions, docs,
  hand-overs, memory. Verify what goes into them or mark it "not verified". A
  claim that something "cannot" or "never" happens needs evidence like any
  other claim.
- **When corrected, fix the artifact**, not only the conversation.

## Before the first edit: the council, then the success criterion

1. **Run a council before choosing the approach to any fix or feature.** A
   review afterwards does not replace it: a review reads code already written,
   so the approach it critiques is the one already chosen. Use members on
   different models; if they share one, say that their agreement is a common
   prior, not corroboration.
2. **Write the council's synthesis to a file** under `scripts/localonly/<item>/`,
   with the tensions and what each member rejected. A conclusion without its
   argument cannot be re-examined.
3. **Invoke `andrej-karpathy-skills:karpathy-guidelines` before the first edit.**
4. **Write the success criterion down, as something checkable.** "Level 2 passes
   and the new test goes red when the defect is re-injected" is one; "it works"
   is not. If you cannot write it, the task is not understood yet.
5. **When a test or live run goes red on a fix, the next step is a council on the
   whole design with every red so far**, not a patch for the last red.

## Evidence

- **A test has teeth only if it goes red with the defect injected, 5 times out of
  5**, and green again once restored. Inject the real defect, restore it from a
  copy, and confirm the red names the test you meant.
- **An empty result is not evidence until a control shows the tool looked.** Ask
  the same tool, at the same moment, something whose answer you know. A grep
  across wrapped lines, a directory the toolchain ignores (`_`-prefixed), a file
  full of NUL bytes and a count capped by a tool default all return silence that
  looks like "nothing there".
- **Filters change the question.** Stripping comments from an allowlist drops the
  reasons that live in them; a lint total equal to a default cap is the cap.
- **A count in a commit message is recounted in the tree**, all of them, after
  the last edit.
- **An instrument built from the defect's own material cannot detect it.** When
  the defect is a pattern (a lost property, a dropped field, a truncated value),
  grep the test harness for the same pattern; synthetic data must have the real
  value's shape, not only its size.
- **A gate writes to a file, and the file carries its own `EXIT=$?`.** Read the
  failing check's name from the log, not from a fixed `tail`, and not from the
  harness's completion notice, which reports the last command in the pipeline.
- **A gate measures the tree that existed when it started.** Do not edit while a
  gate or a repeat loop runs; when a repeat loop is the evidence, record the
  `sha256` of the files under test. Delete the logs of an aborted run.
- **When you remove a default from a constructor or move when something happens,
  enumerate every caller** (`grep -rn` the constructor), and run the packages
  whose tests watch traffic or counters with `-count=5`.
- **Before quoting a performance figure, re-run the benchmark or load test** and
  cite that run.

## Shared machine and live environment

- **The maintainer starts and stops Tilt.** With Tilt up, do not edit `.go` files:
  a rebuild competes with the live gate. With Tilt down, the live gate fails
  preflight; `kubectl port-forward` is not the way out.
- **Only stop processes you started**, by task handle or exact PID; never a broad
  `pkill` or `killall`. Other projects' tests and builds run on this machine.
- **Scripts, logs and scratch go under `scripts/localonly/`**, never `/tmp`.
  Anything that must outlive this session runs in `tmux`.
- **Between two sessions, the tree is the artifact and the message is the
  report.** Work on disk is ready to read; message a peer as soon as you verify.

## Closing and asking

- **Review every commit before offering a push**, and write the review down.
- **Push, PR, merge and tag are asked for**, never done on your own, and the
  question comes after the review and the gates, not instead of them.
- **Use the `close-session` skill.** `scripts/localonly/QUEUE.md` holds only
  items the maintainer approved; every finding goes to the hand-over's "Proposed
  findings" with two questions (queue it? open an issue?). The canonical
  hand-over is whatever `scripts/handoff-index.sh` says.
