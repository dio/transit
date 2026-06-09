For --local what I want:

1. orange server --local

This by default loads orange.yaml as the bootstrap (purge existing, unless we do --no-purge) for a workspace config, while organization and project will be default to orange.io/proj1 (org/proj). When we load examples/orange/orange.yaml, we can see we will have "demo" workspace (as derived from key).

This automatically add "dio" to demo workspace (dio as the demo workspace member, has an API Key that has the right scope). So dio, within the demo workspace can issue tokens, and manipulate some routing and rls policies later. This automatically onboard the egress, where admin can initiate download-bundle for that egress in a workspace.

This will create an admin user at orange.io as the "root" admin

All of the above actions will be done in REPL mode

There will be still `orange admin` and `orange` (for user) CLIs, but this will simplify local dev/debugging experience.

2. ORANGE_SERVER_URL=http://<SERVER> orange egress --local

This will by default connct the egress components: envoy(with liborange.so) and rls to connect to ORANGE_SERVER_URL as the config/secret server. This will act just like when we run orange egress --local without: ORANGE_SERVER_URL=http://<SERVER>

failing e2e/routing and responsesws tests