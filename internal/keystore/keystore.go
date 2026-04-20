// Package keystore provides EVM-compatible key management.
//
// Keys are stored as AES-GCM-encrypted JSON files under the keystore dir.
// Each key is derived from a BIP39 mnemonic using Ethereum's BIP44 path
// (m/44'/60'/0'/0/0) and produces a standard 0x Ethereum address.
package keystore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/tyler-smith/go-bip32"
	"github.com/tyler-smith/go-bip39"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/sha3"
)

// ethDerivationPath is Ethereum's standard BIP44 path: m/44'/60'/0'/0/0
var ethDerivationPath = []uint32{
	bip32.FirstHardenedChild + 44,
	bip32.FirstHardenedChild + 60,
	bip32.FirstHardenedChild + 0,
	0,
	0,
}

// KeyInfo is the public metadata of a stored key.
type KeyInfo struct {
	Name    string `json:"name"`
	Address string `json:"address"` // EIP-55 0x... address
}

// keyFile is the on-disk encrypted record.
type keyFile struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Salt    string `json:"salt"`       // base64-free hex
	Nonce   string `json:"nonce"`      // hex
	Cipher  string `json:"ciphertext"` // hex; encrypted 32-byte private key
}

// Keystore manages encrypted EVM keys on disk.
type Keystore struct {
	dir      string
	password string
}

// NewKeystore opens (or creates) a keystore directory.
// An empty password uses a fixed dev passphrase (same convention as cccli).
func NewKeystore(dir, password string) (*Keystore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create keystore dir: %w", err)
	}
	if password == "" {
		password = "clawlink-keystore-default"
	}
	return &Keystore{dir: dir, password: password}, nil
}

// CreateKey generates a fresh mnemonic + key and saves it encrypted.
// Returns the public KeyInfo and the mnemonic (show once, store safely).
func (k *Keystore) CreateKey(name string) (*KeyInfo, string, error) {
	if err := k.assertNotExists(name); err != nil {
		return nil, "", err
	}
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return nil, "", err
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return nil, "", err
	}
	info, err := k.importMnemonic(name, mnemonic, false)
	if err != nil {
		return nil, "", err
	}
	return info, mnemonic, nil
}

// ImportMnemonic derives a key from a 12/24-word BIP39 mnemonic and saves it.
// If force is true, the checksum is not validated.
func (k *Keystore) ImportMnemonic(name, mnemonic string, force bool) (*KeyInfo, error) {
	if err := k.assertNotExists(name); err != nil {
		return nil, err
	}
	return k.importMnemonic(name, mnemonic, force)
}

// ImportPrivateKey imports a raw 32-byte hex secp256k1 private key.
func (k *Keystore) ImportPrivateKey(name, hexKey string) (*KeyInfo, error) {
	if err := k.assertNotExists(name); err != nil {
		return nil, err
	}
	hexKey = strings.TrimPrefix(strings.TrimPrefix(hexKey, "0x"), "0X")
	privBytes, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("invalid hex: %w", err)
	}
	if len(privBytes) != 32 {
		return nil, fmt.Errorf("private key must be 32 bytes, got %d", len(privBytes))
	}
	priv := secp256k1.PrivKeyFromBytes(privBytes)
	addr := evmAddressFromPubKey(priv.PubKey())
	if err := k.save(name, addr, privBytes); err != nil {
		return nil, err
	}
	return &KeyInfo{Name: name, Address: addr}, nil
}

// GetKey returns the public KeyInfo for a stored key.
func (k *Keystore) GetKey(name string) (*KeyInfo, error) {
	kf, err := k.readFile(name)
	if err != nil {
		return nil, err
	}
	return &KeyInfo{Name: kf.Name, Address: kf.Address}, nil
}

