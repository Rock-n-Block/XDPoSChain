// Copyright 2014 The go-ethereum Authors
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

package types

import (
	"bytes"
	"container/heap"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/common/hexutil"
	"github.com/XinFinOrg/XDPoSChain/crypto"
	"github.com/XinFinOrg/XDPoSChain/rlp"
)

//go:generate gencodec -type txdata -field-override txdataMarshaling -out gen_tx_json.go

var (
	ErrInvalidSig               = errors.New("invalid transaction v, r, s values")
	errNoSigner                 = errors.New("missing signing methods")
	skipNonceDestinationAddress = map[string]bool{
		common.XDCXAddr:                         true,
		common.TradingStateAddr:                 true,
		common.XDCXLendingAddress:               true,
		common.XDCXLendingFinalizedTradeAddress: true,
	}
	ErrUnexpectedProtection = errors.New("transaction type does not supported EIP-155 protected signatures")
	ErrInvalidTxType        = errors.New("transaction type not valid in this context")
	ErrTxTypeNotSupported   = errors.New("transaction type not supported")
	errEmptyTypedTx         = errors.New("empty typed transaction bytes")
)

// Transaction types.
const (
	LegacyTxType = iota
	AccessListTxType
	DynamicFeeTxType
)

// deriveSigner makes a *best* guess about which signer to use.
func deriveSigner(V *big.Int) Signer {
	if V.Sign() != 0 && isProtectedV(V) {
		return NewEIP155Signer(deriveChainId(V))
	} else {
		return HomesteadSigner{}
	}
}

type Transaction struct {
	inner TxData
	time  time.Time // Time first seen locally (spam avoidance)
	// caches
	hash atomic.Value
	size atomic.Value
	from atomic.Value
}

// NewTx creates a new transaction.
func NewTx(inner TxData) *Transaction {
	tx := new(Transaction)
	tx.setDecoded(inner.copy(), 0)
	return tx
}

// TxData is the underlying data of a transaction.
//
// This is implemented by LegacyTx and DynamicTx.
type TxData interface {
	txType() byte // returns the type ID
	copy() TxData // creates a deep copy and initializes all fields

	chainID() *big.Int
	accessList() AccessList
	data() []byte
	gas() uint64
	gasPrice() *big.Int
	tip() *big.Int
	feeCap() *big.Int
	value() *big.Int
	nonce() uint64
	to() *common.Address

	rawSignatureValues() (v, r, s *big.Int)
	setSignatureValues(chainID, v, r, s *big.Int)
}

// EncodeRLP implements rlp.Encoder
func (tx *Transaction) EncodeRLP(w io.Writer) error {
	if tx.Type() == LegacyTxType {
		return rlp.Encode(w, tx.inner)
	}
	// It's an EIP-2718 typed TX envelope.
	buf := encodeBufferPool.Get().(*bytes.Buffer)
	defer encodeBufferPool.Put(buf)
	buf.Reset()
	if err := tx.encodeTyped(buf); err != nil {
		return err
	}
	return rlp.Encode(w, buf.Bytes())
}

// encodeTyped writes the canonical encoding of a typed transaction to w.
func (tx *Transaction) encodeTyped(w *bytes.Buffer) error {
	w.WriteByte(tx.Type())
	return rlp.Encode(w, tx.inner)
}

// MarshalBinary returns the canonical encoding of the transaction.
// For legacy transactions, it returns the RLP encoding. For EIP-2718 typed
// transactions, it returns the type and payload.
func (tx *Transaction) MarshalBinary() ([]byte, error) {
	if tx.Type() == LegacyTxType {
		return rlp.EncodeToBytes(tx.inner)
	}
	var buf bytes.Buffer
	err := tx.encodeTyped(&buf)
	return buf.Bytes(), err
}

// DecodeRLP implements rlp.Decoder
func (tx *Transaction) DecodeRLP(s *rlp.Stream) error {
	kind, size, err := s.Kind()
	switch {
	case err != nil:
		return err
	case kind == rlp.List:
		// It's a legacy transaction.
		var inner LegacyTx
		err := s.Decode(&inner)
		if err == nil {
			tx.setDecoded(&inner, int(rlp.ListSize(size)))
		}
		return err
	case kind == rlp.String:
		// It's an EIP-2718 typed TX envelope.
		var b []byte
		if b, err = s.Bytes(); err != nil {
			return err
		}
		inner, err := tx.decodeTyped(b)
		if err == nil {
			tx.setDecoded(inner, len(b))
		}
		return err
	default:
		return rlp.ErrExpectedList
	}
}

