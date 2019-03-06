package db

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	. "github.com/nknorg/nkn/block"
	. "github.com/nknorg/nkn/common"
	"github.com/nknorg/nkn/common/serialization"
	. "github.com/nknorg/nkn/pb"
	. "github.com/nknorg/nkn/transaction"
	"github.com/nknorg/nkn/util/log"
)

type ChainStore struct {
	st IStore

	mu          sync.RWMutex
	blockCache  map[Uint256]*Block
	headerCache *HeaderCache
	States      *StateDB

	currentBlockHash   Uint256
	currentBlockHeight uint32
}

func NewLedgerStore() (*ChainStore, error) {
	st, err := NewLevelDBStore("ChainDB")
	if err != nil {
		return nil, err
	}

	chain := &ChainStore{
		st:                 st,
		blockCache:         map[Uint256]*Block{},
		headerCache:        NewHeaderCache(),
		currentBlockHeight: 0,
		currentBlockHash:   EmptyUint256,
	}

	return chain, nil
}

func (cs *ChainStore) Close() {
	cs.st.Close()
}

func (cs *ChainStore) ResetDB() error {
	cs.st.NewBatch()
	iter := cs.st.NewIterator(nil)
	for iter.Next() {
		cs.st.BatchDelete(iter.Key())
	}
	iter.Release()

	return cs.st.BatchCommit()
}

func (cs *ChainStore) InitLedgerStoreWithGenesisBlock(genesisBlock *Block) (uint32, error) {
	version, err := cs.st.Get(versionKey())
	if err != nil {
		version = []byte{0x00}
	}

	if version[0] == 0x01 {
		if !cs.IsBlockInStore(genesisBlock.Hash()) {
			return 0, errors.New("genesisBlock is NOT in BlockStore.")
		}

		if cs.currentBlockHash, cs.currentBlockHeight, err = cs.getCurrentBlockHashFromDB(); err != nil {
			return 0, err
		}
		currentHeader, _ := cs.GetHeader(cs.currentBlockHash)

		cs.headerCache.AddHeaderToCache(currentHeader)

		root := cs.GetCurrentBlockStateRoot()
		fmt.Println("---------", root)
		cs.States, _ = NewStateDB(root, NewTrieStore(cs.GetDatabase()))

		return cs.currentBlockHeight, nil

	} else {
		if err := cs.ResetDB(); err != nil {
			return 0, fmt.Errorf("InitLedgerStoreWithGenesisBlock, ResetDB error: %v", err)
		}

		root := EmptyUint256
		cs.States, _ = NewStateDB(root, NewTrieStore(cs.GetDatabase()))

		if err := cs.persist(genesisBlock); err != nil {
			return 0, err
		}

		// put version to db
		if err = cs.st.Put(versionKey(), []byte{0x01}); err != nil {
			return 0, err
		}

		cs.headerCache.AddHeaderToCache(genesisBlock.Header)
		cs.currentBlockHash = genesisBlock.Hash()
		cs.currentBlockHeight = 0

		return 0, nil
	}
}

func (cs *ChainStore) IsTxHashDuplicate(txhash Uint256) bool {
	if _, err := cs.st.Get(transactionKey(txhash)); err != nil {
		return false
	}

	return true
}

func (cs *ChainStore) GetBlockHash(height uint32) (Uint256, error) {
	blockHash, err := cs.st.Get(blockhashKey(height))
	if err != nil {
		return EmptyUint256, err
	}

	return Uint256ParseFromBytes(blockHash)
}

func (cs *ChainStore) GetBlockByHeight(height uint32) (*Block, error) {
	hash, err := cs.GetBlockHash(height)
	if err != nil {
		return nil, err
	}

	return cs.GetBlock(hash)
}

func (cs *ChainStore) GetHeader(hash Uint256) (*Header, error) {
	data, err := cs.st.Get(headerKey(hash))
	if err != nil {
		return nil, err
	}

	h := new(Header)
	dt, _ := serialization.ReadVarBytes(bytes.NewReader(data))
	err = h.Unmarshal(dt)
	if err != nil {
		return nil, err
	}

	return h, nil
}

func (cs *ChainStore) GetHeaderByHeight(height uint32) (*Header, error) {
	hash, err := cs.GetBlockHash(height)
	if err != nil {
		return nil, err
	}

	return cs.GetHeader(hash)
}

func (cs *ChainStore) GetTransaction(hash Uint256) (*Transaction, error) {
	t, _, err := cs.getTx(hash)
	if err != nil {
		return nil, err
	}

	return t, nil
}