// GetPrivateKey decrypts and returns the raw private key bytes for signing.
func (k *Keystore) GetPrivateKey(name string) ([]byte, error) {
	kf, err := k.readFile(name)
	if err != nil {
		return nil, err
	}
	salt, err := hex.DecodeString(kf.Salt)
	if err != nil {
		return nil, err
	}
	nonce, err := hex.DecodeString(kf.Nonce)
	if err != nil {
		return nil, err
	}
	ct, err := hex.DecodeString(kf.Cipher)
	if err != nil {
		return nil, err
	}
	key := pbkdf2.Key([]byte(k.password), salt, 100_000, 32, sha256.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt failed (wrong password?): %w", err)
	}
	return plain, nil
}

// ListKeys returns all stored keys.
func (k *Keystore) ListKeys() ([]KeyInfo, error) {
	entries, err := os.ReadDir(k.dir)
	if err != nil {
		return nil, err
	}
	var out []KeyInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		kf, err := k.readFile(name)
		if err != nil {
			continue
		}
		out = append(out, KeyInfo{Name: kf.Name, Address: kf.Address})
	}
	return out, nil
}

// DeleteKey removes a key from the keystore.
func (k *Keystore) DeleteKey(name string) error {
	path := filepath.Join(k.dir, name+".json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("key %q not found", name)
	}
	return os.Remove(path)
}

// ─── Internal helpers ────────────────────────────────────────────────────────

func (k *Keystore) assertNotExists(name string) error {
	if name == "" {
		return fmt.Errorf("key name cannot be empty")
	}
	if _, err := os.Stat(filepath.Join(k.dir, name+".json")); err == nil {
		return fmt.Errorf("key %q already exists", name)
	}
	return nil
}

func (k *Keystore) importMnemonic(name, mnemonic string, force bool) (*KeyInfo, error) {
	mnemonic = strings.TrimSpace(mnemonic)
	if !force && !bip39.IsMnemonicValid(mnemonic) {
		return nil, fmt.Errorf("invalid BIP39 mnemonic (use --force to skip checksum)")
	}
	seed := bip39.NewSeed(mnemonic, "")
	master, err := bip32.NewMasterKey(seed)
	if err != nil {
		return nil, err
	}
	derived := master
	for _, idx := range ethDerivationPath {
		derived, err = derived.NewChildKey(idx)
		if err != nil {
			return nil, err
		}
	}
	privBytes := derived.Key
	if len(privBytes) != 32 {
		return nil, fmt.Errorf("derived key length %d != 32", len(privBytes))
	}
	priv := secp256k1.PrivKeyFromBytes(privBytes)
	addr := evmAddressFromPubKey(priv.PubKey())
	if err := k.save(name, addr, privBytes); err != nil {
		return nil, err
	}
	return &KeyInfo{Name: name, Address: addr}, nil
}

func (k *Keystore) save(name, addr string, privBytes []byte) error {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	key := pbkdf2.Key([]byte(k.password), salt, 100_000, 32, sha256.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := aead.Seal(nil, nonce, privBytes, nil)

	kf := keyFile{
		Name:    name,
		Address: addr,
		Salt:    hex.EncodeToString(salt),
		Nonce:   hex.EncodeToString(nonce),
		Cipher:  hex.EncodeToString(ct),
	}
	data, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(k.dir, name+".json"), data, 0600)
}

func (k *Keystore) readFile(name string) (*keyFile, error) {
	data, err := os.ReadFile(filepath.Join(k.dir, name+".json"))
	if err != nil {
		return nil, fmt.Errorf("key %q not found: %w", name, err)
	}
	var kf keyFile
	if err := json.Unmarshal(data, &kf); err != nil {
		return nil, err
	}
	return &kf, nil
}

// evmAddressFromPubKey derives a lowercase 0x-prefixed EVM address.
func evmAddressFromPubKey(pub *secp256k1.PublicKey) string {
	uncompressed := pub.SerializeUncompressed()
	h := sha3.NewLegacyKeccak256()
	h.Write(uncompressed[1:]) // drop 0x04 prefix
	hash := h.Sum(nil)
	return "0x" + hex.EncodeToString(hash[12:])
}

// Keccak256 is exported for SIWE message signing elsewhere.
func Keccak256(b []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(b)
	return h.Sum(nil)
}
