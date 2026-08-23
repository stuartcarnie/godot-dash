#!/usr/bin/env nu

# Sync a directory of HTML docs into a .docset bundle's Documents folder,
# mirroring with deletion and using APFS copy-on-write clones.
#
#   step 1 (rsync): delete-only pass that prunes paths no longer in source
#   step 2 (ditto): clone every file from source (instant CoW on same volume)
#
# ditto can't delete and rsync can't clone, so each tool does the half it's
# best at. The docset's Contents/Resources/Documents subpath is appended for you.
def main [
  source: path         # source directory of extracted docs
  docset: path         # destination .docset bundle
  --dry-run (-n)       # preview the prune step; skip the clone step
] {
  if not ($source | path exists) {
    error make { msg: $"source not found: ($source)" }
  }
  if (($source | path type) != "dir") {
    error make { msg: $"source is not a directory: ($source)" }
  }
  if not ($docset | path exists) {
    error make { msg: $"docset not found: ($docset)" }
  }

  let docs = ($docset | path join Contents Resources Documents)
  mkdir $docs   # no-op if it already exists; rsync needs it present to prune

  # Step 1 — prune. --existing + --ignore-existing make rsync transfer nothing;
  # --delete still removes destination paths that are gone from the source.
  mut prune = ["-a" "--delete" "--existing" "--ignore-existing"]
  if $dry_run { $prune = ($prune | append ["--dry-run" "--itemize-changes"]) }
  print $"[prune] rsync ($source)/ -> ($docs)/"
  ^rsync ...$prune $"($source)/" $"($docs)/"

  # Step 2 — clone. ditto clones on APFS same-volume by default and replaces
  # existing destinations cleanly. No dry-run mode, so just report it.
  if $dry_run {
    print $"[clone] dry-run: would `ditto --clone ($source) ($docs)`"
  } else {
    print $"[clone] ditto --clone ($source) -> ($docs)"
    ^ditto --clone $source $docs
  }
}
