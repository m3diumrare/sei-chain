package rootmulti

import (
	"bytes"
	"context"
	"encoding/binary"
	"path/filepath"
	"testing"

	protoio "github.com/gogo/protobuf/io"
	"github.com/sei-protocol/sei-chain/sei-cosmos/store/types"
	"github.com/sei-protocol/sei-chain/sei-db/common/evm"
	seidbconfig "github.com/sei-protocol/sei-chain/sei-db/config"
	"github.com/sei-protocol/sei-chain/sei-db/state_db/sc/flatkv"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Config helpers
// ---------------------------------------------------------------------------

func dualWriteConfig() seidbconfig.StateCommitConfig {
	cfg := seidbconfig.DefaultStateCommitConfig()
	cfg.WriteMode = seidbconfig.DualWrite
	cfg.EnableLatticeHash = true
	cfg.MemIAVLConfig.SnapshotInterval = 1
	cfg.MemIAVLConfig.SnapshotMinTimeInterval = 0
	cfg.MemIAVLConfig.AsyncCommitBuffer = 0
	cfg.HistoricalProofRateLimit = 0
	cfg.HistoricalProofMaxInFlight = 100
	return cfg
}

func integrationSplitWriteConfig() seidbconfig.StateCommitConfig {
	cfg := seidbconfig.DefaultStateCommitConfig()
	cfg.WriteMode = seidbconfig.SplitWrite
	cfg.EnableLatticeHash = true
	cfg.MemIAVLConfig.SnapshotInterval = 1
	cfg.MemIAVLConfig.SnapshotMinTimeInterval = 0
	cfg.MemIAVLConfig.AsyncCommitBuffer = 0
	cfg.HistoricalProofRateLimit = 0
	cfg.HistoricalProofMaxInFlight = 100
	return cfg
}

// ---------------------------------------------------------------------------
// EVM test data and helpers
// ---------------------------------------------------------------------------

type evmTestData struct {
	storKey []byte // 0x03 + addr + slot
	nonKey  []byte // 0x0a + addr
	codeKey []byte // 0x07 + addr
}

func newEVMTestData(seed byte) evmTestData {
	var addr [20]byte
	addr[0] = seed
	addr[19] = 0xFF
	var slot [32]byte
	slot[0] = seed + 1
	slot[31] = 0xEE

	internal := make([]byte, 52)
	copy(internal[:20], addr[:])
	copy(internal[20:], slot[:])

	return evmTestData{
		storKey: evm.BuildMemIAVLEVMKey(evm.EVMKeyStorage, internal),
		nonKey:  evm.BuildMemIAVLEVMKey(evm.EVMKeyNonce, addr[:]),
		codeKey: evm.BuildMemIAVLEVMKey(evm.EVMKeyCode, addr[:]),
	}
}

func makeNonce(n uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, n)
	return b
}

type commitRecord struct {
	version int64
	hash    []byte
	infos   []types.StoreInfo
}

var storeNames = []string{"acc", "bank", "evm"}

func newTestRootMulti(t *testing.T, dir string, scCfg seidbconfig.StateCommitConfig) (*Store, map[string]*types.KVStoreKey) {
	t.Helper()
	store := NewStore(dir, scCfg, seidbconfig.StateStoreConfig{}, nil)
	storeKeys := make(map[string]*types.KVStoreKey)
	for _, name := range storeNames {
		sk := types.NewKVStoreKey(name)
		storeKeys[name] = sk
		store.MountStoreWithDB(sk, types.StoreTypeIAVL, nil)
	}
	require.NoError(t, store.LoadLatestVersion())
	return store, storeKeys
}