// UnmarshalBinary decodes the canonical encoding of transactions.
// It supports legacy RLP transactions and EIP2718 typed transactions.
func (tx *Transaction) UnmarshalBinary(b []byte) error {
	if len(b) > 0 && b[0] > 0x7f {
		// It's a legacy transaction.
		var data LegacyTx
		err := rlp.DecodeBytes(b, &data)
		if err != nil {
			return err
		}
		tx.setDecoded(&data, len(b))
		return nil
	}
	// It's an EIP2718 typed transaction envelope.
	inner, err := tx.decodeTyped(b)
	if err != nil {
		return err
	}
	tx.setDecoded(inner, len(b))
	return nil
}

// decodeTyped decodes a typed transaction from the canonical format.
func (tx *Transaction) decodeTyped(b []byte) (TxData, error) {
	if len(b) == 0 {
		return nil, errEmptyTypedTx
	}
	switch b[0] {
	case AccessListTxType:
		var inner AccessListTx
		err := rlp.DecodeBytes(b[1:], &inner)
		return &inner, err
	case DynamicFeeTxType:
		var inner DynamicFeeTx
		err := rlp.DecodeBytes(b[1:], &inner)
		return &inner, err
	default:
		return nil, ErrTxTypeNotSupported
	}
}

// setDecoded sets the inner transaction and size after decoding.
func (tx *Transaction) setDecoded(inner TxData, size int) {
	tx.inner = inner
	tx.time = time.Now()
	if size > 0 {
		tx.size.Store(common.StorageSize(size))
	}
}

func sanityCheckSignature(v *big.Int, r *big.Int, s *big.Int, maybeProtected bool) error {
	if isProtectedV(v) && !maybeProtected {
		return ErrUnexpectedProtection
	}

	var plainV byte
	if isProtectedV(v) {
		chainID := deriveChainId(v).Uint64()
		plainV = byte(v.Uint64() - 35 - 2*chainID)
	} else if maybeProtected {
		// Only EIP-155 signatures can be optionally protected. Since
		// we determined this v value is not protected, it must be a
		// raw 27 or 28.
		plainV = byte(v.Uint64() - 27)
	} else {
		// If the signature is not optionally protected, we assume it
		// must already be equal to the recovery id.
		plainV = byte(v.Uint64())
	}
	if !crypto.ValidateSignatureValues(plainV, r, s, false) {
		return ErrInvalidSig
	}

	return nil
}

func isProtectedV(V *big.Int) bool {
	if V.BitLen() <= 8 {
		v := V.Uint64()
		return v != 27 && v != 28 && v != 1 && v != 0
	}
	// anything not 27 or 28 is considered protected
	return true
}

// Protected says whether the transaction is replay-protected.
func (tx *Transaction) Protected() bool {
	switch tx := tx.inner.(type) {
	case *LegacyTx:
		return tx.V != nil && isProtectedV(tx.V)
	default:
		return true
	}
}

// Type returns the transaction type.
func (tx *Transaction) Type() uint8 {
	return tx.inner.txType()
}

// ChainId returns the EIP155 chain ID of the transaction. The return value will always be
// non-nil. For legacy transactions which are not replay-protected, the return value is
// zero.
func (tx *Transaction) ChainId() *big.Int {
	return tx.inner.chainID()
}

// Data returns the input data of the transaction.
func (tx *Transaction) Data() []byte { return tx.inner.data() }

// AccessList returns the access list of the transaction.
func (tx *Transaction) AccessList() AccessList { return tx.inner.accessList() }

// Gas returns the gas limit of the transaction.
func (tx *Transaction) Gas() uint64 { return tx.inner.gas() }

// GasPrice returns the gas price of the transaction.
func (tx *Transaction) GasPrice() *big.Int { return new(big.Int).Set(tx.inner.gasPrice()) }

