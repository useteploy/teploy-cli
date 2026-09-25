# Files sent to a remote build

Container builds upload tracked files and untracked files that Git does not
ignore. This follows `.gitignore`, `.git/info/exclude`, and the user's Git
excludes. Outside a Git worktree, all files are considered before exclusions.

Use `.teployignore` to exclude additional files. An explicit `!` pattern can
include a Git-ignored build artifact that the Dockerfile needs:

```
!/dist/
!/web/dist/
/web/dist/**/*.map
```

Exclusions take precedence over includes. Environment files, Git metadata,
node_modules, root Teploy configuration, destination overlays, and conventional
secret stores are protected and cannot be included by `!`. Keep credentials in
the deployment secret store instead of copying them into an image. Dockerfile
COPY/ADD checks report missing excluded inputs before upload and explain how to
include ordinary build artifacts.

The upload's selected paths also determine its recorded context fingerprint.
Each build attempt lives under an owner-only directory on the server; files
inside retain their source modes so unprivileged application processes can read
the files Docker copies into the image.

Static deployments also exclude protected files, but do not apply Git ignores:
static sources normally point at generated output directories.

`teploy doctor` reports older build directories that are traversable by other
users and potentially secret-bearing files under build/static release paths.
The check lists paths only and does not delete files or change permissions.
