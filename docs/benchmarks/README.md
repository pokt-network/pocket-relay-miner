

## v0.1.3, standalone mode

[Relay-Miner-Standalone-Capacity.pdf](v0.1.3/Relay-Miner-Standalone-Capacity.pdf) (15 pages).

The same load and tests as the v0.1.0 report, run against 1 standalone
process. What it measured:

- **Topology**: 1 process (`pocket-relay-miner standalone`), memory limit
  16 GiB with `GOMEMLIMIT` 14 GiB, CPU limit 20, its store on a local volume,
  against a local chain with ~60 s blocks.
- **Load**: the v0.1.0 report's 6 services on 3 transports, each session served
  by 50 suppliers, sent by `pocket-relay-miner relay` straight to the process,
  each response verified by the client. **10.5 M relays served at ~2,400
  relays/s** over 73 minutes of load (2,420 relays/s in steady state).
- **Settlement**: **1,500 of 1,500 claims settled**, each after its proof, none
  expired, none slashed, with the process **killed twice** on purpose
  (`kill -9`, once in a proof window under load and once in a claim window
  with the load stopped). Each time it was ready again in 13 s.
- **A burst**: 128 concurrent 1 MiB requests for 15 s on top of the full load.
  The process kept running; it refused 31 develop-http relays because that
  service's validation queue (128 MiB) was full. The burst's memory peak was
  not sampled.

### The figures, in text

From the report's "Sizing standalone mode" section (highest reading of the run
unless stated):

| Resource | At ~2,400 relays/s | Read it as |
|---|---|---|
| CPU | 4.35 cores steady, 5.72 at a claim (per-block averages) | set by the relays; never throttled |
| memory | working set at most 3.8 GiB steady, 4.7 GiB in the last reading before a kill in a proof window | the 1 MiB burst's peak was not sampled; 29 % of the 16 GiB limit; follows the live trees (600 at the peak) and falls at every claim |
| disk | at most 2.8 GiB | released at every claim, 202 MiB once every generation settled |
| disk writes | ~28,000 tree writes/s | the write-ahead log synced about once a second; its fsync to disk (not request latency) p99 17 ms steady, 0 sync failures |
| request latency | p50 1.4 ms, p99 7.3 ms per block (jsonrpc and grpc) | median of the steady blocks, bucket-interpolated; p99 607.7 ms in the block of the 1 MiB burst; on the burst's service the mean was 54.7 ms, 40.1 of it the backend call |

### How to read it against your load

- The same caveat as v0.1.0: 1 topology on 1 machine, a reference for sizing,
  not a guarantee.
- **CPU**: size it for the relay rate; at this load the process used 4.35
  cores on average, and 5.72 at one generation's claims (per-block averages).
- **Memory and disk**: size them by the sessions held at once: both follow the
  live claim trees and fall at each claim. Keep the store's free disk well above the 1 GiB floor below which new
  work stops; this run never came near it.
- The limits in the standalone examples (`docker-compose.standalone.yaml`
  6 GiB and 2 CPUs; the host unit `MemoryMax=12G`, `CPUQuota=400%`) are below
  this run's measured CPU use: 4.35 cores on average and 5.72 at a claim.
