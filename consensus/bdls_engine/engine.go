// Copyright 2020 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package bdls_engine

import (
	"bytes"
	"crypto/ecdsa"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/yonggewang/BDLSChain/accounts"
	"github.com/yonggewang/BDLSChain/common"
	"github.com/yonggewang/BDLSChain/consensus"
	"github.com/yonggewang/BDLSChain/consensus/bdls_engine/committee"
	"github.com/yonggewang/BDLSChain/core/state"
	"github.com/yonggewang/BDLSChain/core/types"
	"github.com/yonggewang/BDLSChain/crypto"
	"github.com/yonggewang/BDLSChain/ethdb"
	"github.com/yonggewang/BDLSChain/event"
	"github.com/yonggewang/BDLSChain/log"
	"github.com/yonggewang/BDLSChain/rpc"
	"github.com/yonggewang/bdls"
)

const (
	// minimum difference between two consecutive block's timestamps in second
	minBlockPeriod = 3
)

// Message exchange between consensus engine & protocol manager
type (
	// protocol manager will subscribe and broadcast this type of message
	MessageOutput []byte
	// protocol manager will deliver the incoming consensus message via this type to this engine
	MessageInput []byte
)

var (
	// errUnknownBlock is returned when the list of validators is requested for a block
	// that is not part of the local blockchain.
	errUnknownBlock = errors.New("unknown block")
	// errInvalidDifficulty is returned if the difficulty of a block is not 1
	errInvalidDifficulty = errors.New("invalid difficulty")
	// errInvalidW
	errInvalidW = errors.New("invalid W")
	// errInvalidSignature
	errInvalidSignature = errors.New("invalid proposer signature in block header")
	// errInvalidUncleHash is returned if a block contains an non-empty uncle list.
	errInvalidUncleHash = errors.New("non empty uncle hash")
	// errInvalidNonce is returned if a block's nonce is invalid
	errInvalidNonce = errors.New("invalid nonce")
	// errNonEmptyDecision is returned if a block's decision field is not empty
	errNonEmptyDecision = errors.New("non-empty decision field in proposal")
	// errEmptyDecision is returned if a block's decision field is empty
	errEmptyDecision = errors.New("empty decision field")
	// invalid input consensus message
	errInvalidConsensusMessage = errors.New("invalid input consensus message")
	// errInvalidTimestamp is returned if the timestamp of a block is lower than the previous block's timestamp + the minimum block period.
	errInvalidTimestamp = errors.New("invalid timestamp")
)

var (
	defaultDifficulty = big.NewInt(1)            // difficulty in block headers is always 1
	nilUncleHash      = types.CalcUncleHash(nil) // Always Keccak256(RLP([])) as uncles are meaningless outside of PoW.
	emptyNonce        = types.BlockNonce{}       // nonce in block headers is always all-zeros
)

// PublicKey to Identity conversion, for use in BDLS
func PubKeyToIdentity(pubkey *ecdsa.PublicKey) (ret bdls.Identity) {
	// for a publickey first we convert to ethereum common.Address
	commonAddress := crypto.PubkeyToAddress(*pubkey)
	// then we just byte copy to Identiy struct
	copy(ret[:], commonAddress[:])
	return
}

// BDLSEngine implements BDLS-based blockchain consensus engine
type BDLSEngine struct {
	// a nonce for message
	nonce uint32
	// ephermal private key for header verification
	ephermalKey *ecdsa.PrivateKey
	// private key for consensus signing
	privKey     *ecdsa.PrivateKey
	privKeyMu   sync.Mutex
	privKeyOnce sync.Once

	// event mux to exchange consensus message with protocol manager
	mux *event.TypeMux

	// the account manager to get private key as a participant
	accountManager *accounts.Manager

	// as the block will be exchanged via <roundchange> message,
	// we need to validate these blocks in-flight, so we need processBlock at given height with state,
	// and compare the results with related fields in block header.
	stateAt       func(hash common.Hash) (*state.StateDB, error)
	hasBadBlock   func(hash common.Hash) bool
	processBlock  func(block *types.Block, statedb *state.StateDB) (types.Receipts, []*types.Log, uint64, error)
	validateState func(block *types.Block, statedb *state.StateDB, receipts types.Receipts, usedGas uint64) error
}

// New creates a ethereum compatible BDLS engine with account manager for signing and mux for
// message exchanging
func New(accountManager *accounts.Manager, mux *event.TypeMux, db ethdb.Database) *BDLSEngine {
	engine := new(BDLSEngine)
	engine.mux = mux
	engine.accountManager = accountManager

	// create an ephermal key for verification
	priv, err := crypto.GenerateKey()
	if err != nil {
		log.Crit("BDLS generate ephermal key", "crypto.GenerateKey", err)
	}
	engine.ephermalKey = priv
	return engine
}

