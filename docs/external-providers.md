# External providers

An `external` account is backed by a separate program, so calendar sources
dcal does not support natively (task managers, org-mode, internal tools) can
be added without changing dcal. The program can be written in any language.

## Adding an account

```sh
dcal account add external work-tasks --name "Work tasks" \
  --env API_URL=https://tasks.example.com -- /usr/bin/my-provider --flag
```

Re-running the command with the same name updates the account.

### Declarative accounts

The daemon reads `$XDG_CONFIG_HOME/dankcal/external-accounts.json` at
startup and makes the managed accounts match it. Declared accounts are created
or updated. Managed accounts that are no longer listed are removed. Accounts
added with `dcal account add` are never touched.

```json
{
  "accounts": [
    {
      "name": "work-tasks",
      "displayName": "Work tasks",
      "command": ["/usr/bin/my-provider", "--flag"],
      "env": { "API_URL": "https://tasks.example.com" }
    }
  ]
}
```

The Home Manager module generates this file from
`programs.dank-calendar.externalAccounts`.

Do not put secrets in `env` or `command`, because both are stored in the dcal
database. Pass the path of a secret file instead, and have the provider read
it.

## Protocol

dcal starts the command whenever it needs the provider (each sync pass and
each write) and closes its stdin when done. The provider should exit on EOF;
it is killed two seconds later. Messages are JSON-RPC 2.0, one JSON object
per line on stdin and stdout. Anything written to stderr goes to the daemon
log. Messages without an `id` are ignored. Each call times out after 60
seconds by default.

Payloads use dcal's own JSON shapes for calendars, events and tasks (see
`core/internal/calendar/types.go`). Times are RFC 3339.

| Method | Params | Result |
| --- | --- | --- |
| `initialize` | `{protocolVersion: 1, account}` | `{protocolVersion: 1, capabilities: {events: {write}, tasks: {write}}, notices?}` |
| `calendars.list` | `{}` | `{calendars: [Calendar]}` |
| `calendar.sync` | `{calendar, token?}` | `{token?, changes: [EventChange], taskChanges?: [TaskChange], more, fullSnapshot, retryAfterSeconds?, color?, notices?}` |
| `events.list` | `{calendar, start?, end?, limit?, query?}` | `{events: [Event]}` |
| `events.create`, `events.update` | `{calendar, event}` | `{event}` |
| `events.delete` | `{calendar, event}` | `null` |
| `tasks.create`, `tasks.update` | `{calendar, task}` | `{task}` |
| `tasks.delete` | `{calendar, task}` | `null` |

- Calendars are matched across syncs by `remoteId`. Set
  `supportedComponents: ["VTODO"]` for task lists.
- Events and tasks are matched by `uid`. A simple provider can return every
  item each time with `fullSnapshot: true`. dcal then removes items that are
  missing. `token` is passed back on the next sync, so incremental providers
  can return only what changed.
- A calendar is read-only unless the capability for its component type has
  `write: true`. Write methods are only called when that capability is set.
- Return error code `-32001` when credentials are rejected. dcal then marks
  the account as needing re-authorization.
- `notices` are the soft sync warnings defined in
  `core/internal/calendar/provider.go`, such as `items_skipped`.
