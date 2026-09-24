# teploy quickstart (executable)

The C09 acceptance line: *a new user deploys the maintained fixture from
documentation without undocumented repair.* This directory is that
documentation, and it runs.

```
make quickstart        # from the repo root
```

`run.sh` deploys `app/` (the maintained CLI fixture app — busybox httpd
serving `index.html` and a 200 `/health`, with a `teploy.yml`; X05's CLI
fixture) to the **local colima VM's own SSH endpoint**, then:

1. builds the CLI from this checkout,
2. bootstraps the target once (`/deployments` directory; the only
   target-side setup, via the VM's passwordless sudo),
3. deploys version `qs1` — build-on-target, tcp-gated start, host
   ingress on `127.0.0.1:18080`,
4. verifies the app answers with the v1 content,
5. redeploys as `qs2` with changed content and verifies the switch,
6. runs `teploy health`, and checks `teploy status` names the image by
   tag (`quickstart-build-qs2`), not the bare image ID,
7. `teploy rollback` and verifies v1 is served again,
8. deploys `qs3`, a container that runs but never listens, and verifies
   the tcp readiness gate REFUSES it (docker-proxy accepts on the port
   either way) while v1 keeps serving,
9. removes everything it created (app, containers, images, its own
   known_hosts lines).

Requirements: docker CLI, a **running** colima VM (the script never
starts one — `colima start` yourself), ssh/ssh-keyscan/curl/python3.
When any is missing the script prints `SKIP: ...` and exits 0 — a
skipped quickstart is not a failed one.

No remote server, no domain, no DNS: host ingress publishes a plain port
on the target, which is exactly what makes the loop runnable on a
laptop. The deploy path exercised is the real one — same config loader,
same build/health/commit machinery as production.
