# Protocol

The requests the agent makes to Cheflink and the answers it expects. There
are three. `cmd/fakeserver` answers them for development.

The agent only ever calls out. Nothing connects to the agent, so the venue's
router needs no configuration.

## Rules for every request

| | |
| --- | --- |
| Method | `POST` |
| Base path | `/api/print-agent/` under the address given at pairing |
| Body | JSON, `Content-Type: application/json` |
| Authentication | `Authorization: Bearer <token>`, on every request except `pair` |
| Transport | https. The agent refuses plain http unless the address is on the same machine or `allow_insecure_http` is set in its config |
| Agent identity | `User-Agent: cheflink-print-agent/<version> (<os>/<arch>)` |

An error answer is any status outside 2xx, with an optional body
`{"error": "a sentence for the log"}`.

`401` or `403` on an authenticated request means the token is unknown or
revoked. The agent stops asking for jobs, logs it once, and checks its
config file every 15 seconds until someone pairs it again.

Any other error, or no answer at all, makes the agent wait and retry: 2
seconds, then doubling up to 30.

## 1. Pair

`POST /api/print-agent/pair`

The venue's owner issues a short code on the venue's page; whoever installs
the agent types it once. The token it returns is the agent's only
credential.

Request:

```json
{"code": "123456", "name": "CAJA-PC", "version": "1.0.0"}
```

`name` is how the PC should show up on the venue's page; it defaults to the
computer's name.

Answer, `200`:

```json
{"token": "a long random string", "venue": "Restaurante El Patio"}
```

`venue` is shown to the installer, to confirm the code was for the right
place. A wrong, used or expired code answers `400` with an `error`.

## 2. Next job

`POST /api/print-agent/jobs/next`

Claims the next job waiting for this agent's venue. The agent calls it every
couple of seconds while idle, and again immediately after each job, so a
burst of tickets prints back to back. Each call also shows that the agent is
alive.

Request:

```json
{"version": "1.0.0"}
```

Answer, `200`, nothing to print:

```json
{"job": null, "poll_seconds": 2}
```

Answer, `200`, a job:

```json
{
  "job": {
    "id": "8f2c1e",
    "label": "Table 4 · Round 2",
    "printer": {"host": "192.168.1.50", "port": 9100},
    "data": "G0AbdBAbYQE..."
  },
  "poll_seconds": 2
}
```

| Field | Meaning |
| --- | --- |
| `id` | Identifies the job in the result request and in the agent's ledger. 1 to 64 characters from `A-Z a-z 0-9 _ -`. A job that is offered again keeps its `id`; that is how the agent recognises it |
| `label` | What the job is, for the agent's log. Optional |
| `printer.host` | The printer's IP address. It must be an IP literal on a private network; the agent refuses anything else and reports the job as failed |
| `printer.port` | The printer's raw TCP port, usually 9100 |
| `data` | The exact bytes to write to the printer, base64 encoded, at most 1 MB decoded. The agent does not read or change them |
| `poll_seconds` | Optional. How long the agent should wait before asking again when idle, clamped to 1 to 60. It can slow every agent down without an update |

`data` is the whole chit in the printer's command language (ESC/POS),
including the code page selection, the feed and the cut. The agent has no
layout logic, so the chit can change without touching any cashier PC.

Each answer carries at most one job. A job the agent claimed and never
reported is offered again later, and a job that has waited too long is no
longer offered: a chit that prints an hour late is worse than none.
`cmd/fakeserver` uses 30 seconds and 10 minutes.

## 3. Result

`POST /api/print-agent/jobs/{id}/result`

Request, printed:

```json
{"status": "printed"}
```

Request, not printed:

```json
{"status": "failed", "error": "cannot reach the printer at 192.168.1.50:9100: ..."}
```

`error` is at most 500 characters.

Answer: any `2xx`. `404`, `409` and `410` are also final for the agent: the
job is already settled or gone, and it will not report it again.

The agent may send the same `printed` report more than once. A job reported
as `failed` is offered again by a later `jobs/next`. The agent waits 5 seconds
after a failure, so a printer that is off or open is retried every 5 seconds
until it works or the job is no longer offered.

## What "printed" means, and who prevents duplicates

`printed` means the printer's network port accepted every byte. A raw port
gives no receipt. A printer that is out of paper may accept the job and print
it once paper is loaded, or lose it if it is switched off first.

A lost chit is worse than a duplicate, so delivery is at least once:

| Failure | What happens |
| --- | --- |
| The printer is off or unreachable | `failed` is reported; the job is offered again |
| The agent or the PC dies before printing | No result arrives; the job is offered again later |
| The job printed but the `printed` report was lost | The agent remembers it in `state.json`. It reports again before asking for more work, also after a restart. If the job is offered again meanwhile, the agent reports it and does **not** print it again |
| The agent dies after the printer accepted the job and before it saved that fact | The job prints twice. This window is a few milliseconds |

The agent forgets a reported job after 24 hours. A job offered again later
than that would print a second time.