func simulateBlock(t *testing.T, store *Store, storeKeys map[string]*types.KVStoreKey, block int, evmData evmTestData) commitRecord {
	t.Helper()
	cms := store.CacheMultiStore()
	b := byte(block)

	cms.GetKVStore(storeKeys["acc"]).Set([]byte("acct1"), []byte{b})
	cms.GetKVStore(storeKeys["bank"]).Set([]byte("supply"), []byte{b, b})
	cms.GetKVStore(storeKeys["evm"]).Set(evmData.storKey, []byte{b, 0xAA})
	cms.GetKVStore(storeKeys["evm"]).Set(evmData.nonKey, makeNonce(uint64(block)))
	if block == 1 {
		cms.GetKVStore(storeKeys["evm"]).Set(evmData.codeKey, []byte{0x60, 0x60, 0x60, b})
	}

	cms.Write()
	_, err := store.GetWorkingHash()
	require.NoError(t, err)
	cid := store.Commit(true)

	infos := make([]types.StoreInfo, len(store.lastCommitInfo.StoreInfos))
	copy(infos, store.lastCommitInfo.StoreInfos)
	return commitRecord{version: cid.Version, hash: cid.Hash, infos: infos}
}

func simulateCosmosOnlyBlock(t *testing.T, store *Store, storeKeys map[string]*types.KVStoreKey, block int) commitRecord {
	t.Helper()
	cms := store.CacheMultiStore()
	b := byte(block)
	cms.GetKVStore(storeKeys["acc"]).Set([]byte("acct1"), []byte{b})
	cms.GetKVStore(storeKeys["bank"]).Set([]byte("supply"), []byte{b})
	cms.Write()
	_, err := store.GetWorkingHash()
	require.NoError(t, err)
	cid := store.Commit(true)

	infos := make([]types.StoreInfo, len(store.lastCommitInfo.StoreInfos))
	copy(infos, store.lastCommitInfo.StoreInfos)
	return commitRecord{version: cid.Version, hash: cid.Hash, infos: infos}
}

func findStoreInfo(infos []types.StoreInfo, name string) *types.StoreInfo {
	for i := range infos {
		if infos[i].Name == name {
			return &infos[i]
		}
	}
	return nil
}

func verifyHistoricalHashes(t *testing.T, store *Store, records []commitRecord) {
	t.Helper()
	for _, rec := range records {
		scStore, err := store.scStore.LoadVersion(rec.version, true)
		require.NoError(t, err)

		commitInfo := convertCommitInfo(scStore.LastCommitInfo())
		commitInfo = amendCommitInfo(commitInfo, store.storesParams)

		require.Equalf(t, rec.hash, commitInfo.Hash(),
			"ROOT HASH MISMATCH at version %d", rec.version)

		_ = scStore.Close()
	}
}

// rollbackFlatKV opens the FlatKV store at dir, loads latest, rolls back to
// the target version, and closes. Used to simulate a crash where FlatKV is
// behind cosmos.
func rollbackFlatKV(t *testing.T, dir string, cfg seidbconfig.StateCommitConfig, target int64) {
	t.Helper()
	flatkvCfg := cfg.FlatKVConfig
	flatkvCfg.DataDir = filepath.Join(dir, "data", "flatkv")
	evmStore, err := flatkv.NewCommitStore(context.Background(), &flatkvCfg)
	require.NoError(t, err)
	_, err = evmStore.LoadVersion(0, false)
	require.NoError(t, err)
	require.NoError(t, evmStore.Rollback(target))
	require.NoError(t, evmStore.Close())
}

// ---------------------------------------------------------------------------
// Test 1: DualWrite + LatticeHash — hash consistency through rootmulti
// ---------------------------------------------------------------------------

func TestFlatKVDualWriteHashConsistency(t *testing.T) {
	store, storeKeys := newTestRootMulti(t, t.TempDir(), dualWriteConfig())
	defer func() { require.NoError(t, store.Close()) }()

	evmData := newEVMTestData(0xAA)
	var records []commitRecord

	for block := 1; block <= 10; block++ {
		rec := simulateBlock(t, store, storeKeys, block, evmData)
		records = append(records, rec)

		lattice := findStoreInfo(rec.infos, "evm_lattice")
		require.NotNilf(t, lattice, "evm_lattice missing at block %d", block)
		require.Lenf(t, lattice.CommitId.Hash, 32, "lattice hash should be 32 bytes at block %d", block)
	}

	// Lattice hash must change between blocks with different EVM data
	for i := 1; i < len(records); i++ {
		prev := findStoreInfo(records[i-1].infos, "evm_lattice")
		curr := findStoreInfo(records[i].infos, "evm_lattice")
		require.NotEqual(t, prev.CommitId.Hash, curr.CommitId.Hash,
			"lattice hash must change between blocks %d and %d", i, i+1)
	}

	verifyHistoricalHashes(t, store, records)
}

