# Why Hermes (and not OpenClaw)?

Both are MIT-licensed, single-user personal agents with one long-lived
gateway process and all durable state under a single relocatable directory —
either *works* in a suspend/resume, per-user-pod platform. Hermes won on how
deliberately it handles being killed, which is this platform's whole premise:

1. **Restart tolerance is a designed-in feature.** Hermes flags in-flight
   sessions as `restart_interrupted`, auto-resumes them, and notifies the
   user; sessions live in SQLite committed per turn. Our suspend cycle (pod
   deleted → PVC reattached) is just a restart to Hermes. OpenClaw survives
   restarts via its state dir too, but documents no in-flight recovery.
2. **OpenClaw's rapid-restart "safe mode" is actively hostile to an automated
   suspend/resume loop**: after rapid unclean restarts it deliberately comes
   back with messaging channels suppressed — a few bad cycles and a user's
   channels silently stop reconnecting. Hermes has no such failure mode.
3. **Sleep-when-idle is an endorsed Hermes deployment pattern** (its docs
   recommend webhook mode for platforms that auto-wake suspended machines),
   and long-poll platforms like Telegram queue server-side while suspended,
   so messages catch up on resume. OpenClaw assumes 24/7 uptime — its cron
   and webhook channels silently miss events while down.
4. **The container contract is fully env-driven** (dashboard basic-auth +
   HMAC session secret, OpenAI-compatible API key, gateway bootstrap state —
   see `hermes-image.md`), which is exactly what warm pools require:
   identical pods, personalized only by whose PVC and token map to them. As
   a bonus, dashboard sessions survive suspend/resume (validated), so users
   don't get logged out when their pod is recycled.
5. **Cron survives suspension — and upstream planned for external
   schedulers.** Hermes persists each job's `next_run_at` in
   `cron/jobs.json` (on the PVC) and catch-up-fires missed jobs once on
   boot (collapsed backlog, no burst), so a suspended sandbox turns
   "missed jobs" into "late jobs" by design. It also ships external-trigger
   hooks: `hermes cron tick` (fire due jobs once and exit) and an
   experimental pluggable `CronScheduler` provider aimed explicitly at
   scale-to-zero deployments — the platform can own *when* to wake while
   Hermes owns execution (see `cron-wake-design.md`). OpenClaw's cron
   assumes 24/7 uptime: jobs silently miss while it's down.
6. **Multi-surface out of the box**: web dashboard + OpenAI-compatible API +
   20+ messaging platforms from one process, all state on one volume.

OpenClaw remains a fine self-hosted personal assistant; it's the wrong
*tenant* for a platform that kills pods on idle by design.
