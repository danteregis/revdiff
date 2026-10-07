---
name: revdiff-walkthrough
description: Walk the user through a diff in revdiff with Claude's notes beside the code — a short overview per file and notes on tricky lines, in a suggested reading order — and answer the user's replies live while revdiff stays open. Activates on "walk me through this diff", "walkthrough", "explain this PR in revdiff", "guided review", "revdiff walkthrough", "annotate the diff for me", "explain your changes in revdiff", "review with notes".
argument-hint: 'optional: ref(s) or PR number'
allowed-tools: [Bash, Read, Write, Grep, Glob]
---

# revdiff walkthrough — Claude's notes beside the diff

You explain a diff inside revdiff: an **overview** per new or heavily changed file (what it is for, entry points, collaborators) and **notes** on specific lines (`explain`, or `caution` for tricky spots), with a suggested reading order. The user reads them in the notes pane (or inline on narrow terminals), replies to a note, and you answer while revdiff stays open.

Notes are not annotations. They never appear in revdiff's annotation output, and the user's replies to them are **questions and discussion, not change requests** — answer them, do not edit code because of a reply unless the user explicitly asks in it. The annotations revdiff returns when the review closes are the change requests; handle them exactly like the `revdiff` skill does (kind labels `bug`/`suggestion`/`question`/`nitpick`/`praise` apply, Step 3 onward of that skill). Notes need git: they are kept in the branch's review-session directory (`~/.config/revdiff/sessions/…/<branch>/notes/`), never in the repository.

Scripts of the sibling `revdiff` skill are used below: `SCRIPTS="${CLAUDE_SKILL_DIR}/../revdiff/scripts"`.

## Step 1: Pick the ref

Use the user's ref when given. Otherwise run `"$SCRIPTS/detect-ref.sh"` and apply the `revdiff` skill's rules (`suggested_ref`, `use_staged`, ask when `needs_ask: true`). For a branch or PR review use the **three-dot** form `<base>...<branch>` (what GitHub shows): `origin/master...feat/x`. Keep that exact string in `REF` and pass it to every command below as `--ref "$REF"` (use `--staged` instead for a staged review, nothing for uncommitted changes) — the notes belong to the branch that ref resolves to.

## Step 2: Read the diff and write the notes

Read the diff (`git diff "$REF"`) and the surrounding code you need. Write a notes file to a temp path **outside the repository** (`mktemp /tmp/revdiff-notes-XXXXXX.json`):

```json
{"format":"revdiff-notes/v1","target":{"ref":"origin/master...feat/x"},"author":"claude",
 "tour":["app/cache.rb","app/clock.rb"],
 "files":[
  {"path":"app/cache.rb","overview":"Adds TTL expiry to Cache. Entry points: get/set. Only collaborator is the backing store hash.",
   "notes":[
    {"kind":"caution","line":11,"body":"Expired entries are skipped but never deleted, so the store grows until the key is overwritten."},
    {"kind":"explain","line":16,"side":"+","body":"Write time is stored with the value; expiry is decided on read."}]}]}
```

- `line` is the line number in the new file (`side` `+`, the default) or in the old file for a removed line (`side` `-`). Ids are optional; revdiff assigns them and anchors each note to the current diff so it follows the code when lines move. A note whose line is not in the diff is kept but shown as outdated.
- **Overview** every new or heavily changed file; skip trivial files.
- **Budget**: about one line note per 40 changed lines. Prefer `caution` (non-obvious behavior, edge cases, risks, things a reviewer could miss) over `explain`; do not narrate what the code plainly says.
- For code **you wrote in this session**, record the *why* behind choices: rejected alternatives, constraints, trade-offs.
- `tour` is the reading order (entry point first, then what it calls).

Import it, then delete the temp file:

```bash
revdiff notes import --ref "$REF" /tmp/revdiff-notes-XXXXXX.json
```

It prints a summary and warns about notes it could not place; fix and re-import if a warning shows a wrong line. Re-importing replaces the branch's notes but keeps the discussion of every note whose `id` you keep. Single additions: `revdiff note add --ref "$REF" --file F --line N [--side -] [--kind caution] "body"`, `revdiff note overview --ref "$REF" --file F "body"` (both print the note id; `-` as the body reads it from stdin).

## Step 3: Open revdiff in the background

Launch the review through the `revdiff` skill's launcher **with `run_in_background: true`** (unlike a plain review, the launcher must not block: you keep answering replies while it is open). You are notified when it finishes, with the user's annotations as its output:

```bash
"$("$SCRIPTS/resolve-launcher.sh" launch-revdiff.sh "${CLAUDE_PLUGIN_DATA}")" "$REF"
```

Tell the user the keys: `)`/`(` next/previous note in reading order, `r` reply to a note, `c` turn a note into a change request (a `suggestion` annotation quoting it), `>` show/hide the notes pane, `-` fold the overview.

## Step 4: Answer replies while the review is open

Start the waiter as a second background command:

```bash
revdiff inbox --ref "$REF" --wait --timeout 10m
```

It prints one JSON object per new reply and exits:

```json
{"note":{"id":"n3","file":"app/cache.rb","line":11,"side":"+","kind":"caution","body":"…","outdated":false},
 "thread":[{"id":"n3.1","author":"you","body":"…","at":"…"}],
 "reply":{"id":"n3.2","body":"why not purge on read?","at":"…"}}
```

- **Exit 0** (replies printed): answer each one with `revdiff note reply --ref "$REF" <note id> "answer"` — the note, the earlier thread and the reply are all in the line, so answer in context and briefly. Then start the waiter again.
- **Exit 3** (timeout, nothing new): start it again if the review is still open.
- **Exit 4** (the review closed): stop waiting.

While it waits, revdiff shows the user that an agent is listening; without a waiter it says "no agent listening" and replies queue until you run `revdiff inbox`. You may add or refine notes at any time (`note add`, `note overview`, `note reply` on any note, `notes import`); revdiff shows them live.

## Step 5: The review closes

When the background launcher finishes, stop the waiter if it is still running (it exits 4 on its own within a second) and process the launcher's output as the `revdiff` skill's Step 3: those annotations are the change requests. If a waiter exit or a final `revdiff inbox --ref "$REF"` shows replies that arrived just before the close, answer them in the chat. No output means the user had no change requests.

To refresh the notes after code changes (notes on changed lines show as outdated), redo Step 2 and re-import — revdiff never regenerates notes on its own.