// ---------------------------------------------------------------------------
// Test 2: SplitWrite — hash consistency, EVM data not in memiavl tree
// ---------------------------------------------------------------------------

func TestFlatKVSplitWriteHashConsistency(t *testing.T) {
	store, storeKeys := newTestRootMulti(t, t.TempDir(), integrationSplitWriteConfig())
	defer func() { require.NoError(t, store.Close()) }()

	evmData := newEVMTestData(0xBB)
	var records []commitRecord

	for block := 1; block <= 10; block++ {
		rec := simulateBlock(t, store, storeKeys, block, evmData)
		records = append(records, rec)

		lattice := findStoreInfo(rec.infos, "evm_lattice")
		require.NotNilf(t, lattice, "evm_lattice missing at block %d", block)
		require.NotEmpty(t, lattice.CommitId.Hash)

		// In SplitWrite the "evm" memiavl tree receives no data; its IAVL hash
		// must remain unchanged across blocks.
		if block > 1 {
			prev := findStoreInfo(records[block-2].infos, "evm")
			curr := findStoreInfo(rec.infos, "evm")
			require.Equal(t, prev.CommitId.Hash, curr.CommitId.Hash,
				"evm IAVL hash should not change in SplitWrite mode (block %d)", block)
		}
	}

	verifyHistoricalHashes(t, store, records)
}

// ---------------------------------------------------------------------------
// Test 3: Determinism — two stores with identical data produce identical hashes
// ---------------------------------------------------------------------------

func TestFlatKVLatticeHashDeterminism(t *testing.T) {
	cfg := dualWriteConfig()
	evmData := newEVMTestData(0xCC)

	var hashes [2][]byte
	var latticeHashes [2][]byte

	for i := 0; i < 2; i++ {
		store, storeKeys := newTestRootMulti(t, t.TempDir(), cfg)
		for block := 1; block <= 5; block++ {
			simulateBlock(t, store, storeKeys, block, evmData)
		}
		hashes[i] = store.lastCommitInfo.Hash()
		lattice := findStoreInfo(store.lastCommitInfo.StoreInfos, "evm_lattice")
		require.NotNil(t, lattice)
		latticeHashes[i] = lattice.CommitId.Hash
		require.NoError(t, store.Close())
	}

	require.Equal(t, hashes[0], hashes[1], "app hashes must be deterministic")
	require.Equal(t, latticeHashes[0], latticeHashes[1], "lattice hashes must be deterministic")
}

// ---------------------------------------------------------------------------
// Test 4: Sensitivity — single byte change in EVM data changes lattice hash
// ---------------------------------------------------------------------------

func TestFlatKVLatticeHashSensitivity(t *testing.T) {
	cfg := dualWriteConfig()
	evmData := newEVMTestData(0xDD)

	storeA, keysA := newTestRootMulti(t, t.TempDir(), cfg)
	for block := 1; block <= 3; block++ {
		simulateBlock(t, storeA, keysA, block, evmData)
	}

	storeB, keysB := newTestRootMulti(t, t.TempDir(), cfg)
	for block := 1; block <= 3; block++ {
		if block == 3 {
			cms := storeB.CacheMultiStore()
			cms.GetKVStore(keysB["acc"]).Set([]byte("acct1"), []byte{byte(block)})
			cms.GetKVStore(keysB["bank"]).Set([]byte("supply"), []byte{byte(block), byte(block)})
			// 0xBB instead of 0xAA — single byte difference
			cms.GetKVStore(keysB["evm"]).Set(evmData.storKey, []byte{byte(block), 0xBB})
			cms.GetKVStore(keysB["evm"]).Set(evmData.nonKey, makeNonce(uint64(block)))
			cms.Write()
			_, err := storeB.GetWorkingHash()
			require.NoError(t, err)
			storeB.Commit(true)
		} else {
			simulateBlock(t, storeB, keysB, block, evmData)
		}
	}

	latticeA := findStoreInfo(storeA.lastCommitInfo.StoreInfos, "evm_lattice")
	latticeB := findStoreInfo(storeB.lastCommitInfo.StoreInfos, "evm_lattice")
	require.NotEqual(t, latticeA.CommitId.Hash, latticeB.CommitId.Hash,
		"lattice hash must differ when EVM data differs by a single byte")
	require.NotEqual(t, storeA.lastCommitInfo.Hash(), storeB.lastCommitInfo.Hash(),
		"app hash must differ when lattice hash differs")

	require.NoError(t, storeA.Close())
	require.NoError(t, storeB.Close())
}

