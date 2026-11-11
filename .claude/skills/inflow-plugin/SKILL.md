---
name: inflow-plugin
description: Build an Inflowenger Plugin node with the Go go-plugin-sdk (sdkv1). Use when the user asks to create, scaffold, or extend an inflow/Inflowenger plugin — adding an action, parsing request input, reporting progress, reading/writing flow context, building the action's UI form, wiring settings, or reacting to a stopped/timed-out process. Not for extrinsic nodes (those belong to inflow-fusion).
---

# Building an Inflowenger Plugin node

Instructions for writing a plugin with `github.com/Inflowenger/go-plugin-sdk`
(`sdkv1`), imported as a **library**. A plugin is a long-running Go process that
appears as a node on the Inflowenger workflow canvas and is called by the Fractal
runtime over NATS.

Fuller reference lives in the SDK's GitHub repo (this skill is meant to be copied
into a consuming plugin project, so links point there rather than at local paths):
the human cookbook at
[`cookbook.md`](https://github.com/Inflowenger/go-plugin-sdk/blob/main/cookbook.md) and
concept docs under
[`docs/`](https://github.com/Inflowenger/go-plugin-sdk/tree/main/docs). Read those
for detail — this file is the operational checklist. Verify the current API against
the installed `go-plugin-sdk/sdkv1` package before relying on any signature; do not
invent methods.

The **[plugin catalog](https://github.com/Inflowenger/plugin-catalog)** is the other live resource worth reading: it
carries the current developer knowledge base —
[`concepts.md`](https://github.com/Inflowenger/plugin-catalog/blob/main/docs/concepts.md) (the mental model),
[`build-a-plugin.md`](https://github.com/Inflowenger/plugin-catalog/blob/main/docs/build-a-plugin.md) (build from zero),
[`run-a-plugin.md`](https://github.com/Inflowenger/plugin-catalog/blob/main/docs/run-a-plugin.md),
[`dependent-fields.md`](https://github.com/Inflowenger/plugin-catalog/blob/main/docs/dependent-fields.md),
[`sdks.md`](https://github.com/Inflowenger/plugin-catalog/blob/main/docs/sdks.md) (the SDK matrix) and
[`publishing.md`](https://github.com/Inflowenger/plugin-catalog/blob/main/docs/publishing.md) — plus
[`plugins/`](https://github.com/Inflowenger/plugin-catalog/tree/main/plugins), an entry per shipped plugin pointing at its
real source. Those are the best worked examples available: in Go, [jira-plugin](https://github.com/mehdi-shokohi/jira-plugin) (14 actions),
[postgres-plugin](https://github.com/FloMorphic/postgres-plugin),
[mongodb-plugin](https://github.com/FloMorphic/mongodb-plugin) and
[google-office-oc-plugin](https://github.com/FloMorphic/google-office-oc-plugin)
(30 actions). Prefer their
patterns over inventing your own, and check
[`plugins/index.json`](https://github.com/Inflowenger/plugin-catalog/blob/main/plugins/index.json) for the machine-readable
list.

## When to use

Use this when the task involves creating or modifying an inflow **plugin** node:
scaffolding a plugin, adding/editing an `Action`, decoding request bodies, progress
reporting, flow-context read/write, or action/settings forms.

Do **not** use this for **extrinsic** nodes (internal service calls) — those are
registered via `inflow-fusion`, a different repo, and are out of scope here.

## The non-negotiable rules (get these right)

1. **`main` must block after `Start()`.** `p.Start()` only wires NATS subscriptions
   and returns immediately. End `main` with `select {}` (or equivalent) or the
   process exits and the plugin dies.
2. **Every handler ends in exactly one `job.Done(...)` or `job.DoneWithError(...)`
   on every path.** On each error branch call `job.DoneWithError(err.Error())` **and
   `return`**. Never finish twice, never finish zero times.
3. **Decode input with `sdkv1.CastRequestTo[T](job.Req.Data)`**, which unwraps the
   `{ "_registry", "body" }` envelope → `req.Body` (type `T`) + `req.Registry`
   (`map[string]any`). JSON numbers arrive as `float64`; convert before use.
4. **Keep each action's `Jsonschema` in sync with its input struct's JSON tags** —
   the form defines the shape delivered as `body`.
5. **Provisioning is a prerequisite, not code.** The plugin must be defined in a
   space (a NATS account in Infra) to get `PLUGIN_ID`, `INFRA_CRED` (base64), and
   `INFRA_URL`. If these are missing, tell the user to provision first; don't
   fabricate credentials.

## Procedure

1. **Confirm prerequisites**: `PLUGIN_ID`, `INFRA_CRED`, `INFRA_URL` (usually in a
   dotenv like `.env.inflow`), and that Infra + a Fractal are running. Add the dep
   with `go get github.com/Inflowenger/go-plugin-sdk@latest`.
2. **Scaffold `main`** (a `main` package, not a test):
   ```go
   p, err := sdkv1.NewPlugin(sdkv1.WithDotEnv(".env.inflow")) // or WithInfraConnection + WithPluginId
   // handle err
   p.Intro(sdkv1.PluginIntro{Name: "MY.PLUGIN", Author: "…", Version: "v0.0.1"})
   p.AddAction(sdkv1.Action{Method: "do.thing", Title: "…", Form: form, RequestHandler: handler})
   if err := p.Start(); err != nil { /* handle */ }
   select {}
   ```
3. **Write each `RequestHandler(job sdkv1.Job)`** using only these verified `Job`
   operations:
   - `sdkv1.CastRequestTo[T](job.Req.Data)` — typed input (rule 3).
   - `job.Progress(pct, sdkv1.Frame{Title, Content})` — advisory, 0–100; does not finish.
   - `job.Done(map[string]any, key ...string)` — success + output (finishes).
   - `job.DoneWithError(string)` — failure (finishes); the reason goes on the
     command's own `error` field, never into `details`.
   - `job.DoneWithErrorData(string, map[string]any, key ...string)` — failure that
     still reports/commits data. `job.DoneWithErrorCode(int, string, map[string]any, key ...string)`
     adds the plugin's own error number (pass `0` when it has none).
   - `job.CmdGetCurrentScope()` / `job.CmdGetScope("$.path")` — read context; both
     return `any`, type-assert to `[]byte`.
   - `job.CmdSetOnPath("$.path", map[string]any{...})` — write into flow context.
   - `job.CmdNextFilter([]string{...})` — fire only the outbound branch(es) with
     these tags (the runtime counterpart of the action's declared
     `Outbound: []sdkv1.OutboundPort{...}`).
   - Any path above may start at `$this`, inflow's non-standard root for the
     location this run was handed (the slice the node's `scope` selected), e.g.
     `job.CmdGetScope("$this.customer.id")`. Prefer it over a hardcoded index
     when the node's scope can select more than one location.
   - `job.CmdSvcCall(action, data, opData)` — ask the extrinsics service to run
     `action` (e.g. `add.db.record`) through the runtime (feeder pattern);
     action is required and is not a registered extrinsics subject. The call is
     origin-tagged `plugin:<node title>`; the service may refuse it if plugin
     calls aren't granted.
4. **Add forms** when the node needs configuration:
   `sdkv1.FormBuilder{Jsonschema: <JSON Schema>, Jsonui: <UI Schema>}` (JSON Forms).
   For plugin-level onboarding/config use `p.RequiredParams(&sdkv1.Settings{...})`
   with a `SubmitHandler`. An optional Markdown manual for the plugin's page goes
   on `sdkv1.PluginIntro{Manual: ...}`; a fenced ` ```inflow-meta ` block naming a
   meta method becomes a Run button.
5. **Make dependent fields work.** Any field a user cannot type from memory (an
   `accountId`, a project key, an id valid only inside another selection) must not
   ship as a bare text input. Register a **meta function** and put a button on the
   control that calls it:
   ```go
   p.AddMeta(sdkv1.Meta{                    // before Start()
       Method:         "my.meta.users.resolve",
       RequestHandler: func(r sdkv1.Request) any { /* … */ },
   })
   ```
   ```jsonc
   // in Jsonui, on the control
   "x-inflow-ui": {
     "action": { "name": "pluginFn", "fn": "my.meta.users.resolve" },
     "button": { "position": "append", "label": "Find user" }
   }
   ```
   Four rules, each of which fails **silently** if broken:
   - `action.name` is always the literal `pluginFn`. It is the host's only action.
   - The request arrives **flat** (form fields + `settings` + `value` at the top
     level), *not* in the `{_registry, body}` action envelope — so
     `CastRequestTo` yields a zero struct here. Decode tolerantly, trying `body`
     first and then the raw bytes.
   - Return the **patch object** (`map[string]any{"assignee": "5b10…"}`), not
     `sdkv1.Response` — the latter's `{data,error}` envelope gets patched in as
     fields called `data` and `error`. Patch keys are absolute leaf paths.
   - There is no error channel in the transport. Say what happened under the
     reserved `x-inflow-notif` key, which the host lifts out of the answer and
     shows — or the button appears to do nothing. `formkit` builds it:
     ```go
     return formkit.Success("Issue: %s", key).Patch(map[string]any{"issueKey": key})
     return formkit.Failure("cannot reach %s: %s", site, err).Patch(nil) // message only
     ```
     `formkit.Info` / `Success` / `Warning` / `Failure` / `Help` are the five
     severities; `.About(field)` re-aims a message, `.Patch(nil)` is a valid
     answer on its own (a connection test writes nothing). The message defaults
     to the field the button targets; a field some *other* control fills needs
     `.Inline()` on it so the host has somewhere to put it. Do **not** add a
     readonly `lookupStatus`-style property for this — a message is not form
     data, and one declared as a field is sent to the service and stored with
     the rest.
   Full contract: `docs/form-builder.md` and the catalog's `dependent-fields.md`
   (that doc still describes the pre-`x-inflow-notif` status-field workaround;
   the notification channel above supersedes it).
6. **Only if in-flight work must stop with the process**, compose the `jobstop`
   capability onto that action and the signal port, before `Start()`:
   ```go
   var stops jobstop.Registry          // github.com/Inflowenger/go-plugin-sdk/jobstop
   p.OnSignal(stops.OnSignal)          // cancels the job a Canceled() signal names
   p.AddAction(sdkv1.Action{
       Method: "run",
       Middleware: sdkv1.Use(stops.Middleware), // this action only
       RequestHandler: func(job sdkv1.Job) {
           ctx := job.Context()        // pass ctx down; on ctx.Err() != nil just return (do not Done)
       },
   })
   ```
   Use it; do not hand-write a `sync.Map` of cancel funcs. Without
   `p.OnSignal(stops.OnSignal)` no stop arrives (`Start` logs "Signals not
   subscribed"). Middleware runs before the runtime knows the jobId, so no stop
   is ever lost. This is **optional and
   not the default**: a job without it deliberately keeps running after a stop,
   because a later run of the node may build on its progress (the previous
   `jobId` comes back in `_registry`). Add it only for a stream to close, an
   upstream call to abort, a lock to release. Other per-job capabilities — a
   long-running job kept in your own map, a trace around the handler — are
   middleware functions of your own
   (`func(ctx, job) (context.Context, error)`, run in order before the job is
   accepted; register there, clean up with `context.AfterFunc(ctx, …)`; an
   error rejects), listed with `sdkv1.Use(…)` on an action or `p.Use(…)` on
   every action; several signal
   handlers compose with `sdkv1.ChainSignals(…)` — chain
   `sdkv1.LogSignals("<plugin>")` to keep a log line per arriving signal, which
   registering a handler of your own otherwise replaces (`jobstop` logs the
   cancel itself). Once a stop cancels `ctx`, the runtime no longer
   answers that job's commands — do not try to `Done` it.
7. **If the plugin fronts a service that names work itself** (a Joern HTTP
   server answering `POST /query` with a `queryId`, a render farm, a scan),
   **do not keep a `map[jobId]upstreamId`.** Register upstream in a middleware
   function and bind the id it returns as the job's own — middleware runs before
   the job is accepted, so that id is what the runtime is told:
   ```go
   func registerQuery(ctx context.Context, job sdkv1.Job) (context.Context, error) {
       in, err := sdkv1.CastRequestTo[QueryInput](job.Req.Data)
       if err != nil {
           return nil, err // an error here rejects the request: the node never ran
       }
       if prev, ok := in.Registry["jobId"].(string); ok && joern.Alive(ctx, prev) {
           return sdkv1.WithJobIDContext(ctx, prev), nil // reattach, don't duplicate
       }
       queryId, err := joern.Register(ctx, in.Body.Project, in.Body.Query)
       if err != nil {
           return nil, err
       }
       return sdkv1.WithJobIDContext(ctx, queryId), nil
   }
   // namer FIRST — stops.Middleware files the job under job.JobId as of when it runs
   Middleware: sdkv1.Use(registerQuery, stops.Middleware),
   ```
   One name then serves both systems: `job.JobId`, every command subject, the
   stop signal's `jobId`, and the next run's `_registry["jobId"]`. The abort
   needs no local state — `joern.Cancel(ctx, sig.JobId)` in the signal handler,
   so any replica that hears the stop can forward it. Rules: the namer comes
   **before** anything keyed on the jobId; validate the adopted id — at least
   **10 characters** (fractal-core rejects a shorter one: `init failed. invalid
   job ID`), a usable NATS subject token (no `.`, space, `*`, `>`), unique
   plugin-wide; keep
   the registration call fast and timeout-bounded (the runtime is waiting for
   the jobId); reject from middleware when there is nothing to report, accept and
   `job.DoneWithError` when the flow should see a failed node. If the service
   accepts a client-supplied id instead, do the mirror image — keep the SDK's
   uuid and send `job.JobId` upstream. Full treatment:
   [`docs/external-job-identity.md`](https://github.com/Inflowenger/go-plugin-sdk/blob/main/docs/external-job-identity.md).
8. **If the external work takes longer than a flow run** (hours: a CPG build, a
   render, a nightly scan), **do not block the job.** The runtime waits 15s for
   the `jobId`, abandons a job that sends no command for the node's `idle_min`,
   and ends the run at `ExecuteTimeOut`. Report state and end instead — the
   plugin is an async function, the flow an observer:
   ```go
   // accept stage: the only inputs are body and _registry (the node's memory of
   // its own previous run; job commands need a jobId, which this decides)
   if prev, ok := in.Registry["jobId"].(string); ok && prev != "" && joern.Has(ctx, prev) {
       return sdkv1.WithJobIDContext(ctx, prev), nil // observe; start nothing
   }
   // handler:
   case !status.Done:
       job.CmdNextFilter([]string{"pending"})   // a SUCCESSFUL "not yet" — never DoneWithError
       job.Done(map[string]any{"state": "running", "percent": status.Percent}, "joern")
   default:
       job.CmdNextFilter([]string{"ready"})
       job.Done(map[string]any{"result": status.Result}, "joern") // commit, with a key
   ```
   Declare the ports (`Outbound: []sdkv1.OutboundPort{{Title: "Still running",
   Tags: []string{"pending"}}, …}`) so the canvas shows them, and route
   `_exception` + `DoneWithErrorData` when the work failed upstream. Rules: the
   state snapshot must be **committed** (`Done(data, key)` — a bare `Done`
   commits nothing); `_registry` is per **call site** and lives in the context
   document, so re-entry must be over the **same context** (a Continue
   After/delay node on the pending branch, a schedule, or a loop edge) and the
   loop needs a floor (attempt counter, or a `reqAt` age limit); `doneAt` /
   `conclusion` describe the *run*, not the work — a pending run ends `done`;
   handle the **stale handle** (service forgot the id → start fresh); and add
   **no** `stops.Middleware` to an observer action, because outliving the flow is
   the point. Full treatment:
   [`docs/detached-work.md`](https://github.com/Inflowenger/go-plugin-sdk/blob/main/docs/detached-work.md).
9. **Build & run**: `go build ./...`, then `go run .`; the SDK logs each subscribed
   subject on startup. Verify by adding the node to a flow and running it.

## Known limitations to respect

- A form action **cannot mutate the schema** — answers are patched into form
  *data* only, so you cannot populate a `<select>`'s `enum` at runtime. Model a
  picker as free text + a resolve button (scalar fields) or as an array field
  filled with a returned list (multi-value). Do not invent an options-loading API.
- Nothing fires automatically: no on-change, no debounce, no type-ahead. The user
  clicks. Label the button with what it does.
- If asked for anything about **extrinsic** nodes, redirect to `inflow-fusion`; it
  is not part of this SDK.

## Verify before finishing

- `go build ./...` passes.
- `main` blocks after `Start()`.
- Each action: unique `Method`, a `RequestHandler`, exactly one finish per path.
- Each `Jsonschema` matches its input struct.
- Every meta function is registered before `Start()`, decodes a flat body, and
  returns a patch (not `sdkv1.Response`) if a form button calls it.
- No fabricated SDK methods — every `Job`/`Plugin` call exists in `sdkv1/`.