// SetBlockValidator starts the validating engine, this will be set by miner while starting.
func (e *BDLSEngine) SetBlockValidator(hasBadBlock func(common.Hash) bool,
	processBlock func(*types.Block, *state.StateDB) (types.Receipts, []*types.Log, uint64, error),
	validateState func(*types.Block, *state.StateDB, types.Receipts, uint64) error,
	stateAt func(hash common.Hash) (*state.StateDB, error)) {

	e.hasBadBlock = hasBadBlock
	e.processBlock = processBlock
	e.validateState = validateState
	e.stateAt = stateAt
}

// Author retrieves the Ethereum address of the account that minted the given
// block, which may be different from the header's coinbase if a consensus
// engine is based on signatures.
func (e *BDLSEngine) Author(header *types.Header) (common.Address, error) {
	return header.Coinbase, nil
}

// VerifyHeader checks whether a header conforms to the consensus rules of a
// given engine. Verifying the seal may be done optionally here, or explicitly
// via the VerifySeal method.
func (e *BDLSEngine) VerifyHeader(chain consensus.ChainReader, header *types.Header, seal bool) error {
	err := e.verifyHeader(chain, header, nil)
	if err != nil {
		return err
	}
	return nil
}

// verifyHeader checks whether a header conforms to the consensus rules.The
// caller may optionally pass in a batch of parents (ascending order) to avoid
// looking those up from the database. This is useful for concurrently verifying
// a batch of new headers.
func (e *BDLSEngine) verifyHeader(chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	// Ensure the block's parent exist
	number := header.Number.Uint64()
	if chain.GetHeader(header.Hash(), number) != nil {
		return nil
	}
	// Ensure that the nonce is empty
	if header.Nonce != (emptyNonce) {
		return errInvalidNonce
	}
	// Ensure that the block doesn't contain any uncles which are meaningless in BDLS
	if header.UncleHash != nilUncleHash {
		return errInvalidUncleHash
	}

	// Ensure that the block's difficulty is 1
	if header.Difficulty == nil || header.Difficulty.Cmp(defaultDifficulty) != 0 {
		return errInvalidDifficulty
	}

	// Ensure that the block's timestamp isn't too close to it's parent
	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}
	if parent == nil || parent.Number.Uint64() != number-1 || parent.Hash() != header.ParentHash {
		return consensus.ErrUnknownAncestor
	}
	if parent.Time+minBlockPeriod > header.Time {
		return errInvalidTimestamp
	}

	// Ensure W has correctly set
	if committee.DeriveW(parent) != header.W {
		return errInvalidW
	}

	// Ensure the signature is not empty
	if len(header.Signature) != crypto.SignatureLength {
		return errInvalidSignature
	}

	return nil
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
// concurrently. The method returns a quit channel to abort the operations and
// a results channel to retrieve the async verifications (the order is that of
// the input slice).
func (e *BDLSEngine) VerifyHeaders(chain consensus.ChainReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	abort, results := make(chan struct{}), make(chan error, len(headers))
	go func() {
		for i, header := range headers {
			err := e.verifyHeader(chain, header, headers[:i])

			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()

	return abort, results
}

// VerifyUncles verifies that the given block's uncles conform to the consensus
// rules of a given engine.
func (e *BDLSEngine) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errInvalidUncleHash
	}
	return nil
}

// VerifySeal checks whether the crypto seal on a header is valid according to
// the consensus rules of the given engine.
// Fields to verify:
// 	- Signature
// 	- Decision
//	- R
func (e *BDLSEngine) VerifySeal(chain consensus.ChainReader, header *types.Header) error {
	// The genesis block is the always valid dead-end
	number := header.Number.Uint64()
	if number == 0 {
		return nil
	}

	// Ensure the parent is not nil
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}

	// retrieve the state at parent height
	parentState, err := e.stateAt(header.ParentHash)
	if err != nil {
		return errors.New("VerifySeal - Error in getting the block's parent's state")
	}

	// Ensure it's a valid proposer(header.Signature & header.R field)
	if !e.verifyProposerField(header, parentState) {
		return errors.New("VerifySeal - verifyProposerField failed")
	}

	// Ensure the proof field is not nil
	if len(header.Decision) == 0 {
		return errEmptyDecision
	}

	// Get the SealHash of this header to verify against
	sealHash := e.SealHash(header).Bytes()

	// create a consensus config to validate this message at the correct height
	config := &bdls.Config{
		Epoch:            time.Now(),
		PrivateKey:       e.ephermalKey,
		StateCompare:     func(a bdls.State, b bdls.State) int { return bytes.Compare(a, b) },
		StateValidate:    func(bdls.State) bool { return true },
		CurrentHeight:    header.Number.Uint64() - 1,
		PubKeyToIdentity: PubKeyToIdentity,
	}
	// create the consensus object along with participants to validate decide message
	config.Participants = committee.CreateValidators(header, parentState)

	consensus, err := bdls.NewConsensus(config)
	if err != nil {
		log.Error("VerifySeal", "bdls.NewConsensus", err)
		return err
	}

	// Ensure the block has a validate decide message(header.Decision field)
	err = consensus.ValidateDecideMessage(header.Decision, sealHash)
	if err != nil {
		log.Debug("VerifySeal", "consensus..ValidateDecideMessage", err)
		return err
	}

	return nil
}

