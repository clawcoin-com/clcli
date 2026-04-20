// Package evm provides an EVM JSON-RPC client sufficient for balance queries
// and sending legacy (EIP-155) native-token transfers on ClawCoin Testnet.
package evm

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/clawcoin-com/clcli/internal/keystore"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// Client is a thin JSON-RPC client.
type Client struct {
	URL     string
	ChainID *big.Int
	HTTP    *http.Client
}

// New returns a client pointing at the given RPC URL.
func New(rpcURL string, chainID int64) *Client {
	return &Client{
		URL:     rpcURL,
		ChainID: big.NewInt(chainID),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

type rpcReq struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      int           `json:"id"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) call(ctx context.Context, method string, params []interface{}, out interface{}) error {
	body, err := json.Marshal(rpcReq{JSONRPC: "2.0", Method: method, Params: params, ID: 1})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("rpc %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var r rpcResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("decode %s: %w (body=%s)", method, err, string(raw))
	}
	if r.Error != nil {
		return fmt.Errorf("rpc %s: %s (code %d)", method, r.Error.Message, r.Error.Code)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// GetBalance returns the native CC balance (wei) of the address.
func (c *Client) GetBalance(ctx context.Context, address string) (*big.Int, error) {
	var hexStr string
	if err := c.call(ctx, "eth_getBalance", []interface{}{address, "latest"}, &hexStr); err != nil {
		return nil, err
	}
	return hexToBigInt(hexStr), nil
}

// GetNonce returns the next transaction nonce for address (pending).
func (c *Client) GetNonce(ctx context.Context, address string) (uint64, error) {
	var hexStr string
	if err := c.call(ctx, "eth_getTransactionCount", []interface{}{address, "pending"}, &hexStr); err != nil {
		return 0, err
	}
	return hexToBigInt(hexStr).Uint64(), nil
}

// GasPrice returns the suggested gas price (wei).
func (c *Client) GasPrice(ctx context.Context) (*big.Int, error) {
	var hexStr string
	if err := c.call(ctx, "eth_gasPrice", nil, &hexStr); err != nil {
		return nil, err
	}
	return hexToBigInt(hexStr), nil
}

// BlockNumber returns the latest block number.
func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	var hexStr string
	if err := c.call(ctx, "eth_blockNumber", nil, &hexStr); err != nil {
		return 0, err
	}
	return hexToBigInt(hexStr).Uint64(), nil
}

// SendRawTransaction broadcasts a signed raw transaction and returns its hash.
func (c *Client) SendRawTransaction(ctx context.Context, rawHex string) (string, error) {
	var txHash string
	if err := c.call(ctx, "eth_sendRawTransaction", []interface{}{rawHex}, &txHash); err != nil {
		return "", err
	}
	return txHash, nil
}

// TxReceipt is a minimal subset of the eth_getTransactionReceipt response.
type TxReceipt struct {
	TransactionHash   string `json:"transactionHash"`
	BlockNumber       string `json:"blockNumber"`
	Status            string `json:"status"` // 0x1 success, 0x0 failure
	From              string `json:"from"`
	To                string `json:"to"`
	GasUsed           string `json:"gasUsed"`
	CumulativeGasUsed string `json:"cumulativeGasUsed"`
}

// GetReceipt fetches a transaction receipt (nil if pending).
func (c *Client) GetReceipt(ctx context.Context, txHash string) (*TxReceipt, error) {
	var r *TxReceipt
	if err := c.call(ctx, "eth_getTransactionReceipt", []interface{}{txHash}, &r); err != nil {
		return nil, err
	}
	return r, nil
}

// SendNativeTransfer builds, signs, and broadcasts an EIP-155 legacy transfer.
// amount is in wei.
func (c *Client) SendNativeTransfer(
	ctx context.Context,
	privBytes []byte,
	fromAddr, toAddr string,
	amount *big.Int,
	gasLimit uint64,
	gasPrice *big.Int,
) (string, error) {
	nonce, err := c.GetNonce(ctx, fromAddr)
	if err != nil {
		return "", fmt.Errorf("get nonce: %w", err)
	}

	to := strings.TrimPrefix(strings.ToLower(toAddr), "0x")
	toBytes, err := hex.DecodeString(to)
	if err != nil || len(toBytes) != 20 {
		return "", fmt.Errorf("invalid to address: %s", toAddr)
	}

	// Build RLP of unsigned tx per EIP-155:
	// [nonce, gasPrice, gasLimit, to, value, data, chainId, 0, 0]
	unsigned := rlpList(
		rlpBytes(bigUintBytes(new(big.Int).SetUint64(nonce))),
		rlpBytes(bigUintBytes(gasPrice)),
		rlpBytes(bigUintBytes(new(big.Int).SetUint64(gasLimit))),
		rlpBytes(toBytes),
		rlpBytes(bigUintBytes(amount)),
		rlpBytes(nil), // empty data
		rlpBytes(bigUintBytes(c.ChainID)),
		rlpBytes(nil),
		rlpBytes(nil),
	)
	sigHash := keystore.Keccak256(unsigned)

	priv := secp256k1.PrivKeyFromBytes(privBytes)
	compact := ecdsa.SignCompact(priv, sigHash, false) // [v(27/28), r, s]
	r := compact[1:33]
	s := compact[33:65]
	recID := compact[0] - 27

	// EIP-155: V = recID + chainID*2 + 35
	v := new(big.Int).Mul(c.ChainID, big.NewInt(2))
	v.Add(v, big.NewInt(35+int64(recID)))

	signed := rlpList(
		rlpBytes(bigUintBytes(new(big.Int).SetUint64(nonce))),
		rlpBytes(bigUintBytes(gasPrice)),
		rlpBytes(bigUintBytes(new(big.Int).SetUint64(gasLimit))),
		rlpBytes(toBytes),
		rlpBytes(bigUintBytes(amount)),
		rlpBytes(nil),
		rlpBytes(bigUintBytes(v)),
		rlpBytes(trimLeadingZeros(r)),
		rlpBytes(trimLeadingZeros(s)),
	)

	rawHex := "0x" + hex.EncodeToString(signed)
	return c.SendRawTransaction(ctx, rawHex)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func hexToBigInt(s string) *big.Int {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if s == "" {
		return big.NewInt(0)
	}
	n, _ := new(big.Int).SetString(s, 16)
	if n == nil {
		return big.NewInt(0)
	}
	return n
}

// bigUintBytes returns the minimal big-endian unsigned encoding (canonical RLP).
// An input of zero encodes to empty bytes per RLP rules.
func bigUintBytes(n *big.Int) []byte {
	if n == nil || n.Sign() == 0 {
		return nil
	}
	return n.Bytes()
}

// FormatCC returns a human-readable CC amount (18 decimals) from wei.
func FormatCC(wei *big.Int) string {
	if wei == nil || wei.Sign() == 0 {
		return "0 CC"
	}
	// Divide by 10^18 with 6 decimal places display.
	exp := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	intPart := new(big.Int).Quo(wei, exp)
	fracPart := new(big.Int).Mod(wei, exp)
	fracStr := fmt.Sprintf("%018d", fracPart)
	// Trim trailing zeros (up to 6 decimals shown).
	if len(fracStr) > 6 {
		fracStr = fracStr[:6]
	}
	fracStr = strings.TrimRight(fracStr, "0")
	if fracStr == "" {
		return fmt.Sprintf("%s CC", intPart.String())
	}
	return fmt.Sprintf("%s.%s CC", intPart.String(), fracStr)
}

// ParseCC parses a human-readable CC amount (like "0.5" or "1.25") into wei.
func ParseCC(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ".")
	exp := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

	intPart, ok := new(big.Int).SetString(parts[0], 10)
	if !ok {
		return nil, fmt.Errorf("invalid amount: %s", s)
	}
	result := new(big.Int).Mul(intPart, exp)

	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) > 18 {
			frac = frac[:18]
		}
		// pad right with zeros to 18 digits
		for len(frac) < 18 {
			frac += "0"
		}
		fracInt, ok := new(big.Int).SetString(frac, 10)
		if !ok {
			return nil, fmt.Errorf("invalid fractional part: %s", parts[1])
		}
		result.Add(result, fracInt)
	} else if len(parts) > 2 {
		return nil, fmt.Errorf("invalid amount: %s", s)
	}
	return result, nil
}
