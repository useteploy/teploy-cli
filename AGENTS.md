# Working on teploy-cli

Instructions for coding agents. `CLAUDE.md` carries the detail on how this
codebase is built and tested; this file carries the routing rule for defects
that are not ours.

## Which layer owns a defect

The CLI is the bottom of the Teploy stack. It has no Neutron or Nucleus
dependency — check `go.mod` if you doubt it — so there is no framework to hand a
bug to, and almost everything found here is genuinely ours to fix.

That cuts the other way too: **teploy-dash and teploy-ship both shell out to
this binary**, so a defect either of them reports is frequently ours, and a
workaround landed in one of them leaves the other still broken and the two
disagreeing about what a deploy does. When a report arrives from up there, fix
it here rather than accepting the workaround as the fix.

Where a defect turns out to belong to a repository you are not in, say so in the
commit message and open the report there. A workaround with no report is how the
same bug gets paid for twice.

**Never cut a Neutron or Nucleus release from a Teploy session**, on the rare
occasion something reaches that far. An upstream fix is a standalone change in
the upstream repo plus a written handover.