func (cs *ChainStore) getTx(hash Uint256) (*Transaction, uint32, error) {
	value, err := cs.st.Get(transactionKey(hash))
	if err != nil {
		return nil, 0, err
	}

	height := binary.LittleEndian.Uint32(value)
	value = value[4:]
	var txn Transaction
	if err := txn.Unmarshal(value); err != nil {
		return nil, height, err
	}

	return &txn, height, nil
}

func (cs *ChainStore) GetBlock(hash Uint256) (*Block, error) {
	bHash, err := cs.st.Get(headerKey(hash))
	if err != nil {
		return nil, err
	}

	b := new(Block)
	if err = b.FromTrimmedData(bytes.NewReader(bHash)); err != nil {
		return nil, err
	}

	for i := 0; i < len(b.Transactions); i++ {
		if b.Transactions[i], _, err = cs.getTx(b.Transactions[i].Hash()); err != nil {
			return nil, err
		}
	}

	return b, nil
}

func (cs *ChainStore) GetHeightByBlockHash(hash Uint256) (uint32, error) {
	header, err := cs.getHeaderWithCache(hash)
	if err == nil {
		return header.UnsignedHeader.Height, nil
	}

	block, err := cs.GetBlock(hash)
	if err != nil {
		return 0, err
	}

	return block.Header.UnsignedHeader.Height, nil
}

func (cs *ChainStore) IsBlockInStore(hash Uint256) bool {
	if header, err := cs.GetHeader(hash); err != nil || header.UnsignedHeader.Height > cs.currentBlockHeight {
		return false
	}

	return true
}

func (cs *ChainStore) persist(b *Block) error {
	cs.st.NewBatch()

	headerHash := b.Hash()

	//batch put header
	headerBuffer := bytes.NewBuffer(nil)
	b.Trim(headerBuffer)
	if err := cs.st.BatchPut(headerKey(headerHash), headerBuffer.Bytes()); err != nil {
		return err
	}

	//batch put headerhash
	headerHashBuffer := bytes.NewBuffer(nil)
	headerHash.Serialize(headerHashBuffer)
	if err := cs.st.BatchPut(blockhashKey(b.Header.UnsignedHeader.Height), headerHashBuffer.Bytes()); err != nil {
		return err
	}

	//batch put transactions
	for _, txn := range b.Transactions {
		buffer := make([]byte, 4)
		binary.LittleEndian.PutUint32(buffer[:], b.Header.UnsignedHeader.Height)
		dt, _ := txn.Marshal()
		buffer = append(buffer, dt...)

		if err := cs.st.BatchPut(transactionKey(txn.Hash()), buffer); err != nil {
			return err
		}

		//TODO if need New?
		if txn.UnsignedTx.Payload.Type != CoinbaseType {
			pg, _ := ToCodeHash(txn.Programs[0].Code)
			nonce := cs.States.GetNonce(pg)
			err := cs.States.SetNonce(pg, nonce+1)
			if err != nil {
				return errors.New("nonce increasement error")
			}
		}

		pl, err := Unpack(txn.UnsignedTx.Payload)
		if err != nil {
			return err
		}

		switch txn.UnsignedTx.Payload.Type {
		case CoinbaseType:
			coinbase := pl.(*Coinbase)
			acc := cs.States.GetOrNewAccount(BytesToUint160(coinbase.Recipient))
			amount := acc.GetBalance()
			acc.SetBalance(amount + Fixed64(coinbase.Amount))
			cs.States.setAccount(BytesToUint160(coinbase.Recipient), acc)
		case TransferAssetType:
			transfer := pl.(*TransferAsset)
			accSender := cs.States.GetOrNewAccount(BytesToUint160(transfer.Sender))
			amountSender := accSender.GetBalance()
			accSender.SetBalance(amountSender - Fixed64(transfer.Amount))
			cs.States.setAccount(BytesToUint160(transfer.Sender), accSender)

			accRecipient := cs.States.GetOrNewAccount(BytesToUint160(transfer.Recipient))
			amountRecipient := accRecipient.GetBalance()
			accRecipient.SetBalance(amountRecipient + Fixed64(transfer.Amount))
			cs.States.setAccount(BytesToUint160(transfer.Recipient), accRecipient)
		case RegisterNameType:
			registerNamePayload := pl.(*RegisterName)
			err = cs.SaveName(registerNamePayload.Registrant, registerNamePayload.Name)
			if err != nil {
				return err
			}
		case DeleteNameType:
			deleteNamePayload := pl.(*DeleteName)
			err = cs.DeleteName(deleteNamePayload.Registrant)
			if err != nil {
				return err
			}
		case SubscribeType:
			subscribePayload := pl.(*Subscribe)
			err = cs.Subscribe(subscribePayload.Subscriber, subscribePayload.Identifier, subscribePayload.Topic, subscribePayload.Bucket, subscribePayload.Duration, subscribePayload.Meta, b.Header.UnsignedHeader.Height)
			if err != nil {
				return err
			}

		}
	}

	expiredKeys := cs.GetExpiredKeys(b.Header.UnsignedHeader.Height)
	for i := 0; i < len(expiredKeys); i++ {
		err := cs.RemoveExpiredKey(expiredKeys[i])
		if err != nil {
			return err
		}
	}

	//StateRoot
	root, err := cs.States.CommitTo(true)
	if err != nil {
		return err
	}

	headerRoot, _ := Uint256ParseFromBytes(b.Header.UnsignedHeader.StateRoot)
	if ok := root.CompareTo(headerRoot); ok != 0 {
		//TODO clean states accounts
		return fmt.Errorf("state root not equal:%v, %v", root, headerRoot)
	}

	err = cs.st.BatchPut(currentStateTrie(), root.ToArray())
	if err != nil {
		return err
	}

	//batch put currentblockhash
	serialization.WriteUint32(headerHashBuffer, b.Header.UnsignedHeader.Height)
	err = cs.st.BatchPut(currentBlockHashKey(), headerHashBuffer.Bytes())
	if err != nil {
		return err
	}

	return cs.st.BatchCommit()
}

