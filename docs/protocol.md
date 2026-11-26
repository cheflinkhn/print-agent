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
    "printer": {"host": "192.168.1.50", "port": 9100, "mac": "AA:BB:CC:DD:EE:FF"},
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
| `printer.mac` | Optional. The printer's MAC address. With it, `printer.host` is only a hint: see [A printer whose address changes](#a-printer-whose-address-changes). Colons, dashes or no separators, in either case; the agent compares the twelve hex digits. A value that is not twelve hex digits (or is all zeros) is ignored and logged once, and the job prints by `printer.host` as if it had no `mac` |
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

Request, printed, for a job that had a `printer.mac`:

```json
{"status": "printed", "printer": {"host": "192.168.1.37"}}
```

`printer.host` is the address the job printed on. The agent sends it on
every `printed` report for a job with a usable `mac`, whether or not it is
the address the job gave, and also when it sends the report again later. A
job without a `mac` is reported without `printer`, as before.

Request, not printed:

```json
{"status": "failed", "error": "cannot reach the printer at 192.168.1.50:9100: ..."}
```

`error` is at most 500 characters.

Answer: any `2xx`. `404`, `409` and `410` are also final for the agent: the
job is already settled or gone, and it will not report it again.

The agent may send the same `printed` report more than once, with the same
`printer.host` each time. A repeat can arrive long after the first, when the
printer may have moved again, so only the first says where the printer is.
An error answer makes the agent send the report again, whatever the reason:
a `printer.host` that is not wanted is ignored, not refused.

A job reported as `failed` is offered again by a later `jobs/next`. The agent
waits 5 seconds after a failure, so a printer that is off or open is retried
every 5 seconds until it works or the job is no longer offered.

## A printer whose address changes

A router hands the printer its address and can hand it another: a lease
expires, the router restarts or is replaced. The printer's MAC never
changes, and the agent is on the printer's network, so a job that carries
`printer.mac` is printed like this:

1. The agent connects to the address where this MAC last printed, if it
   remembers one, and then to `printer.host`.
2. If the system can tell which MAC is behind the address it connected to,
   and it is another device's, the agent closes the connection without
   writing anything. If the system cannot tell, the agent writes: losing a
   chit is worse than the small risk of a wrong device.
3. If no address took the job, the agent looks the MAC up on the private
   IPv4 networks the computer is directly on, and connects to the address
   it finds. The lookup asks at most 254 addresses on each of at most 3
   networks, takes at most 3 seconds, and runs at most once every 30
   seconds for one MAC; in between, the last answer is used.
4. The `printed` report says which address worked. Jobs that carry that
   address as their `printer.host` from then on print at the first try,
   also after the agent has restarted and forgotten it.

The agent only ever connects to a private address, whether the job gave it
or the lookup found it.

One attempt, lookup included, takes at most 25 seconds, the same as for a
job without a `mac`, so the result arrives before the job is offered again.

When the job does not print, `error` says which of these happened:

| `error` | Meaning |
| --- | --- |
| `printer AA:BB:CC:DD:EE:FF is not on this computer's network (looked on 192.168.1.0/24)` | Nothing on the computer's networks answers with this MAC: the printer is off, or the computer is on another network than the printer. The networks searched are listed, separated by commas. When an address that was tried now belongs to another device, the sentence goes on: `; 192.168.1.50 is now another device (11:22:33:44:55:66)` |
| `printer AA:BB:CC:DD:EE:FF is at 192.168.1.37 and refuses connections on port 9100: ...` | The printer is on the network, and its port did not accept the connection. After the colon comes the system's reason. Some printers refuse one connection and accept the next, so this can pass by itself |
| `this computer is not connected to a network` | The computer has no address on a private network |

The agent retries all three every 5 seconds, like any failed job.

Limits: a printer behind a Wi-Fi repeater that replaces the MAC of the
devices behind it, or on another network segment than the computer, is not
found by its MAC. See the README.

## What "printed" means, and who prevents duplicates

`printed` means the printer's network port accepted every byte. A raw port
gives no receipt. A printer that is out of paper may accept the job and print
it once paper is loaded, or lose it if it is switched off first.

A lost chit is worse than a duplicate, so delivery is at least once:

| Failure | What happens |
| --- | --- |
| The printer is off or unreachable | `failed` is reported; the job is offered again |
| The printer's address changed, and the job has its MAC | The agent finds the printer, prints, and reports the address it printed on |
| The agent or the PC dies before printing | No result arrives; the job is offered again later |
| The job printed but the `printed` report was lost | The agent remembers it in `state.json`. It reports again before asking for more work, also after a restart. If the job is offered again meanwhile, the agent reports it and does **not** print it again |
| The agent dies after the printer accepted the job and before it saved that fact | The job prints twice. This window is a few milliseconds |

The agent forgets a reported job after 24 hours. A job offered again later
than that would print a second time.
