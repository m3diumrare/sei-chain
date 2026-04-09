# Giga Store Migration Guide

## Overview
For most operators, migrating to Giga is mostly just a fresh state sync into a node with Giga support enabled.

The target setup has two parts:
- Giga executor enabled
- EVM state-store routed fully to the Giga layout with `split_write`, `split_read`, and a single EVM SS DB

This guide does not cover an in-place migration of existing local data.

## Important Warnings
- Treat this as a fresh-state migration. Do not reuse an existing pre-Giga local data directory.
- Enabling state-store on a node without rebuilding state is unsafe. The app explicitly rejects enabling SS without state sync because it can cause data corruption.
- `giga_executor` is opt-in and experimental.
- When Giga executor is enabled, Tendermint skips `LastResultsHash` validation because gas-used values may differ.
- Only change EVM state-store routing here. Do not enable state-commit `split_write` unless there is separate network guidance, because SC `split_write` requires lattice hash.
- `evm-ss-separate-dbs = false` is the default and recommended setting. Keep a single EVM SS DB for this migration.

## Target Config
Apply the following settings in `app.toml`:

```toml
[state-commit]
sc-enable = true

[state-store]
ss-enable = true
ss-backend = "pebbledb"
evm-ss-write-mode = "split_write"
evm-ss-read-mode = "split_read"
evm-ss-separate-dbs = false

[giga_executor]
enabled = true
occ_enabled = false

[evm]
enable_parallelized_block_trace = true
```

Notes:
- `occ_enabled = false` keeps the initial rollout simpler. Turn on OCC separately once you specifically want the parallel Giga executor path.
- If you are using RocksDB, build with the RocksDB tag and set `ss-backend = "rocksdb"` instead.
- This guide assumes your binary includes the Giga and EVM SS config fields shown above.

## Migration Steps
1. Upgrade to a binary that includes Giga support.
2. Stop `seid`.
3. Back up validator keys and any local files you need to preserve.
4. Remove the existing local state.
5. Apply the target config in `app.toml`.
6. State sync the node using the normal state sync procedure.
7. Start the node and let it complete state sync.

For a reference state sync flow, see the [SeiDB Migration Guide](./seidb_migration.md).

## Verification
- On startup, logs should show `benchmark: Giga Executor is ENABLED - using new EVM execution path (sequential)`.
- Logs should also show `SeiDB EVM StateStore optimization is enabled` with `writeMode=split_write` and `readMode=split_read`.
- If this node serves EVM RPC, run a sample `debug_traceBlockByNumber` or `debug_traceBlockByHash` request and confirm it succeeds.

## Rollback
Rollback also requires a fresh state sync.

Disable the Giga-specific settings, wipe local state again, and state sync back into the non-Giga layout. Do not reuse a Giga-enabled data directory with a pre-Giga configuration.