// Tip returns the tip per gas of the transaction.
func (tx *Transaction) Tip() *big.Int { return new(big.Int).Set(tx.inner.tip()) }

// FeeCap returns the fee cap per gas of the transaction.
func (tx *Transaction) FeeCap() *big.Int { return new(big.Int).Set(tx.inner.feeCap()) }

// Value returns the ether amount of the transaction.
func (tx *Transaction) Value() *big.Int { return new(big.Int).Set(tx.inner.value()) }

// Nonce returns the sender account nonce of the transaction.
func (tx *Transaction) Nonce() uint64 { return tx.inner.nonce() }

// To returns the recipient address of the transaction.
// For contract-creation transactions, To returns nil.
func (tx *Transaction) To() *common.Address {
	// Copy the pointed-to address.
	ito := tx.inner.to()
	if ito == nil {
		return nil
	}
	cpy := *ito
	return &cpy
}

// Cost returns gas * gasPrice + value.
func (tx *Transaction) Cost() *big.Int {
	total := new(big.Int).Mul(tx.GasPrice(), new(big.Int).SetUint64(tx.Gas()))
	total.Add(total, tx.Value())
	return total
}

// RawSignatureValues returns the V, R, S signature values of the transaction.
// The return values should not be modified by the caller.
func (tx *Transaction) RawSignatureValues() (v, r, s *big.Int) {
	return tx.inner.rawSignatureValues()
}

// GasPriceCmp compares the gas prices of two transactions.
func (tx *Transaction) GasPriceCmp(other *Transaction) int {
	return tx.inner.gasPrice().Cmp(other.inner.gasPrice())
}

// GasPriceIntCmp compares the gas price of the transaction against the given price.
func (tx *Transaction) GasPriceIntCmp(other *big.Int) int {
	return tx.inner.gasPrice().Cmp(other)
}

// Hash returns the transaction hash.
func (tx *Transaction) Hash() common.Hash {
	if hash := tx.hash.Load(); hash != nil {
		return hash.(common.Hash)
	}

	var h common.Hash
	if tx.Type() == LegacyTxType {
		h = rlpHash(tx.inner)
	} else {
		h = prefixedRlpHash(tx.Type(), tx.inner)
	}
	tx.hash.Store(h)
	return h
}

// Size returns the true RLP encoded storage size of the transaction, either by
// encoding and returning it, or returning a previously cached value.
func (tx *Transaction) Size() common.StorageSize {
	if size := tx.size.Load(); size != nil {
		return size.(common.StorageSize)
	}
	c := writeCounter(0)
	rlp.Encode(&c, &tx.inner)
	tx.size.Store(common.StorageSize(c))
	return common.StorageSize(c)
}

// WithSignature returns a new transaction with the given signature.
// This signature needs to be in the [R || S || V] format where V is 0 or 1.
func (tx *Transaction) WithSignature(signer Signer, sig []byte) (*Transaction, error) {
	r, s, v, err := signer.SignatureValues(tx, sig)
	if err != nil {
		return nil, err
	}
	cpy := tx.inner.copy()
	cpy.setSignatureValues(signer.ChainID(), v, r, s)
	return &Transaction{inner: cpy, time: tx.time}, nil
}

type TxDataMarshaling struct {
	AccountNonce hexutil.Uint64
	Price        *hexutil.Big
	GasLimit     hexutil.Uint64
	FeeCap       *hexutil.Big
	Tip          *hexutil.Big
	Amount       *hexutil.Big
	Payload      hexutil.Bytes
	V            *hexutil.Big
	R            *hexutil.Big
	S            *hexutil.Big
}

func (tx *Transaction) CheckNonce() bool { return true }

func (tx *Transaction) From() *common.Address {
	v, _, _ := tx.RawSignatureValues()
	if v != nil {
		signer := deriveSigner(v)
		if f, err := Sender(signer, tx); err != nil {
			return nil
		} else {
			return &f
		}
	} else {
		return nil
	}
}

func (tx *Transaction) CacheHash() {
	v := rlpHash(tx)
	tx.hash.Store(v)
}