// ---------------------------------------------------------------------------
// Test 5: Double flush (FinalizeBlock + Commit) with DualWrite
// ---------------------------------------------------------------------------

func TestFlatKVDualWriteDoubleFlush(t *testing.T) {
	store, storeKeys := newTestRootMulti(t, t.TempDir(), dualWriteConfig())
	defer func() { require.NoError(t, store.Close()) }()

	evmData := newEVMTestData(0xEE)
	var records []commitRecord

	for block := 1; block <= 5; block++ {
		cms := store.CacheMultiStore()
		b := byte(block)

		cms.GetKVStore(storeKeys["acc"]).Set([]byte("acct1"), []byte{b})
		cms.GetKVStore(storeKeys["bank"]).Set([]byte("supply"), []byte{b, b})
		cms.GetKVStore(storeKeys["evm"]).Set(evmData.storKey, []byte{b, 0xAA})
		cms.GetKVStore(storeKeys["evm"]).Set(evmData.nonKey, makeNonce(uint64(block)))

		// Simulate FinalizeBlock: Write + GetWorkingHash
		cms.Write()
		_, err := store.GetWorkingHash()
		require.NoError(t, err)

		// Simulate Commit: Write + GetWorkingHash + Commit (double flush)
		cms.Write()
		_, err = store.GetWorkingHash()
		require.NoError(t, err)
		cid := store.Commit(true)

		infos := make([]types.StoreInfo, len(store.lastCommitInfo.StoreInfos))
		copy(infos, store.lastCommitInfo.StoreInfos)
		records = append(records, commitRecord{version: cid.Version, hash: cid.Hash, infos: infos})
	}

	for _, rec := range records {
		scStore, err := store.scStore.LoadVersion(rec.version, true)
		require.NoError(t, err)

		commitInfo := convertCommitInfo(scStore.LastCommitInfo())
		commitInfo = amendCommitInfo(commitInfo, store.storesParams)
		require.Equalf(t, rec.hash, commitInfo.Hash(),
			"ROOT HASH MISMATCH at version %d (double flush)", rec.version)

		lattice := findStoreInfo(commitInfo.StoreInfos, "evm_lattice")
		require.NotNilf(t, lattice, "evm_lattice must survive double flush at version %d", rec.version)
		_ = scStore.Close()
	}
}

// ---------------------------------------------------------------------------
// Test 6: Rollback preserves lattice hash correctness
// ---------------------------------------------------------------------------

func TestFlatKVRollbackWithLatticeHash(t *testing.T) {
	store, storeKeys := newTestRootMulti(t, t.TempDir(), dualWriteConfig())
	evmData := newEVMTestData(0x11)

	var records []commitRecord
	for block := 1; block <= 5; block++ {
		records = append(records, simulateBlock(t, store, storeKeys, block, evmData))
	}

	require.NoError(t, store.RollbackToVersion(3))
	require.Equal(t, int64(3), store.LastCommitID().Version)
	require.Equal(t, records[2].hash, store.lastCommitInfo.Hash(),
		"after rollback to v3, app hash must match original v3")

	lattice := findStoreInfo(store.lastCommitInfo.StoreInfos, "evm_lattice")
	origLattice := findStoreInfo(records[2].infos, "evm_lattice")
	require.Equal(t, origLattice.CommitId.Hash, lattice.CommitId.Hash,
		"lattice hash must match original v3 after rollback")

	// New commits after rollback produce sequential versions
	for block := 4; block <= 7; block++ {
		rec := simulateBlock(t, store, storeKeys, block+100, evmData)
		require.Equal(t, int64(block), rec.version)
		require.NotNil(t, findStoreInfo(rec.infos, "evm_lattice"))
	}

	require.NoError(t, store.Close())
}

