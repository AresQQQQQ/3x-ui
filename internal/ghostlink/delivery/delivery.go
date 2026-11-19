// Package delivery implements the public GhostLink encrypted bundle format.
package delivery

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
)

const ChunkSize = 64 << 10
const MaxCoreSize = 256 << 20
const MaxMetadataSize = 96 << 10
const ContentType = "application/vnd.ghostlink.bundle.v1"

var b64 = base64.RawURLEncoding.Strict()

type Metadata struct {
	License      []byte `json:"license"`
	ServerPublic string `json:"server_eph_pub"`
	Nonce        string `json:"base_nonce"`
	SHA256       string `json:"sha256"`
}

type Status struct {
	ID    string `json:"request_id"`
	State string `json:"status"`
	Token string `json:"poll_token,omitempty"`
}

func ParsePublic(encoded string) (*ecdh.PublicKey, error) {
	raw, err := b64.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPublicKey(raw)
}

func PublicString(key *ecdh.PublicKey) string { return b64.EncodeToString(key.Bytes()) }

// Sender prepares the session before the hub commits single-use consumption.
type Sender struct {
	stream
	metadata []byte
	used     atomic.Bool
}

func NewSender(client *ecdh.PublicKey, requestID, machineID, checksum string, license []byte) (*Sender, error) {
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	var nonce [12]byte
	rand.Read(nonce[:])
	meta := Metadata{
		License: license, ServerPublic: PublicString(private.PublicKey()),
		Nonce: b64.EncodeToString(nonce[:]), SHA256: checksum,
	}
	raw, err := json.Marshal(meta)
	if err != nil || len(raw) > MaxMetadataSize {
		return nil, errors.New("invalid bundle metadata")
	}
	s, err := newStream(private, client, nonce[:], requestID, machineID, raw)
	if err != nil {
		return nil, err
	}
	return &Sender{stream: s, metadata: raw}, nil
}

func (s *Sender) Write(w io.Writer, core io.Reader) error {
	if !s.used.CompareAndSwap(false, true) {
		return errors.New("bundle sender has already been used")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(s.metadata)))
	if _, err := w.Write(size[:]); err != nil {
		return err
	}
	if _, err := w.Write(s.metadata); err != nil {
		return err
	}
	plain := make([]byte, ChunkSize)
	ciphertext := make([]byte, 0, ChunkSize+s.aead.Overhead())
	var total int
	for index := uint64(0); ; index++ {
		n, err := io.ReadFull(core, plain)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		total += n
		if total > MaxCoreSize {
			return errors.New("core exceeds bundle size limit")
		}
		final := n == 0
		nonce, aad := s.frame(index, final)
		ciphertext = s.aead.Seal(ciphertext[:0], nonce[:], plain[:n], aad)
		binary.BigEndian.PutUint32(size[:], uint32(n))
		if _, err := w.Write(size[:]); err != nil {
			return err
		}
		if _, err := w.Write(ciphertext); err != nil {
			return err
		}
		if final {
			return nil
		}
	}
}

func Receive(w io.Writer, bundle io.Reader, key *ecdh.PrivateKey, requestID, machineID string) (Metadata, error) {
	var metadata Metadata
	var size [4]byte
	if _, err := io.ReadFull(bundle, size[:]); err != nil {
		return metadata, err
	}
	length := binary.BigEndian.Uint32(size[:])
	if length == 0 || length > MaxMetadataSize {
		return metadata, errors.New("invalid bundle metadata length")
	}
	raw := make([]byte, length)
	if _, err := io.ReadFull(bundle, raw); err != nil {
		return metadata, err
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return metadata, err
	}
	expectedHash, err := hex.DecodeString(metadata.SHA256)
	if err != nil || len(expectedHash) != sha256.Size || len(metadata.License) == 0 || len(metadata.License) > 64<<10 {
		return metadata, errors.New("invalid bundle checksum or license")
	}
	peer, err := ParsePublic(metadata.ServerPublic)
	if err != nil {
		return metadata, err
	}
	nonce, err := b64.DecodeString(metadata.Nonce)
	if err != nil || len(nonce) != 12 {
		return metadata, errors.New("invalid bundle nonce")
	}
	s, err := newStream(key, peer, nonce, requestID, machineID, raw)
	if err != nil {
		return metadata, err
	}
	digest := sha256.New()
	chunk := make([]byte, ChunkSize+s.aead.Overhead())
	var total uint32
	for index := uint64(0); ; index++ {
		if _, err := io.ReadFull(bundle, size[:]); err != nil {
			return metadata, err
		}
		n := binary.BigEndian.Uint32(size[:])
		if n > ChunkSize || total > MaxCoreSize-n {
			return metadata, errors.New("invalid encrypted chunk length")
		}
		sealed := chunk[:int(n)+s.aead.Overhead()]
		if _, err := io.ReadFull(bundle, sealed); err != nil {
			return metadata, err
		}
		final := n == 0
		nonce, aad := s.frame(index, final)
		plain, err := s.aead.Open(sealed[:0], nonce[:], sealed, aad)
		if err != nil {
			return metadata, fmt.Errorf("authenticate bundle chunk %d: %w", index, err)
		}
		if final {
			if _, err := io.ReadFull(bundle, size[:1]); err != io.EOF {
				return metadata, errors.New("extra data or invalid end of bundle")
			}
			actual := digest.Sum(nil)
			if total == 0 || !bytes.Equal(actual, expectedHash) {
				return metadata, errors.New("core SHA-256 mismatch")
			}
			return metadata, nil
		}
		total += n
		digest.Write(plain)
		if _, err := w.Write(plain); err != nil {
			return metadata, err
		}
	}
}

type stream struct {
	aead  cipher.AEAD
	nonce [12]byte
	aad   []byte
}

func newStream(private *ecdh.PrivateKey, peer *ecdh.PublicKey, nonce []byte, requestID, machineID string, header []byte) (stream, error) {
	shared, err := private.ECDH(peer)
	if err != nil {
		return stream{}, err
	}
	defer clear(shared)
	key, err := hkdf.Key(sha256.New, shared, nonce, "ghostlink-delivery-v1:"+requestID+machineID, 32)
	if err != nil {
		return stream{}, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return stream{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return stream{}, err
	}
	digest := sha256.Sum256(header)
	aad := make([]byte, len(requestID)+len(digest)+9)
	copy(aad, requestID)
	copy(aad[len(requestID):], digest[:])
	return stream{aead: aead, nonce: [12]byte(nonce), aad: aad}, nil
}

func (s stream) frame(index uint64, final bool) ([12]byte, []byte) {
	nonce := s.nonce
	binary.BigEndian.PutUint64(nonce[4:], binary.BigEndian.Uint64(nonce[4:])^index)
	offset := len(s.aad) - 9
	binary.BigEndian.PutUint64(s.aad[offset:], index)
	if final {
		s.aad[offset+8] = 1
	}
	return nonce, s.aad
}