// Prepare initializes the fields of a block header according to the
// rules of a particular engine. The changes are executed inline.
func (e *BDLSEngine) Prepare(chain consensus.ChainReader, header *types.Header) error {
	// unused fields, force to set to empty
	header.Nonce = emptyNonce
	// use the same difficulty for all blocks
	header.Difficulty = defaultDifficulty
	// check parent
	number := header.Number.Uint64()
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}

	// set header's timestamp(unix time) to at least minBlockPeriod since last block
	header.Time = parent.Time + minBlockPeriod
	if header.Time < uint64(time.Now().Unix()) {
		header.Time = uint64(time.Now().Unix())
	}

	// set W based on parent block
	header.W = committee.DeriveW(parent)
	return nil
}

// Finalize runs any post-transaction state modifications (e.g. block rewards)
// but does not assemble the block.
//
// Note: The block header and state database might be updated to reflect any
// consensus rules that happen at finalization (e.g. block rewards).
func (e *BDLSEngine) Finalize(chain consensus.ChainReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header) {
	e.accumulateRewards(chain, state, header)
	header.Root = state.IntermediateRoot(true)
}

// FinalizeAndAssemble runs any post-transaction state modifications (e.g. block
// rewards) and assembles the final block.
//
// Note: The block header and state database might be updated to reflect any
// consensus rules that happen at finalization (e.g. block rewards).
func (e *BDLSEngine) FinalizeAndAssemble(chain consensus.ChainReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	e.accumulateRewards(chain, state, header)
	header.Root = state.IntermediateRoot(true)
	return types.NewBlock(header, txs, nil, receipts), nil
}

// Seal generates a new sealing request for the given input block and pushes
// the result into the given channel.
//
// Note, the method returns immediately and will send the result async. More
// than one result may also be returned depending on the consensus algorithm.
func (e *BDLSEngine) Seal(chain consensus.ChainReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	go e.consensusTask(chain, block, results, stop)
	return nil
}

// waitForPrivateKey gets private key from account manager
func (e *BDLSEngine) waitForPrivateKey(coinbase common.Address, stop <-chan struct{}) *ecdsa.PrivateKey {
	e.privKeyMu.Lock()
	privKey := e.privKey
	e.privKeyMu.Unlock()

	if privKey != nil {
		return privKey
	}

	for {
		select {
		case <-stop:
			return nil
		default:
			log.Debug("looking for the wallet of coinbase:", "coinbase", coinbase)
			wallet, err := e.accountManager.Find(accounts.Account{Address: coinbase})
			if err != nil {
				log.Debug("cannot find the wallet of coinbase", "coinbase", coinbase)
				return nil
			}

			priv, err := wallet.GetPrivateKey(accounts.Account{Address: coinbase})
			if err != nil {
				<-time.After(time.Second) // wait for a second before retry
				continue
			}

			e.privKeyMu.Lock()
			e.privKey = priv
			e.privKeyMu.Unlock()
			return priv
		}
	}
}

// SealHash returns the hash of a block prior to it being sealed.
func (e *BDLSEngine) SealHash(header *types.Header) (hash common.Hash) {
	copied := types.CopyHeader(header)
	copied.Decision = nil
	copied.Signature = nil
	copied.R = common.Hash{}
	return copied.Hash()
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have.
func (e *BDLSEngine) CalcDifficulty(chain consensus.ChainReader, time uint64, parent *types.Header) *big.Int {
	return defaultDifficulty
}

// APIs returns the RPC APIs this consensus engine provides.
func (e *BDLSEngine) APIs(chain consensus.ChainReader) []rpc.API {
	return []rpc.API{{
		Namespace: "bdls",
		Version:   "1.0",
		Service:   &API{chain: chain, engine: e},
		Public:    true,
	}}
}

// Close terminates any background threads maintained by the consensus engine.
func (e *BDLSEngine) Close() error { return nil }