// ---------------------------------------------------------------------------
// Test 7: Crash recovery — FlatKV behind cosmos, version reconciliation
// ---------------------------------------------------------------------------

func TestFlatKVCrashRecoveryThroughRootMulti(t *testing.T) {
	dir := t.TempDir()
	cfg := dualWriteConfig()
	evmData := newEVMTestData(0x22)

	// Phase 1: commit 5 blocks
	store1, storeKeys1 := newTestRootMulti(t, dir, cfg)
	var records []commitRecord
	for block := 1; block <= 5; block++ {
		records = append(records, simulateBlock(t, store1, storeKeys1, block, evmData))
	}
	require.NoError(t, store1.Close())

	// Simulate crash: roll FlatKV back to version 3
	rollbackFlatKV(t, dir, cfg, 3)

	// Phase 2: reopen — reconciliation should bring both to version 3
	store2, storeKeys2 := newTestRootMulti(t, dir, cfg)

	require.Equal(t, int64(3), store2.LastCommitID().Version,
		"after crash recovery, version should reconcile to 3")
	require.Equal(t, records[2].hash, store2.lastCommitInfo.Hash(),
		"after crash recovery, app hash must match original v3")

	lattice := findStoreInfo(store2.lastCommitInfo.StoreInfos, "evm_lattice")
	origLattice := findStoreInfo(records[2].infos, "evm_lattice")
	require.NotNil(t, lattice)
	require.NotNil(t, origLattice)
	require.Equal(t, origLattice.CommitId.Hash, lattice.CommitId.Hash,
		"lattice hash must match original v3 after crash recovery")

	// Chain must continue making progress
	for block := 4; block <= 8; block++ {
		rec := simulateBlock(t, store2, storeKeys2, block+200, evmData)
		require.Equal(t, int64(block), rec.version)
		require.NotNil(t, findStoreInfo(rec.infos, "evm_lattice"))
	}

	require.NoError(t, store2.Close())
}

// ---------------------------------------------------------------------------
// Test 8: Snapshot and Restore round-trip with lattice hash
// ---------------------------------------------------------------------------

func TestFlatKVSnapshotRestoreWithLatticeHash(t *testing.T) {
	cfg := dualWriteConfig()
	evmData := newEVMTestData(0x33)

	// Source: commit 5 blocks
	srcStore, srcKeys := newTestRootMulti(t, t.TempDir(), cfg)
	for block := 1; block <= 5; block++ {
		simulateBlock(t, srcStore, srcKeys, block, evmData)
	}

	srcHash := srcStore.lastCommitInfo.Hash()
	srcLattice := findStoreInfo(srcStore.lastCommitInfo.StoreInfos, "evm_lattice")
	require.NotNil(t, srcLattice)
	srcLatticeHash := srcLattice.CommitId.Hash
	t.Logf("source: ver=5 appHash=%X lattice=%X", srcHash, srcLatticeHash)

	// Snapshot to buffer
	var buf bytes.Buffer
	writer := protoio.NewDelimitedWriter(&buf)
	require.NoError(t, srcStore.Snapshot(5, writer))
	require.NoError(t, srcStore.Close())
	require.NotEmpty(t, buf.Bytes())

	// Destination: restore from snapshot
	dstStore, _ := newTestRootMulti(t, t.TempDir(), cfg)
	reader := protoio.NewDelimitedReader(bytes.NewReader(buf.Bytes()), 1<<30)
	_, err := dstStore.Restore(5, 1, reader)
	require.NoError(t, err)

	require.Equal(t, int64(5), dstStore.LastCommitID().Version)
	require.Equal(t, srcHash, dstStore.lastCommitInfo.Hash(),
		"restored app hash must match source")

	dstLattice := findStoreInfo(dstStore.lastCommitInfo.StoreInfos, "evm_lattice")
	require.NotNil(t, dstLattice, "evm_lattice must be present after restore")
	require.Equal(t, srcLatticeHash, dstLattice.CommitId.Hash,
		"restored lattice hash must match source")

	// Continue committing after restore
	dstKeys := make(map[string]*types.KVStoreKey)
	for name, key := range dstStore.storeKeys {
		if kvKey, ok := key.(*types.KVStoreKey); ok {
			dstKeys[name] = kvKey
		}
	}
	rec := simulateBlock(t, dstStore, dstKeys, 6, evmData)
	require.Equal(t, int64(6), rec.version)
	newLattice := findStoreInfo(rec.infos, "evm_lattice")
	require.NotNil(t, newLattice)
	require.NotEqual(t, srcLatticeHash, newLattice.CommitId.Hash,
		"lattice hash should change after new commit post-restore")

	require.NoError(t, dstStore.Close())
}