func (cs *ChainStore) SaveBlock(b *Block, fastAdd bool) error {
	if err := cs.persist(b); err != nil {
		log.Error("error to persist block:", err.Error())
		return err
	}

	cs.mu.Lock()
	cs.currentBlockHeight = b.Header.UnsignedHeader.Height
	cs.currentBlockHash = b.Hash()
	cs.mu.Unlock()

	if cs.currentBlockHeight > 3 {
		cs.headerCache.RemoveCachedHeader(cs.currentBlockHeight - 3)
	}
	cs.headerCache.AddHeaderToCache(b.Header)

	return nil
}

func (cs *ChainStore) GetCurrentBlockHash() Uint256 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	return cs.currentBlockHash
}

func (cs *ChainStore) GetHeight() uint32 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	return cs.currentBlockHeight
}

func (cs *ChainStore) AddHeader(header *Header) error {
	cs.headerCache.AddHeaderToCache(header)

	return nil
}

func (cs *ChainStore) GetHeaderHeight() uint32 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	return cs.headerCache.GetCurrentCachedHeight()
}

func (cs *ChainStore) GetCurrentHeaderHash() Uint256 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	return cs.headerCache.GetCurrentCacheHeaderHash()
}

func (cs *ChainStore) GetHeaderHashByHeight(height uint32) Uint256 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	return cs.headerCache.GetCachedHeaderHashByHeight(height)
}

func (cs *ChainStore) GetHeaderWithCache(hash Uint256) (*Header, error) {
	return cs.headerCache.GetCachedHeader(hash)
}

func (cs *ChainStore) getHeaderWithCache(hash Uint256) (*Header, error) {
	return cs.headerCache.GetCachedHeader(hash)
}

func (cs *ChainStore) IsDoubleSpend(tx *Transaction) bool {
	return false
}

func (cs *ChainStore) getCurrentBlockHashFromDB() (Uint256, uint32, error) {
	data, err := cs.st.Get(currentBlockHashKey())
	if err != nil {
		return EmptyUint256, 0, err
	}

	var blockHash Uint256
	r := bytes.NewReader(data)
	blockHash.Deserialize(r)
	currentHeight, err := serialization.ReadUint32(r)
	return blockHash, currentHeight, err
}

func (cs *ChainStore) GetCurrentBlockStateRoot() Uint256 {
	currentState, _ := cs.st.Get(currentStateTrie())
	hash, _ := Uint256ParseFromBytes(currentState)

	return hash
}

func (cs *ChainStore) GetDatabase() IStore {
	return cs.st
}

func (cs *ChainStore) GetStateRootHash() Uint256 {
	return cs.States.IntermediateRoot(false)
}

func (cs *ChainStore) GetBalance(addr Uint160) Fixed64 {
	return cs.States.GetBalance(addr)
}

func (cs *ChainStore) GetNonce(addr Uint160) uint64 {
	return cs.States.GetNonce(addr)
}