// Cost returns amount + gasprice * gaslimit.
func (tx *Transaction) TRC21Cost() *big.Int {
	total := new(big.Int).Mul(common.TRC21GasPrice, new(big.Int).SetUint64(tx.inner.gas()))
	total.Add(total, tx.inner.value())
	return total
}

// TODO is not working correctly
func (tx *Transaction) IsSpecialTransaction() bool {
	if tx.To() == nil {
		return false
	}
	toBytes := tx.To().Bytes()
	randomizeSMCBytes := common.HexToAddress(common.RandomizeSMC).Bytes()
	blockSignersBytes := common.HexToAddress(common.BlockSigners).Bytes()
	return bytes.Equal(toBytes, randomizeSMCBytes) || bytes.Equal(toBytes, blockSignersBytes)
}

func (tx *Transaction) IsTradingTransaction() bool {
	if tx.To() == nil {
		return false
	}

	if tx.To().String() != common.XDCXAddr {
		return false
	}

	return true
}

func (tx *Transaction) IsLendingTransaction() bool {
	if tx.To() == nil {
		return false
	}

	if tx.To().String() != common.XDCXLendingAddress {
		return false
	}
	return true
}

func (tx *Transaction) IsLendingFinalizedTradeTransaction() bool {
	if tx.To() == nil {
		return false
	}

	if tx.To().String() != common.XDCXLendingFinalizedTradeAddress {
		return false
	}
	return true
}

func (tx *Transaction) IsSkipNonceTransaction() bool {
	if tx.To() == nil {
		return false
	}
	if skip := skipNonceDestinationAddress[tx.To().String()]; skip {
		return true
	}
	return false
}

func (tx *Transaction) IsSigningTransaction() bool {
	if tx.To() == nil {
		return false
	}

	if tx.To().String() != common.BlockSigners {
		return false
	}

	method := common.ToHex(tx.Data()[0:4])

	if method != common.SignMethod {
		return false
	}

	if len(tx.Data()) != (32*2 + 4) {
		return false
	}

	return true
}

func (tx *Transaction) IsVotingTransaction() (bool, *common.Address) {
	if tx.To() == nil {
		return false, nil
	}
	b := (tx.To().String() == common.MasternodeVotingSMC)

	if !b {
		return b, nil
	}

	method := common.ToHex(tx.Data()[0:4])
	if b = (method == common.VoteMethod); b {
		addr := tx.Data()[len(tx.Data())-20:]
		m := common.BytesToAddress(addr)
		return b, &m
	}

	if b = (method == common.UnvoteMethod); b {
		addr := tx.Data()[len(tx.Data())-32-20 : len(tx.Data())-32]
		m := common.BytesToAddress(addr)
		return b, &m
	}

	if b = (method == common.ProposeMethod); b {
		addr := tx.Data()[len(tx.Data())-20:]
		m := common.BytesToAddress(addr)
		return b, &m
	}

	if b = (method == common.ResignMethod); b {
		addr := tx.Data()[len(tx.Data())-20:]
		m := common.BytesToAddress(addr)
		return b, &m
	}

	return b, nil
}

func (tx *Transaction) IsXDCXApplyTransaction() bool {
	if tx.To() == nil {
		return false
	}

	addr := common.XDCXListingSMC
	if common.IsTestnet {
		addr = common.XDCXListingSMCTestNet
	}
	if tx.To().String() != addr.String() {
		return false
	}

	method := common.ToHex(tx.Data()[0:4])

	if method != common.XDCXApplyMethod {
		return false
	}

	// 4 bytes for function name
	// 32 bytes for 1 parameter
	if len(tx.Data()) != (32 + 4) {
		return false
	}
	return true
}

func (tx *Transaction) IsXDCZApplyTransaction() bool {
	if tx.To() == nil {
		return false
	}

	addr := common.TRC21IssuerSMC
	if common.IsTestnet {
		addr = common.TRC21IssuerSMCTestNet
	}
	if tx.To().String() != addr.String() {
		return false
	}

	method := common.ToHex(tx.Data()[0:4])
	if method != common.XDCZApplyMethod {
		return false
	}

	// 4 bytes for function name
	// 32 bytes for 1 parameter
	if len(tx.Data()) != (32 + 4) {
		return false
	}

	return true
}

