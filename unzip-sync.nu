#!/usr/bin/env nu

# Extract a zip archive into a target directory with rsync semantics:
# only changed files are written, and files no longer present in the
# archive are removed from the target. A clean staging dir is used so the
# whole archive is compared against the target in a single rsync pass.
def main [
  archive: path        # the .zip archive to extract
  target: path         # the directory to mirror the archive into
  --dry-run (-n)       # show what would change without touching the target
  --checksum (-c)      # compare by checksum instead of size+mtime (slower, exact)
] {
  if not ($archive | path exists) {
    error make { msg: $"archive not found: ($archive)" }
  }

  # Stage into a temp dir (honours $TMPDIR), then rsync --delete into target.
  let tmp = (mktemp -d)
  try {
    unzip -q -o $archive -d $tmp

    mut flags = ["-a" "--delete"]
    if $checksum { $flags = ($flags | append "-c") }
    if $dry_run  { $flags = ($flags | append ["--dry-run" "--itemize-changes"]) }

    # Trailing slashes: sync the *contents* of tmp into target.
    ^rsync ...$flags $"($tmp)/" $"($target)/"
  } catch { |e|
    rm -rf $tmp
    error make { msg: $"sync failed: ($e.msg)" }
  }
  rm -rf $tmp
}
