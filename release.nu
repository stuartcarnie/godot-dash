#!/usr/bin/env nu

# Archive a .docset bundle and publish it as a GitHub release.
#
#   step 1 (tar): pack the docset into <Name>.tgz, excluding macOS cruft
#   step 2 (git): create and push an annotated tag
#   step 3 (gh):  create the release and upload the archive
#
# Version strings follow semver with a pre-release suffix for nightlies,
# e.g. 4.8.0-nightly.20260823 (tag v4.8.0-nightly.20260823). Nightlies
# are marked as pre-releases automatically.
def main [
  version: string      # semver version, e.g. 4.8.0-nightly.20260823 or 4.7.1
  --docset: path = Godot.docset   # docset bundle to archive
  --notes: string      # release notes; defaults to a generic description
  --dry-run (-n)       # build the archive but do not tag or publish
] {
  if not ($docset | path exists) {
    error make { msg: $"docset not found: ($docset)" }
  }
  if ($version | str starts-with "v") {
    error make { msg: $"version must not start with 'v': ($version)" }
  }

  let tag = $"v($version)"
  let nightly = ($version | str contains "-nightly.")
  let name = ($docset | path parse | get stem)
  let archive = $"($name).tgz"
  let today = (date now | format date "%Y-%m-%d")

  # Step 1 — archive. COPYFILE_DISABLE stops bsdtar adding ._* resource forks.
  print $"[tar] ($docset) -> ($archive)"
  ^find $docset -name .DS_Store -delete
  with-env { COPYFILE_DISABLE: "1" } {
    ^tar --exclude=.DS_Store -czf $archive $docset
  }
  print $"[tar] (ls $archive | get 0.size)"

  let title = if $nightly {
    $"($name) ($version | str replace -r '\.0-nightly\..*' '') nightly \(($today)\)"
  } else {
    $"($name) ($version)"
  }
  let notes = if $notes != null { $notes } else if $nightly {
    $"Dash docset built from the godot-docs `master` branch offline HTML archive, downloaded ($today).

This is a nightly build of unreleased documentation and may describe features not available in stable Godot releases.

Install: download `($archive)`, extract, and open `($docset)` with Dash."
  } else {
    $"Dash docset for Godot ($version).

Install: download `($archive)`, extract, and open `($docset)` with Dash."
  }

  if $dry_run {
    print $"[tag] dry-run: would create and push ($tag)"
    print $"[gh]  dry-run: would create release '($title)' (if $nightly { '(pre-release)' } else { '' })"
    return
  }

  # Step 2 — tag.
  print $"[tag] ($tag)"
  ^git tag -a $tag -m $"($name) docset ($version), built ($today)"
  ^git push -q origin $tag

  # Step 3 — release.
  mut args = ["release" "create" $tag $archive "--title" $title "--notes" $notes]
  if $nightly { $args = ($args | append "--prerelease") }
  print $"[gh]  ($title)"
  ^gh ...$args
}
