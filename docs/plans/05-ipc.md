# Phase 5 — Agent IPC (Mailbox)

Inter-instance communication for parallel agents. Two primitives: `send`
(write a message into an instance's mailbox) and `recv` (read and remove
messages). No daemon, no locking — built on filesystem operations.

---

## 5.1 Mailbox directory layout

```
.plax/mail/
  i1/                         # one directory per instance
    1743782400_abc123.json    # message files (timestamp_nanoid.json)
    1743782410_def456.json
  i2/
    1743782399_ghi789.json
```

The mailbox directory is created when `plax up` runs and removed on
`plax down`. It lives alongside the worktree (under `.plax/`).

## 5.2 Message format

Each message is a JSON file:

```json
{
  "from": "i2",
  "subject": "review request",
  "body": "Can you look at PR #42 when you get a chance?",
  "timestamp": "2026-08-03T10:00:00Z"
}
```

The format is intentionally minimal. The design doc says "prose" is the
expected first content. Structured fields (`subject`) are optional — every
field except `body` can be empty.

## 5.3 `plax send <name> [--subject S] [--from F] -- <body>`

1. Verify instance `<name>` exists in registry (any state — suspended
   instances can receive mail)
2. Write a new file in `.plax/mail/<name>/` with nanosecond-precision
   timestamp + random suffix for uniqueness
3. Timestamp is set by the sender (wall clock), not the filesystem mtime
4. `--from` defaults to the caller's instance (looked up from git branch or
   env) or an empty string if undetermined
5. `--subject` is optional

Since writes are `O_CREATE` (not `O_WRONLY`), two concurrent `send`
operations to the same instance never collide — they create different files.

## 5.4 `plax recv <name> [--all | --count N]`

1. List `.plax/mail/<name>/` sorted by filename (timestamp order)
2. Read and print the oldest N messages (default 1)
3. Remove the files after reading
4. `--all` reads and removes every message

Output:

```
From: i2
Subject: review request
---
Can you look at PR #42 when you get a chance?
---
1 message(s) remaining
```

`--json` flag prints the raw messages as a JSON array.

## 5.5 `plax ls` mailbox indicator

`plax ls` shows an unread count column:

```
NAME   STATE     BRANCH            MAIL  PORTS
i1     running   plax/feature-auth  2    3001 5433 6380 3031
i2     running   plax/feature-fix   0    3002 5434 6381 3032
```

Counted by listing `.plax/mail/<name>/` — cheap since it's a directory read
(< 1ms even for hundreds of messages).

## 5.6 `plax attach` notification

When attaching to an instance, print a one-line notice if mail is waiting:

```
📫 3 unread messages — run 'plax recv <name>' to read
```

Same check as the `ls` indicator: count files in the mailbox directory.

## 5.7 Security notes

- No authentication. Any process on the machine that can write to `.plax/mail/`
  can send messages. That is by design — Plax operates on trust-within-machine.
- No encryption. Messages are plain JSON. If the mailbox needs protection, the
  whole `.plax/` directory lives on an encrypted filesystem.

---

## Deliverables

- [ ] Mailbox directory create/remove on instance up/down
- [ ] `plax send <name>` — write message file
- [ ] `plax recv <name>` — read and remove messages
- [ ] `plax ls` — unread count column
- [ ] `plax attach` — mailbox notification
- [ ] `--json` output for both send and recv