// ---------------------------------------------------------------------------
// Test 9: Empty EVM blocks — lattice hash stays stable
// ---------------------------------------------------------------------------

func TestFlatKVEmptyEVMBlocks(t *testing.T) {
	store, storeKeys := newTestRootMulti(t, t.TempDir(), dualWriteConfig())
	defer func() { require.NoError(t, store.Close()) }()

	evmData := newEVMTestData(0x44)

	// Block 1: write EVM data
	rec1 := simulateBlock(t, store, storeKeys, 1, evmData)
	lattice1 := findStoreInfo(rec1.infos, "evm_lattice")
	require.NotNil(t, lattice1)

	// Blocks 2-4: cosmos only — no EVM writes
	for block := 2; block <= 4; block++ {
		rec := simulateCosmosOnlyBlock(t, store, storeKeys, block)
		lattice := findStoreInfo(rec.infos, "evm_lattice")
		require.NotNil(t, lattice)
		require.Equalf(t, lattice1.CommitId.Hash, lattice.CommitId.Hash,
			"lattice hash should not change without EVM writes (block %d)", block)
	}

	// Block 5: write EVM data again — lattice hash must change
	rec5 := simulateBlock(t, store, storeKeys, 5, evmData)
	lattice5 := findStoreInfo(rec5.infos, "evm_lattice")
	require.NotNil(t, lattice5)
	require.NotEqual(t, lattice1.CommitId.Hash, lattice5.CommitId.Hash,
		"lattice hash must change when EVM data changes again")
}

// ---------------------------------------------------------------------------
// Test 10: Mixed cosmos+EVM blocks — selective lattice hash changes
// ---------------------------------------------------------------------------

func TestFlatKVMixedCosmosAndEVMBlocks(t *testing.T) {
	store, storeKeys := newTestRootMulti(t, t.TempDir(), dualWriteConfig())
	defer func() { require.NoError(t, store.Close()) }()

	evmData := newEVMTestData(0x55)
	var records []commitRecord

	for block := 1; block <= 10; block++ {
		if block%2 == 1 {
			// Odd blocks: full block with EVM data
			records = append(records, simulateBlock(t, store, storeKeys, block, evmData))
		} else {
			// Even blocks: cosmos only
			records = append(records, simulateCosmosOnlyBlock(t, store, storeKeys, block))
		}
	}

	verifyHistoricalHashes(t, store, records)

	// Lattice hash should only change on odd blocks
	for i := 1; i < len(records); i++ {
		prev := findStoreInfo(records[i-1].infos, "evm_lattice")
		curr := findStoreInfo(records[i].infos, "evm_lattice")
		require.NotNil(t, prev)
		require.NotNil(t, curr)

		block := i + 1
		if block%2 == 1 {
			require.NotEqualf(t, prev.CommitId.Hash, curr.CommitId.Hash,
				"lattice hash should change on EVM-write block %d", block)
		} else {
			require.Equalf(t, prev.CommitId.Hash, curr.CommitId.Hash,
				"lattice hash should be stable on cosmos-only block %d", block)
		}
	}
}
