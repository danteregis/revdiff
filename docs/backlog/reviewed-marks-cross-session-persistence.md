---
worth: later
where: app/ui/sidepane/filetree.go:FileTree
added: 2026-08-20
---
# reviewed marks do not survive the process

Reviewed marks (`Space`) live only in `FileTree.reviewed` and are lost on exit, so `F` (unreviewed-only)
is useful within one session and worthless across two. `--filter-unreviewed` (#366) makes that sharper:
a scripted launch can now enable the filter without a keypress, and with no marks to preload it filters
nothing. Nothing else preserves them: `history.Save` returns early when there are no annotations
(`app/history/history.go:43`), so a session that produced marks and no annotations writes nothing at
all. Requested in #324, which asked for `--reviewed-output` to write bare paths and `--reviewed` to
preload them. The need was accepted publicly; the bare-path
format was not.

Constraints settled while answering #324, so they do not have to be re-derived:

- **The file must carry the fingerprint, not just the path.** A mark is `path -> semantic diff
  fingerprint`, and `SetReviewed` refuses an empty fingerprint. Preloading bare paths either gets
  dropped by the first `ReconcileReviewed` or silently adopts today's fingerprint, which makes `F` hide
  files whose content was never read. That is the defect the fingerprint mechanism was added to prevent.
- **Structured and versioned, not line-oriented.** Paths may contain tabs and newlines; revdiff already
  strips control bytes before display (`app/ui/style/display.go:17`), so a delimited format cannot
  round-trip a hostile or merely unusual path. JSON escaping handles those paths; the objection is to
  raw delimiters, not to a line-oriented file as such.
- **Import and export stay separate.** GitHub's `viewerViewedState` returns paths with no fingerprint, so
  an inbound GitHub list can only ever be advisory and has to be documented as such.
- **Export belongs inside the `!signaled` gate, next to `-o`.** `finalize` returns `(0, nil)` on a signal
  (`app/revdiff/main.go:finalize`), so a checkpoint written on SIGTERM is indistinguishable to a
  calling script from a finished review and would publish a partial set as complete. It must, however,
  sit outside the `annotations == ""` guard in the same function, or the marks-only case #324 is about
  still writes nothing.
- **No ref-range equality gate on the file.** It would discard marks after a rebase, which is exactly the
  case `FileFingerprint` deliberately preserves.
- **Preload must go through `loadReviewedFingerprints`, not a bare hash compare.** That path also applies
  `ReviewFingerprintStable` (`app/ui/loaders.go`), which drops binary and placeholder rows whose rendered
  text can stay identical while the file changes. A preload that only compares `FileFingerprint` results
  re-admits exactly those. Seeding `FileTree.reviewed` before `Init` runs `loadFiles` reuses the whole
  existing check and needs no new matching code.

## Why marks in review history, restored with no flags, is the wrong shape

The reporter asked for this in a follow-up on 2026-09-09 (after this item was first written), arguing it
covers the "next morning" case with no flags at all. Four findings against it:

- **It hides annotations from the agent path.** Both copies of `read-latest-history.sh` return the newest
  `.md` without inspecting it, and the skill reads an empty `## Annotations` as "quit without
  annotating". Probed against both current scripts with a synthetic history: a newer marks-only snapshot
  was selected and the older annotated review was omitted.
- **Auto-save silently shrinks the set.** `Rebuild` deletes marks for any path not in the current entry
  list, and `ReconcileReviewed` drops any path missing from the load. One run under `--only`,
  `--include`, or with untracked off saves the smaller set, and the next full run restores only that.
  With an explicit flag the user picks the file; with auto-restore this is invisible.
- **There is no sound lookup key.** The binary never reads history back (`Save` is the only entry point),
  history directories are keyed by repo basename (#331, still open — note the maintainer there endorsed
  reader-side path-header filtering and rejected a layout change, so auto-restore needs correct checkout
  selection rather than all of #331). "Same base/head" does not exist in most modes: working-tree and
  staged reviews record no ref, the commit hash is git-only, hg and jj record none, and compare mode keys
  on a directory. Keying on resolved SHAs would break the rebase survival the fingerprint exists for.
- **Every run pays the startup cost**, where an opt-in flag pays it only when asked. See the cost note
  below.

## Open decisions, both the maintainer's

- does `Q` suppress the export? `Q` is documented as discarding annotations and a reviewed mark is not an
  annotation, but pressing `Q` usually means the session was a write-off.
- does an empty reviewed set write an empty snapshot or leave the previous file? Writing it is safer
  (stale marks cannot resurrect), leaving it is friendlier to a mistyped path.

## Cost and one adjacent gap

Preloaded marks are validated by the existing pipeline, but `loadReviewedFingerprints` runs inside
`loadFiles` before `filesLoadedMsg` is emitted, so each preloaded path costs one effective-diff fetch
through the 4-worker pool before the file list paints. Measure that for a large preload rather than
assuming it is free.

`fileFingerprintVersion` is only mixed into the hash and no test pins a digest, so any change to the diff
parser invalidates every saved mark and looks identical to a real content change. A persisted snapshot
should carry the fingerprint version as its own field so a mismatch can be reported instead of appearing
as "everything changed".

## Resolution: per-branch review sessions (`app/session`)

Implemented as silent per-branch sessions rather than history-embedded marks, answering the findings
above as follows:

- **Not in review history.** Sessions live in their own tree (`~/.config/revdiff/sessions/`), so
  `read-latest-history.sh` and the skill's reading of history are untouched.
- **No silent shrink.** `saveSession` merges: only paths in the current file list are updated or
  removed; marks for paths a narrowed run (`--only`, `--include`, untracked off) does not show are
  kept as stored.
- **Sound lookup key.** Repository identity is a hash of the normalized `origin` URL, else of the git
  common dir — never the basename (#331). The branch key is derived from the reviewed ref (checked-out
  branch for working-tree / staged / single-ref, `B` of `A..B`), never from resolved SHAs, so rebases
  keep their marks. hg, jj, stdin, compare and all-files have no sessions.
- **Preload goes through `loadReviewedFingerprints`.** Marks are seeded with `ResetReviewed` before the
  first `loadFiles`; binary/placeholder rows are still rejected by `ReviewFingerprintStable`.
- **Fingerprint version is stored** (`fingerprint_version`); a mismatch is reported as "could not be
  verified" instead of looking like a content change.
- **Cost.** The startup cost is the existing fingerprint fetch for previously reviewed paths
  (4 workers), only when a session with marks exists for the branch. `--no-session` opts out.
- Open decisions: `Q` does not touch the session's reviewed marks (marks are not annotations); an emptied session is
  rewritten as empty so removed marks cannot resurrect.
