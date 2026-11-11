# Cheflink print agent

A small program for a restaurant's cashier PC that prints Cheflink kitchen
chits by themselves. It asks Cheflink for new chits every couple of seconds
and writes each one to the kitchen's network printer. Nobody taps "print".

It is for kitchens that work from paper. The browser cannot do this: a web
page cannot print without a person confirming the dialog, and Cheflink's
server cannot reach a printer inside the venue's network.

```
Cheflink  <--- "anything to print?" ---  agent on the cashier PC  --- bytes --->  kitchen printer
 (cloud)   ---  one chit, as bytes  --->   (this program)          (TCP 9100)      (venue's Wi-Fi)
```

The agent is deliberately dumb. Cheflink builds the whole chit as printer
commands; the agent fetches the bytes, sends them and reports back. A change
to the chit's layout never needs a visit to a cashier PC.

## Where this stands (2026-11-10)

| | |
| --- | --- |
| Fetch, print, report, retry | Works, against the stand-in server and a pretend printer, on Linux and on Windows 11 |
| Automated tests | Pass (`make test`) |
| A real printer | **Not tried yet.** `test-print` sends the same bytes as the PowerShell test that worked on the Sewoo SLK-TS400 |
| Windows service (`install`, `start`, `stop`) | **Written, never run.** It compiles; it needs an administrator prompt to try |
| Cheflink's side | **Not built.** `cmd/fakeserver` stands in for it. [docs/protocol.md](docs/protocol.md) is what Cheflink has to implement |

## Try it without Cheflink or a printer

Needs Go 1.27 or later (`go version`).

```sh
make build fakeserver

# Terminal 1: a stand-in Cheflink and a pretend printer
dist/fakeserver -sink 127.0.0.1:9100

# Terminal 2: pair once, then run
dist/agent pair -dir /tmp/agent -server http://127.0.0.1:8080 -code 123456
dist/agent run -dir /tmp/agent
```

Press Enter in terminal 1 to queue a sample chit. Terminal 1 shows the chit
the pretend printer received; terminal 2 shows the agent printing it.

Things worth trying: stop the fake server and start it again; start it with
`-printer 127.0.0.1:9999` (nothing listens there) to see a failed chit being
retried; `-every 10s` for a steady stream.

## Try it with a real printer

First check this machine reaches the printer, with no server involved:

```sh
dist/agent test-print -printer 192.168.1.50:9100
```

It prints two lines and cuts. Then run the loop against the real printer:

```sh
dist/fakeserver -printer 192.168.1.50:9100
```

and pair and run the agent as above. The sample chit ends with a line of
accented letters (`ñ Ñ á é í ó ú ü ¿ ¡ °`). If they print wrong, the printer
wants another code page number: try `-codepage` with the values in the
printer manual's `ESC t` table. The number that works is what Cheflink will
have to send.

## Build

```sh
make            # vet, test, and build for this machine and for Windows
make windows    # dist/cheflink-print-agent.exe, for 64-bit Windows 10 or later
make linux-arm64   # for a Raspberry Pi, where a venue has no cashier PC
```

Every build is one self-contained file of about 7 MB. The Windows `.exe` is
built from Linux or WSL; no Windows toolchain is needed.

## Install on a cashier PC

Not done on a real cashier PC yet; these are the intended steps.

1. Copy `cheflink-print-agent.exe` into a folder of its own, for example
   `C:\CheflinkPrintAgent`. Its files are kept beside it.
2. Open a command prompt **as administrator** in that folder.
3. Pair it with the venue, using the code from the venue's page in Cheflink:

   ```
   cheflink-print-agent.exe pair -server https://THE-VENUES-CHEFLINK-ADDRESS -code 123456
   ```

4. Register and start the service:

   ```
   cheflink-print-agent.exe install
   cheflink-print-agent.exe start
   ```

From then on it starts with Windows, before anyone signs in, and Windows
restarts it if it crashes.

The PC has to be on whenever the kitchen is working, with sleep and
hibernation switched off, and on the same network as the printer. A guest
network, or a router with "client isolation", lets the PC reach Cheflink but
not the printer.

Windows may warn that the program is from an unknown publisher, because the
`.exe` is not signed.

To remove it: `cheflink-print-agent.exe uninstall`, then delete the folder.

## Commands

| Command | What it does |
| --- | --- |
| `pair -server URL -code CODE [-name NAME]` | Redeems a pairing code and saves the token. A running agent picks the new pairing up by itself |
| `run` | Fetches and prints until stopped. In a terminal it logs to the terminal; as a service, to `agent.log` |
| `test-print -printer IP:PORT` | Prints two lines and cuts, without a server |
| `install`, `start`, `stop`, `uninstall` | The Windows service. Administrator prompt only |
| `version` | Prints the version |

Every command takes `-dir FOLDER` for where the agent's files live. The
default is the folder the program is in.

## Its files

| File | What it is |
| --- | --- |
| `config.json` | The server address and the agent's token. Written by `pair`. Treat it as a password: on Windows, anyone who can read the folder can read it |
| `state.json` | The chits printed in the last day and whether Cheflink has been told. It is what stops a chit printing twice |
| `agent.log` | The service's log. At 5 MB it becomes `agent.log.1` and a new one starts |

## How it behaves when things go wrong

| Situation | Behaviour |
| --- | --- |
| The internet or Cheflink is down | Waits and retries, 2 seconds doubling up to 30. One line in the log, and one when it is back. Chits sent meanwhile print when the connection returns, if Cheflink still offers them |
| The printer is off, open or unreachable | Tells Cheflink the chit failed and tries it again every 5 seconds |
| The printer is out of paper | Usually invisible to the agent: the printer accepts the chit and prints it when paper is loaded |
| The PC restarts | The service starts again and reports anything it had printed but not reported |
| The venue revokes the PC | Stops fetching, logs it once, and resumes by itself when paired again |
| The chit's printer address is not on a private network | Refuses the chit and reports it failed |

A lost chit is worse than a duplicate, so in the rare case where the agent
cannot know whether a chit printed, it prints it again. See "What printed
means" in [docs/protocol.md](docs/protocol.md).

## Not built yet

- Cheflink's three endpoints, the print queue and the chit rendered as
  printer commands.
- Asking the printer for its status (paper out, cover open) before and after
  a chit. ESC/POS has commands for it; whether the Sewoo answers them has not
  been tried.
- A signed `.exe` and an installer.
- Updating itself. A new version is a new `.exe` copied over the old one.
- Printing to a USB printer through Windows, for receipts.

## Layout

| Path | |
| --- | --- |
| `cmd/agent` | The program: the commands and the Windows service wrapper |
| `internal/agent` | The loop, the server client, the ledger and the printer connection, with their tests |
| `cmd/fakeserver` | The stand-in for Cheflink, a pretend printer and the sample chit. Development only |
| `docs/protocol.md` | The requests the agent makes, and what the server owes it |

The only dependency outside Go's standard library is `golang.org/x/sys`, for
the Windows service.
