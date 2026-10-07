---
name: revdiff-walkthrough
description: Walk the user through a diff in revdiff with the agent's notes beside the code — a short overview per file and notes on tricky lines, in a suggested reading order — and answer the user's replies live while revdiff stays open. Activates on "walk me through this diff", "walkthrough", "explain this PR in revdiff", "guided review", "revdiff walkthrough", "annotate the diff for me", "explain your changes in revdiff", "review with notes".
---

# revdiff walkthrough — notes beside the diff

You explain a diff inside revdiff: an **overview** per new or heavily changed file (what it is for, entry points, collaborators) and **notes** on specific lines (`explain`, or `caution` for tricky spots), with a suggested reading order. The user reads them in the notes pane (or inline on narrow terminals), replies to a note, and you answer while revdiff stays open.

Notes are not annotations. They never appear in revdiff's annotation output, and the user's replies to them are **questions and discussion, not change requests** — answer them, do not edit code because of a reply unless the user explicitly asks in it. The annotations revdiff returns when the review closes are the change requests; handle them exactly like the `revdiff` skill does (kind labels `bug`/`suggestion`/`question`/`nitpick`/`praise` apply, Step 3 onward of that skill). Notes need git: they are kept in the branch's review-session directory (`~/.config/revdiff/sessions/…/<branch>/notes/`), never in the repository.

## Script Path Resolution

Resolve `<plugin-root>` from this skill's absolute path in the available-skills catalogue: the directory containing this plugin's `.codex-plugin/plugin.json`. Then set `SCRIPT_DIR="<plugin-root>/skills/revdiff/scripts"` (the sibling `revdiff` skill's scripts).

## Step 1: Pick the ref

Use the user's ref when given. Otherwise run `"$SCRIPT_DIR/detect-ref.sh"` and apply the `revdiff` skill's rules (`suggested_ref`, `use_staged`, ask when `needs_ask: true`). For a branch or PR review use the **three-dot** form `<base>...<branch>`: `origin/master...feat/x`. Keep that exact string in `REF` and pass it to every command below as `--ref "$REF"` (use `--staged` instead for a staged review, nothing for uncommitted changes).

## Step 2: Read the diff and write the notes

Read the diff (`git diff "$REF"`) and the code around it. Write a notes file to a temp path **outside the repository** (`mktemp /tmp/revdiff-notes-XXXXXX.json`):

```json
{"format":"revdiff-notes/v1","target":{"ref":"origin/master...feat/x"},"author":"codex",
 "tour":["app/cache.rb","app/clock.rb"],
 "files":[
  {"path":"app/cache.rb","overview":"Adds TTL expiry to Cache. Entry points: get/set.",
   "notes":[
    {"kind":"caution","line":11,"body":"Expired entries are skipped but never deleted, so the store grows."},
    {"kind":"explain","line":16,"body":"Write time is stored with the value; expiry is decided on read."}]}]}
```

- `line` counts in the new file (`side` `+`, the default) or the old file for a removed line (`side` `-`). Ids are optional; revdiff assigns them and anchors notes to the current diff. A note whose line is not in the diff is shown as outdated.
- **Overview** every new or heavily changed file. **Budget** about one line note per 40 changed lines; prefer `caution` over `explain`; for code you wrote in this session, record the *why* behind choices.
- `tour` is the reading order.

```bash
revdiff notes import --ref "$REF" /tmp/revdiff-notes-XXXXXX.json
```

Fix and re-import if a warning shows a misplaced note (re-importing keeps the discussion of every note whose `id` you keep). Single additions: `revdiff note add --ref "$REF" --file F --line N [--side -] [--kind caution] "body"`, `revdiff note overview --ref "$REF" --file F "body"`.

## Step 3: Open revdiff without blocking

Start the launcher in the background of one shell command, keeping its output:

```bash
OUT=$(mktemp /tmp/revdiff-walkthrough-XXXXXX.out)
nohup "$SCRIPT_DIR/launch-revdiff.sh" "$REF" > "$OUT" 2>&1 &
echo "$OUT"
```

The launcher's overlay rules (approval escalation in a sandbox, pane-scoped modes) are those of the `revdiff` skill. Tell the user the keys: `)`/`(` next/previous note, `r` reply, `c` turn a note into a `suggestion` annotation, `>` show/hide the notes pane, `-` fold the overview.

## Step 4: Answer replies until the review closes

Loop on the waiter in the foreground (set the command timeout above `--timeout`):

```bash
revdiff inbox --ref "$REF" --wait --timeout 5m
```

It prints one JSON object per new reply: the `note` (id, file, line, kind, body, outdated), the earlier `thread`, and the `reply`.

- **Exit 0**: answer each reply with `revdiff note reply --ref "$REF" <note id> "answer"`, briefly and in context, then run the waiter again.
- **Exit 3** (timeout): run it again.
- **Exit 4** (the review closed): go to Step 5.

## Step 5: The review closes

Read `$OUT` — the launcher's output is the user's annotations, the change requests; process them as the `revdiff` skill's Step 3 (exit status `10` there means annotations were captured). Answer any replies a final `revdiff inbox --ref "$REF"` still shows in the chat. To refresh notes after code changes, redo Step 2; revdiff never regenerates notes on its own.
