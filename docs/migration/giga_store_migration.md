# Giga Store Migration Guide

## Overview
For most operators, migrating to Giga store is a single-step state sync.

Start a fresh node with the target Giga config, wipe local state, and state sync into the new layout. There is no live migration path in this guide.

## Target Config
Apply the following settings in `app.toml`:

```toml
[state-commit]
sc-enable = true

[state-store]
ss-enable = true
ss-backend = "pebbledb"          # or rocksdb
evm-ss-write-mode = "split_write"
evm-ss-read-mode = "split_read"
evm-ss-separate-dbs = false      # keep a single EVM SS DB

[evm]
enable_parallelized_block_trace = true
```

If you are using RocksDB, build with `-tags rocksdbBackend` and set `ss-backend = "rocksdb"`.

## Why State Sync Works
State sync imports snapshot data using the importing node's state-store write mode.

With `evm-ss-write-mode = "split_write"`:
- EVM snapshot nodes import directly into EVM SS
- non-EVM snapshot nodes import into Cosmos SS

The import path also normalizes `evm_flatkv` snapshots to `evm`, so the source snapshot format does not matter.

Because the target layout is fully populated at the snapshot height, the node can start directly with `evm-ss-read-mode = "split_read"`.

## Migration Steps
1. Upgrade to a binary that includes Giga store support.
2. Set the target config above in `app.toml`.
3. Stop `seid`.
4. Back up validator files you need to preserve.
5. Remove the existing local state.
6. State sync the node.
7. Start `seid`.

For a reference state sync flow, see the [SeiDB Migration Guide](./seidb_migration.md).

## Verification
- Startup logs should show EVM state-store optimization enabled with `writeMode=split_write` and `readMode=split_read`.
- If this node serves EVM RPC, confirm `debug_traceBlockByNumber` or `debug_traceBlockByHash` succeeds.

## Warnings
- Existing non-Giga local data directories are not backwards compatible. Wipe state and state sync.
- Do not enable state-store on an existing local node without rebuilding state.
- `split_read` has no Cosmos fallback. Use it only with the state-sync path above.
