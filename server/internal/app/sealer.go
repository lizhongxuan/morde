package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
	"os"
)

type Sealer struct {
	aead cipher.AEAD
}

func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("data key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create data cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create data cipher: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

func LoadSealer(path string) (*Sealer, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read data key: %w", err)
	}
	return NewSealer(key)
}

func (s *Sealer) Seal(plain []byte) ([]byte, error) {
	if s == nil || s.aead == nil {
		return nil, fmt.Errorf("data sealer is not initialized")
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate data nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plain, nil), nil
}

func (s *Sealer) Open(ciphertext []byte) ([]byte, error) {
	if s == nil || s.aead == nil || len(ciphertext) < s.aead.NonceSize()+s.aead.Overhead() {
		return nil, fmt.Errorf("encrypted data is invalid")
	}
	nonce, body := ciphertext[:s.aead.NonceSize()], ciphertext[s.aead.NonceSize():]
	plain, err := s.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt data: %w", err)
	}
	return plain, nil
}
