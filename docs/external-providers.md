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
otherwise it is killed two seconds later. Messages are JSON-RPC 2.0, one JSON object
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

### Field rules

- Omitted and `null` fields both mean the zero value (empty string, `false`,
  zero time). dcal sets `accountId` on calendars and `calendarId` on events
  and tasks itself, so providers can leave them out.
- Required fields: `remoteId` and `name` on calendars; `uid`, `summary`,
  `start` and `end` on events; `uid` and `summary` on tasks. An `upsert`
  change without its `event` or `task` is ignored.
- Calendars are matched across syncs by `remoteId`. Set
  `supportedComponents: ["VTODO"]` for task lists.
- Events and tasks are matched by `uid`. Use an identifier that survives
  edits on the provider side.
- `allDay` items use UTC midnight of their date. Timed items are RFC 3339
  instants with an offset; set `startTimeZone`, `endTimeZone` or
  `dueTimeZone` to an IANA name when the source has one.

### Sync

- `token` is passed back on the next sync, so incremental providers can
  return only what changed. A simple provider can return every item each time
  with `fullSnapshot: true` and ignore the token.
- A full snapshot may span several responses: return `more: true` and dcal
  calls `calendar.sync` again with the new token. Items are pruned only after
  the last page, and nothing is pruned if any page fails. Because a full
  snapshot is authoritative, return an error rather than an empty list when
  the source fails or answers with unexpected data.

### Writes

- A calendar is writable when the plugin can write at least one component
  type it holds. Write methods are only called for components whose
  capability has `write: true`.
- Write results are stored as returned, so return the item as the source now
  has it (for example, a repeating task that moved to its next date).
- dcal does not retry failed writes. A provider that cannot tell whether a
  write landed should return an error; the next sync shows the real state.

### Errors

- Return error code `-32001` when credentials are rejected. dcal then marks
  the account as needing re-authorization. Other failures can use any code.
- `notices` are the soft sync warnings defined in
  `core/internal/calendar/provider.go`, such as `items_skipped`.
- Each call has a timeout (60 seconds by default). A provider that times out,
  stops reading, or closes its output is killed together with its process
  group, so helper processes it started are stopped too.