func (tx *Transaction) String() string {
	var from, to string
	v, r, s := tx.RawSignatureValues()
	if v != nil {
		// make a best guess about the signer and use that to derive
		// the sender.
		signer := deriveSigner(v)
		if f, err := Sender(signer, tx); err != nil { // derive but don't cache
			from = "[invalid sender: invalid sig]"
		} else {
			from = fmt.Sprintf("%x", f[:])
		}
	} else {
		from = "[invalid sender: nil V field]"
	}

	if tx.inner.to() == nil {
		to = "[contract creation]"
	} else {
		to = fmt.Sprintf("%x", tx.inner.to()[:])
	}
	enc, _ := rlp.EncodeToBytes(&tx.inner)
	return fmt.Sprintf(`
	TX(%x)
	Contract: %v
	From:     %s
	To:       %s
	Nonce:    %v
	GasPrice: %#x
	GasLimit  %#x
	Value:    %#x
	Data:     0x%x
	V:        %#x
	R:        %#x
	S:        %#x
	Hex:      %x
`,
		tx.Hash(),
		tx.inner.to() == nil,
		from,
		to,
		tx.inner.nonce(),
		tx.inner.gasPrice(),
		tx.inner.gas(),
		tx.inner.value(),
		tx.inner.data(),
		v,
		r,
		s,
		enc,
	)
}

// Transactions is a Transaction slice type for basic sorting.
type Transactions []*Transaction

// Len returns the length of s.
func (s Transactions) Len() int { return len(s) }

// EncodeIndex encodes the i'th transaction to w. Note that this does not check for errors
// because we assume that *Transaction will only ever contain valid txs that were either
// constructed by decoding or via public API in this package.
func (s Transactions) EncodeIndex(i int, w *bytes.Buffer) {
	tx := s[i]
	if tx.Type() == LegacyTxType {
		rlp.Encode(w, tx.inner)
	} else {
		tx.encodeTyped(w)
	}
}

// Swap swaps the i'th and the j'th element in s.
func (s Transactions) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

// GetRlp implements Rlpable and returns the i'th element of s in rlp.
func (s Transactions) GetRlp(i int) []byte {
	enc, _ := rlp.EncodeToBytes(s[i])
	return enc
}

// TxDifference returns a new set t which is the difference between a to b.
func TxDifference(a, b Transactions) (keep Transactions) {
	keep = make(Transactions, 0, len(a))

	remove := make(map[common.Hash]struct{})
	for _, tx := range b {
		remove[tx.Hash()] = struct{}{}
	}

	for _, tx := range a {
		if _, ok := remove[tx.Hash()]; !ok {
			keep = append(keep, tx)
		}
	}

	return keep
}

// TxByNonce implements the sort interface to allow sorting a list of transactions
// by their nonces. This is usually only useful for sorting transactions from a
// single account, otherwise a nonce comparison doesn't make much sense.
type TxByNonce Transactions

func (s TxByNonce) Len() int           { return len(s) }
func (s TxByNonce) Less(i, j int) bool { return s[i].inner.nonce() < s[j].inner.nonce() }
func (s TxByNonce) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// TxByPriceAndTime implements both the sort and the heap interface, making it useful
// for all at once sorting as well as individually adding and removing elements.
type TxByPriceAndTime Transactions

func (s TxByPriceAndTime) Len() int { return len(s) }
func (s TxByPriceAndTime) Less(i, j int) bool {
	// If the prices are equal, use the time the transaction was first seen for
	// deterministic sorting
	cmp := s[i].GasPrice().Cmp(s[j].GasPrice())
	if cmp == 0 {
		return s[i].time.Before(s[j].time)
	}
	return cmp > 0
}
func (s TxByPriceAndTime) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

func (s *TxByPriceAndTime) Push(x interface{}) {
	*s = append(*s, x.(*Transaction))
}

func (s *TxByPriceAndTime) Pop() interface{} {
	old := *s
	n := len(old)
	x := old[n-1]
	*s = old[0 : n-1]
	return x
}

// TxByPrice implements both the sort and the heap interface, making it useful
// for all at once sorting as well as individually adding and removing elements.
type TxByPrice struct {
	txs        Transactions
	payersSwap map[common.Address]*big.Int
}

