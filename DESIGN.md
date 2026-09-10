# Jarvis MVP

Jarvis is a Go daemon and a static browser client. The daemon observes existing
Mux terminals, serves authenticated local controls, and connects a GPT-Live
conversation to managed Astra reasoning. Mux owns PTYs; Jarvis never reads their
master descriptors or starts a replacement runtime for an existing coding session.

## Ownership

| Owner | State and behavior |
|---|---|
| muxd | PTY reads, rendered viewport, interactive client, serialized input, session incarnation |
| jarvisd | Local session view, short-lived inspection tokens, watches, command receipts, provider connection |
| OpenAI | Voice interaction and configured Responses delegation |
| Browser | Microphone, speaker, selected terminal and the user's unsent message |

One process contains HTTP handlers, the Mux observer, a Live reader and a bounded
tool queue. Network and tool work run outside the mutex protecting the current
session map. SQLite persists watches and receipts. Runtime observation can be
rebuilt; the database is not a second scheduler or a coding-agent transcript store.

## Terminal contract

Mux protocol v9 appends Inspect, Observe and Input variants. Existing v7/v8
requests retain their byte layout and remain accepted. Inspection and observation
never call attach or resize. A watch channel holds the newest change notification;
slow observers receive a fresh viewport instead of delaying the PTY reader.

Snapshots include a random incarnation, output revision, input revision, foreground
PGID, PID, dimensions, cursor and current screen. Incarnations change on daemon
handoff as well as pane recreation. This deliberately requires fresh inspection
after an upgrade; it does not change existing interactive reconnection behavior.

Both interactive and automation writes use one per-terminal mutex. Automation
checks the snapshot's incarnation, revisions and foreground process group before
writing. Its input revision advances before the first byte, making a partially
failed request ineligible for replay. These are observation checks, not an atomic
transaction with the foreground application: it may change after the check.

Jarvis issues one-use inspection tokens that expire after a minute. Literal text
contains no control characters; Enter is an explicit submit flag. The HTTP API
also allows explicit Enter, Escape and interrupt keys. Voice tools initially expose
single-line text submission only. A terminal's shared composer remains an application
boundary: the model is instructed to refuse ambiguous targets and existing drafts;
PTY inspection does not provide a universal semantic guarantee. Native Codex
steering is a future integration, not silently simulated through a new runtime.

Every write gets a durable request ID before dispatch. Duplicate IDs return their
recorded result. A timeout or daemon crash leaves delivery unknown; it is never
automatically retried. A written receipt proves PTY delivery, not task acceptance,
approval, completion or success. Commands outside Mux's control API are absent.

## Voice contract

The browser makes an SDP offer; Spark creates the Live session and attaches its
sideband before returning the answer. The project key stays on Spark. Browser
code never executes function calls. The sideband collects complete function items
from nested Responses events, executes each call once, submits every result, and
then continues Responses. Empty lifecycle output arrays are not treated as proof
that no function calls occurred.

Voice runs only after a user starts a conversation. A browser heartbeat keeps the
session alive; after 45 seconds without one, Jarvis attempts a graceful close.
Closing waits for the provider's final event, with a bounded fallback that reports
unconfirmed finalization. Microphone mute, ending voice and stopping terminal work
are distinct. A provider disconnection ends that voice session; reconnection starts
a fresh one and does not replay terminal writes.

Only explicitly followed terminals generate notices. Mux's idle/blocked classifier
is labeled heuristic. A pane exit is not a task-success signal. Watching a pane is
bound to its incarnation; replacement removes the watch instead of following an
unrelated process under a reused name. Resource inspection uses existing /proc
files; per-pane cgroups, process killing and resource limits are outside this MVP.

## Deployment and access

The Jarvis flake owns the package and NixOS module; Git/nix imports it and supplies
the Mux package, user and runtime secret paths. The service listens on loopback at
19743. Authentication requires a random access token stored with mode 0600 in its
private state directory. The browser gets an HTTP-only, SameSite=Strict cookie.
Host and Origin checks prevent a foreign page from driving the local API.

Use localhost directly or an SSH tunnel for the MVP. A phone needs a separately
configured HTTPS endpoint; background iOS audio is not established by this browser
implementation. OpenAI voice is optional at startup so local controls remain useful
while project access is provisioned. API keys never enter the Nix store or source.

## Acceptance

1. A real disposable PTY stays attached and keeps its dimensions through inspect,
   observation and input. Stale, repeated and replacement-target writes fail.
2. Authenticated Jarvis HTTP calls drive that PTY; unauthenticated and cross-origin
   requests cannot. A simulated transport loss cannot produce a duplicate write.
3. A mock Live server verifies session configuration, nested function events,
   duplicate suppression, result submission and explicit continuation.
4. Package build, NixOS option evaluation and a browser check use the same source.
5. Actual project access, microphone playback, interruption and useful proactive
   speech require a real-key/device test and must be reported separately.
