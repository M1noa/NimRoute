# Documentation

If you just want to build and run the proxy, start with the [README](../README.md).

| Guide | What's inside |
|---|---|
| [Backends](backends.md) | Provider prefixes (`opencode/`, `freeepi/`, `antseed/`, `nous/`, `nvidia/`, `route/`) and credentials |
| [Service & CLI](service.md) | `./nim` commands, flags, environment variables, and service install |
| [Troubleshooting](troubleshooting.md) | What HTTP 429/403/502/503 responses mean and how to capture debug dumps |
| [Architecture](architecture.md) | Request flow, session lanes, proxy pool rotation, PII vault, and RTK |
| [Development](development.md) | Building (`-tags ner`), running tests, and testing on a scratch port |
| [TODO](TODO.md) | Open work items |
| [Reference](reference/) | Protocol specs for AntSeed, FreePI, and RTK |
| [Archive](archive/) | Historical design notes and past audits |

Contributor rules and invariants for coding agents live in [`AGENTS.md`](../AGENTS.md).