func (s TxByPrice) Len() int { return len(s.txs) }
func (s TxByPrice) Less(i, j int) bool {
	i_price := s.txs[i].inner.gasPrice()
	if s.txs[i].To() != nil {
		if _, ok := s.payersSwap[*s.txs[i].To()]; ok {
			i_price = common.TRC21GasPrice
		}
	}

	j_price := s.txs[j].inner.gasPrice()
	if s.txs[j].To() != nil {
		if _, ok := s.payersSwap[*s.txs[j].To()]; ok {
			j_price = common.TRC21GasPrice
		}
	}
	return i_price.Cmp(j_price) > 0
}
func (s TxByPrice) Swap(i, j int) { s.txs[i], s.txs[j] = s.txs[j], s.txs[i] }

func (s *TxByPrice) Push(x interface{}) {
	s.txs = append(s.txs, x.(*Transaction))
}

func (s *TxByPrice) Pop() interface{} {
	old := s.txs
	n := len(old)
	x := old[n-1]
	s.txs = old[0 : n-1]
	return x
}

// TransactionsByPriceAndNonce represents a set of transactions that can return
// transactions in a profit-maximizing sorted order, while supporting removing
// entire batches of transactions for non-executable accounts.
type TransactionsByPriceAndNonce struct {
	txs    map[common.Address]Transactions // Per account nonce-sorted list of transactions
	heads  TxByPrice                       // Next transaction for each unique account (price heap)
	signer Signer                          // Signer for the set of transactions
}

// NewTransactionsByPriceAndNonce creates a transaction set that can retrieve
// price sorted transactions in a nonce-honouring way.
//
// Note, the input map is reowned so the caller should not interact any more with
// if after providing it to the constructor.

// It also classifies special txs and normal txs
func NewTransactionsByPriceAndNonce(signer Signer, txs map[common.Address]Transactions, signers map[common.Address]struct{}, payersSwap map[common.Address]*big.Int) (*TransactionsByPriceAndNonce, Transactions) {
	// Initialize a price based heap with the head transactions
	heads := TxByPrice{}
	heads.payersSwap = payersSwap
	specialTxs := Transactions{}
	for _, accTxs := range txs {
		from, _ := Sender(signer, accTxs[0])
		var normalTxs Transactions
		lastSpecialTx := -1
		if len(signers) > 0 {
			if _, ok := signers[from]; ok {
				for i, tx := range accTxs {
					if tx.IsSpecialTransaction() {
						lastSpecialTx = i
					}
				}
			}
		}
		if lastSpecialTx >= 0 {
			for i := 0; i <= lastSpecialTx; i++ {
				specialTxs = append(specialTxs, accTxs[i])
			}
			normalTxs = accTxs[lastSpecialTx+1:]
		} else {
			normalTxs = accTxs
		}
		if len(normalTxs) > 0 {
			heads.txs = append(heads.txs, normalTxs[0])
			// Ensure the sender address is from the signer
			txs[from] = normalTxs[1:]
		}
	}
	heap.Init(&heads)

	// Assemble and return the transaction set
	return &TransactionsByPriceAndNonce{
		txs:    txs,
		heads:  heads,
		signer: signer,
	}, specialTxs
}

// Peek returns the next transaction by price.
func (t *TransactionsByPriceAndNonce) Peek() *Transaction {
	if len(t.heads.txs) == 0 {
		return nil
	}
	return t.heads.txs[0]
}

// Shift replaces the current best head with the next one from the same account.
func (t *TransactionsByPriceAndNonce) Shift() {
	acc, _ := Sender(t.signer, t.heads.txs[0])
	if txs, ok := t.txs[acc]; ok && len(txs) > 0 {
		t.heads.txs[0], t.txs[acc] = txs[0], txs[1:]
		heap.Fix(&t.heads, 0)
	} else {
		heap.Pop(&t.heads)
	}
}

// Pop removes the best transaction, *not* replacing it with the next one from
// the same account. This should be used when a transaction cannot be executed
// and hence all subsequent ones should be discarded from the same account.
func (t *TransactionsByPriceAndNonce) Pop() {
	heap.Pop(&t.heads)
}
